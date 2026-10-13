package tracingcontract

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// zeroWindow is how a zero time.Time renders on each transport: the year 1 over
// HTTP, and a timestamp of the same instant over gRPC. It is the window a caller
// that forgot to set one produces, so it is the shape the guard in
// tracing/jaeger/apiv3req.go refuses rather than a shape anyone types.
const zeroWindow = "0001-01-01T00:00:00Z"

// TestWindowOverHTTP pins what api_v3's HTTP gateway does with a window it
// cannot search, which on the Jaeger Istio ships is to answer something rather
// than to refuse.
//
// Both answers are worse than a failure. An inverted window is a 404 carrying
// the same envelope an empty store answers with, so a classifier reports "no
// traces" for a malformed query; a window of zero width is a 200 carrying every
// trace of the service, so the window the user asked about is silently replaced
// by all of time. Neither can be told from a correct answer downstream, which
// is why the query is refused where it is built.
func TestWindowOverHTTP(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			baseline := search(t, store, url.Values{"query.num_traces": {seedDepth}})
			require.Equal(t, http.StatusOK, baseline.status, "%s", baseline)
			inWindow := baseline.traceIDs(t)
			require.NotEmpty(t, inWindow, "the seeded window holds no trace, so no row below means anything")

			t.Run("an inverted window", func(t *testing.T) {
				params := searchParams()
				params.Set("query.start_time_min", formatTime(windowMax))
				params.Set("query.start_time_max", formatTime(windowMin))
				params.Set("query.num_traces", seedDepth)
				got := get(t, store.addr, searchPath, params)

				if store.caps.windowValidatedHTTP {
					require.Equal(t, http.StatusBadRequest, got.status,
						"Jaeger %s refuses a window whose start is not before its end: %s", store.version, got)
					assertEnvelope(t, got, http.StatusBadRequest, "must be before")
					return
				}

				require.Equal(t, http.StatusNotFound, got.status,
					"Jaeger %s does not validate the window and answers an inverted one as a search that matched nothing: %s",
					store.version, got)
				// The sharp edge, and the reason the guard is in the request
				// builder rather than in the error classifier: this is the one
				// body Kiali reads as an empty result, so the user is told the
				// store holds no traces for a query that was never searchable.
				assertEnvelope(t, got, http.StatusNotFound, "No traces found")
			})

			t.Run("a window of zero width", func(t *testing.T) {
				params := searchParams()
				params.Set("query.start_time_min", zeroWindow)
				params.Set("query.start_time_max", zeroWindow)
				params.Set("query.num_traces", seedDepth)
				got := get(t, store.addr, searchPath, params)

				if store.caps.windowValidatedHTTP {
					require.Equal(t, http.StatusBadRequest, got.status,
						"Jaeger %s refuses a window of zero width as it refuses an inverted one: %s", store.version, got)
					assertEnvelope(t, got, http.StatusBadRequest, "must be before")
					return
				}

				require.Equal(t, http.StatusOK, got.status,
					"Jaeger %s answers a window of zero width rather than refusing it: %s", store.version, got)
				// Worse than the 404 above, because it is a success: the
				// window is dropped and every trace the store holds for the
				// service is reported as the answer to a question about one
				// instant.
				answered := got.traceIDs(t)
				assert.Subset(t, answered, inWindow,
					"the window was ignored, so the answer holds the %d traces of the real window as well", len(inWindow))
				assert.Greater(t, len(answered), len(inWindow),
					"and more besides: %d traces came back for a window one instant wide, against %d in the hour that was asked for",
					len(answered), len(inWindow))
			})

			t.Run("no service name at all", func(t *testing.T) {
				// The third refusal, and the only one every version answers
				// alike. There is no error to classify here at all: the store
				// reports every trace it holds, from every service, which on a
				// shared Jaeger includes Jaeger's own.
				params := url.Values{
					"query.start_time_min": {formatTime(windowMin)},
					"query.start_time_max": {formatTime(windowMax)},
					"query.num_traces":     {seedDepth},
				}
				got := get(t, store.addr, searchPath, params)
				require.Equal(t, http.StatusOK, got.status,
					"a search carrying no service name is answered, not refused: %s", got)

				answered := got.traceIDs(t)
				assert.Subset(t, answered, inWindow,
					"it answers the seeded service among the rest")
				assert.Greater(t, len(answered), len(inWindow),
					"%d traces came back against the %d of the seeded service, so without a service name the trace list of one workload is the whole store",
					len(answered), len(inWindow))
			})
		})
	}
}

// TestWindowOverGRPC pins the same three queries on the other transport, where
// two of the three answers differ.
//
// The divergence is the point. One query cannot be refused in the rendering of
// one transport, because a window that is merely empty over gRPC is a refusal
// over HTTP and the other way round, so the only place the two can be made to
// agree is where the query is built.
func TestWindowOverGRPC(t *testing.T) {
	for _, store := range requireJaegerGRPC(t) {
		t.Run(store.version, func(t *testing.T) {
			client := queryClient(t, store)
			inWindow, _, err := findTraces(t, client, seededQuery())
			require.NoError(t, err)
			require.NotEmpty(t, inWindow, "the seeded window holds no trace, so no row below means anything")

			t.Run("an inverted window is OK with no message", func(t *testing.T) {
				query := seededQuery()
				query.StartTimeMin, query.StartTimeMax = timestamppb.New(windowMax), timestamppb.New(windowMin)
				ids, messages, err := findTraces(t, client, query)

				// Where the HTTP gateway at least answers 400 on the newer
				// versions, every version answers this one with success and
				// nothing in it, so no status code anywhere in Kiali can tell
				// a malformed window from a store with no traces.
				require.NoError(t, err,
					"Jaeger %s does not refuse an inverted window over gRPC, and it answered %d traces in %d messages",
					store.version, len(ids), messages)
				assert.Empty(t, ids)
				assert.Zero(t, messages,
					"which is the same answer an unmatched search gives, and the reason the window is checked before the request is built")
			})

			t.Run("a window of zero width is refused", func(t *testing.T) {
				query := seededQuery()
				query.StartTimeMin, query.StartTimeMax = timestamppb.New(time.Time{}), timestamppb.New(time.Time{})
				_, _, err := findTraces(t, client, query)

				// The opposite of the HTTP answer on 2.17.0, where the same
				// window is a 200 holding the whole store. One query, two
				// transports, two unrelated wrong answers.
				require.Error(t, err,
					"Jaeger %s refuses a window of zero width over gRPC, where its HTTP gateway may answer it", store.version)
				assert.Equal(t, store.caps.grpcZeroWindowCode, status.Code(err),
					"and reports it with the status this version uses for a request it cannot read: %v", err)
				assert.Contains(t, status.Convert(err).Message(), "start time min and max are required",
					"naming the window, which is the only thing that says the fault is the caller's: %v", err)
			})

			t.Run("no service name answers every trace in the store", func(t *testing.T) {
				query := seededQuery()
				query.ServiceName = ""
				ids, _, err := findTraces(t, client, query)
				require.NoError(t, err,
					"a search carrying no service name is answered on this transport too, not refused")
				assert.Subset(t, ids, inWindow)
				assert.Greater(t, len(ids), len(inWindow),
					"%d traces came back against the %d of the seeded service, so the service name is the whole of what narrows a trace list to one workload",
					len(ids), len(inWindow))
			})
		})
	}
}
