package tracingcontract

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiali/kiali/tracing/jaeger/apiv3"
)

// unknownTraceID is a well-formed 32-character hex ID no store holds.
const unknownTraceID = "ffffffffffffffffffffffffffffffff"

// malformedTraceID is not hex, so it is refused before the store is consulted.
const malformedTraceID = "not-hex-at-all"

// absentService narrows a search to a service.name nothing exported, which is
// the only way to ask a store for an empty search answer without also asking it
// to read a parameter it may not understand.
const absentService = "no-such-service-in-any-store"

// tracePath is api_v3's HTTP trace detail route.
const tracePath = "/api/v3/traces/"

// getTrace asks for one trace over api_v3 gRPC and reports the distinct trace
// IDs, the number of stream messages and the RPC error.
//
// GetTrace is server-streaming like FindTraces, so an absent trace has two
// possible shapes and the message count is what tells them apart: an error, or
// OK with no message at all.
func getTrace(t *testing.T, client apiv3.QueryServiceClient, traceID string) (ids []string, messages int, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := client.GetTrace(ctx, &apiv3.GetTraceRequest{TraceId: traceID})
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	for {
		message, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			sort.Strings(ids)
			return ids, messages, nil
		}
		if recvErr != nil {
			return ids, messages, recvErr
		}
		messages++
		for _, rs := range message.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, span := range ss.GetSpans() {
					id := hex.EncodeToString(span.GetTraceId())
					if id != "" && !seen[id] {
						seen[id] = true
						ids = append(ids, id)
					}
				}
			}
		}
	}
}

// TestNotFoundOverHTTP pins how api_v3's HTTP gateway says it has nothing. Both
// of Kiali's sentinels come from the same status and the same body, so the
// envelope is the whole of what distinguishes "the store holds nothing" from a
// wrong URL.
func TestNotFoundOverHTTP(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			t.Run("an unknown trace is a 404 carrying the envelope", func(t *testing.T) {
				got := get(t, store.addr, tracePath+unknownTraceID, searchParams())
				require.Equal(t, http.StatusNotFound, got.status, "%s", got)
				assertEnvelope(t, got, http.StatusNotFound, "No traces found")
			})

			t.Run("an unmatched search is the same 404 and the same envelope", func(t *testing.T) {
				// The detail and the search are indistinguishable from the
				// response alone, which is why Kiali's classifier takes the
				// sentinel from the caller rather than deriving it here.
				params := searchParams()
				params.Set("query.service_name", absentService)
				params.Set("query.num_traces", seedDepth)
				got := get(t, store.addr, searchPath, params)
				require.Equal(t, http.StatusNotFound, got.status,
					"an api_v3 search with no match is a 404, not a 200 holding an empty list: %s", got)
				assertEnvelope(t, got, http.StatusNotFound, "No traces found")
			})

			t.Run("a malformed trace id is a 400 carrying the envelope", func(t *testing.T) {
				// The status is what matters and the message is not asserted:
				// 2.17.0 reports strconv.ParseUint where the newer two report
				// an invalid length, so a classifier keyed on the text would
				// read one of them as a different fault.
				got := get(t, store.addr, tracePath+malformedTraceID, searchParams())
				require.Equal(t, http.StatusBadRequest, got.status,
					"a trace id that is not hex must be refused, not answered as absent: %s", got)
				assertEnvelope(t, got, http.StatusBadRequest, "malformed parameter trace_id")
			})
		})
	}
}

// TestNotFoundOverGRPC pins the two shapes an absent trace takes over api_v3
// gRPC, per version.
//
// The divergence is load-bearing rather than cosmetic: on 2.17.0 and 2.20.0 an
// unknown trace never reaches an error classifier at all, because the RPC
// succeeds and the stream is empty. So the answer Kiali gives a user asking for
// a trace that aged out has to come from the empty-stream path, and an
// implementation that only maps codes.NotFound is correct on 2.21.0 alone —
// which is every version except the one Istio ships.
func TestNotFoundOverGRPC(t *testing.T) {
	for _, store := range requireJaegerGRPC(t) {
		t.Run(store.version, func(t *testing.T) {
			client := queryClient(t, store)

			t.Run("an unknown trace", func(t *testing.T) {
				ids, messages, err := getTrace(t, client, unknownTraceID)
				assert.Empty(t, ids, "no store holds %s", unknownTraceID)

				if store.caps.grpcUnknownTraceNotFound {
					require.Error(t, err,
						"Jaeger %s reports an unknown trace as codes.NotFound, and it answered %d messages with no error instead",
						store.version, messages)
					assert.Equal(t, codes.NotFound, status.Code(err),
						"the one status a classifier may read as absent is NotFound: %v", err)
					return
				}

				require.NoError(t, err,
					"Jaeger %s answers an unknown trace with OK and an empty stream, so nothing reaches the error classifier",
					store.version)
				assert.Zero(t, messages,
					"and with no message at all, which is the only signal that the trace is absent")
			})

			t.Run("a malformed trace id is refused on every version", func(t *testing.T) {
				// The one shape the three versions agree on, and the reason a
				// refusal cannot be treated as an absence: this is a client
				// fault and must reach the user as one.
				_, _, err := getTrace(t, client, malformedTraceID)
				require.Error(t, err, "a trace id that is not hex must be refused")
				assert.NotEqual(t, codes.NotFound, status.Code(err),
					"a malformed id must not arrive as NotFound, or Kiali would report the trace as simply absent: %v", err)
			})

			t.Run("an unmatched search is OK with no message", func(t *testing.T) {
				query := seededQuery()
				query.ServiceName = absentService
				ids, messages, err := findTraces(t, client, query)
				require.NoError(t, err,
					"an unmatched api_v3 gRPC search is not an error on any version, so an empty search never reaches the error classifier")
				assert.Empty(t, ids)
				assert.Zero(t, messages)
			})
		})
	}
}

// assertEnvelope checks the api_v3 error envelope, which is what Kiali's
// classifier tests for before reading a 404 as an empty store. A mistyped path
// answers 404 with plain text instead, and reading that as a result turns a
// wrong URL into a backend holding no traces.
func assertEnvelope(t *testing.T, got answer, code int, message string) {
	t.Helper()
	var envelope struct {
		Error struct {
			HTTPCode int    `json:"httpCode"`
			Message  string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(got.body, &envelope), "the body is not the api_v3 error envelope: %s", got)
	assert.Equal(t, code, envelope.Error.HTTPCode,
		"the envelope repeats the status, and a classifier may rely on it: %s", got)
	assert.Contains(t, envelope.Error.Message, message, "%s", got)
}
