package tracingcontract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/tempo"
)

// tempoSearchPath is Tempo's own search route, used here only to find an ID the
// store holds.
const tempoSearchPath = "/api/search"

// fullWidthHex matches a trace ID at the width the browser compares them at.
var fullWidthHex = regexp.MustCompile(`^[0-9a-f]{32}$`)

// tempoLimit bounds the Tempo searches here. It is wide enough that the window
// is likely to contain an ID Tempo strips a leading zero from, which is about
// one in sixteen and is what the padding row needs in order to mean anything.
const tempoLimit = 100

// TestTempoIsNotAJaeger is the negative control for the api_v3 HTTP client.
//
// Pointing it at a Tempo is a configuration mistake with no status code of its
// own: Tempo answers every api_v3 route 404, as a Jaeger answers a search that
// matched nothing. The difference is the body, so the envelope test
// in Kiali's classifier is what decides whether the user is told the URL is
// wrong or told the store is empty.
func TestTempoIsNotAJaeger(t *testing.T) {
	addr := requireTempoHTTP(t)

	for _, path := range []string{
		searchPath,
		apiV3ServicesPath,
		tracePath + unknownTraceID,
	} {
		t.Run(path, func(t *testing.T) {
			got := get(t, addr, path, nil)
			require.Equal(t, http.StatusNotFound, got.status,
				"Tempo serves none of Jaeger's routes: %s", got)
			assert.Equal(t, "404 page not found", strings.TrimSpace(string(got.body)),
				"and says so in plain text: %s", got)

			// The test Kiali's classifier runs, repeated here because this is
			// the body it has to run it against. A real api_v3 404 carries
			// {"error":{"httpCode":404,"message":"No traces found"}} and is
			// read as an empty result; anything else must be reported, or a
			// wrong internal_url becomes a trace store holding nothing.
			var envelope struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			decoded := json.Unmarshal(got.body, &envelope)
			assert.True(t, decoded != nil || envelope.Error.Message == "",
				"this 404 must not satisfy the api_v3 envelope test: %s", got)
		})
	}
}

// TestTempoThroughTheShippedClient is the regression baseline for the provider
// that already reads its traces through the shared OTLP path.
//
// Tempo is in this suite because the decode and conversion it uses are now
// shared with api_v3, so a change made for Jaeger lands here first. The two
// transports are the two Kiali actually uses: search over the streaming gRPC
// querier, trace detail over HTTP.
func TestTempoThroughTheShippedClient(t *testing.T) {
	// The shipped clients read the query timeout and the trace cache setting
	// from the global config. Unset, that timeout is zero and every call
	// deadlines immediately.
	config.Set(config.NewConfig())

	t.Run("search over the streaming querier", func(t *testing.T) {
		client, err := tempo.NewgRPCClient(requireTempoConn(t))
		require.NoError(t, err)

		response, err := client.FindTraces(context.Background(), service, tempoQuery())
		require.NoError(t, err, "Tempo refused the search the shipped client builds")
		require.NotNil(t, response)
		assert.Equal(t, service, response.TracingServiceName)
		require.NotEmpty(t, response.Data,
			"Tempo accepted the search and answered no trace for %q", service)
		assert.LessOrEqual(t, len(response.Data), tempoLimit,
			"the shipped client bounds the distinct traces at the limit asked for, whatever the stream carried")

		var reported []string
		for _, trace := range response.Data {
			reported = append(reported, string(trace.TraceID))
			assert.NotEmpty(t, trace.Spans, "a trace with no span is a converted answer that lost its span set")
		}

		// Tempo spells a search answer's trace ID with its leading zeros
		// stripped, about one ID in sixteen, while the trace itself carries
		// all 16 bytes. The browser compares the two, so a stripped ID makes
		// one trace into two.
		//
		// The comparison is against Tempo's own answer rather than against the
		// width alone: a window that happens to hold no stripped ID cannot
		// exercise the padding, and that is a lab fault the row has to name
		// rather than pass over.
		raw := tempoSearchIDs(t, requireTempoHTTP(t))
		padded := map[string]bool{}
		stripped := 0
		for _, id := range raw {
			if len(id) < 32 {
				stripped++
			}
			padded[strings.Repeat("0", 32-len(id))+id] = true
		}
		require.NotZero(t, stripped,
			"Tempo reported no stripped trace ID among %d, so this window cannot exercise the padding at all: drive more traffic through the lab", len(raw))

		for _, id := range reported {
			assert.Regexp(t, fullWidthHex, id,
				"the trace detail reports this ID at full width, and the browser compares the two: a stripped ID makes one trace into two, so the scatter plot draws the selected trace twice and never marks it as selected (Tempo spelled %d of %d short)",
				stripped, len(raw))
			assert.True(t, padded[id],
				"trace %s is not one Tempo answered, padded: the search answer was rewritten rather than widened", id)
		}
	})

	t.Run("trace detail over HTTP", func(t *testing.T) {
		addr := requireTempoHTTP(t)
		client, err := tempo.NewOtelClient(context.Background())
		require.NoError(t, err)

		traceID := tempoSearchIDs(t, addr)[0]
		baseURL := &url.URL{Scheme: "http", Host: addr}

		single, err := client.GetTraceDetailHTTP(context.Background(), http.Client{}, baseURL, traceID)
		require.NoError(t, err, "Tempo refused the trace detail request for %s", traceID)
		require.NotNil(t, single, "Tempo answered no trace for an ID its own search reported")
		assert.Regexp(t, fullWidthHex, string(single.Data.TraceID),
			"the trace detail reports the ID at full width, which is the form the search answer is padded to match")
		require.NotEmpty(t, single.Data.Spans,
			"the detail is read as OTLP, so an empty span list means the envelope or the ID encoding was misread rather than that the trace is empty")
		for _, span := range single.Data.Spans {
			assert.NotEmpty(t, span.SpanID)
			assert.Equal(t, single.Data.TraceID, span.TraceID,
				"every span carries the trace's own ID, at the same width")
			assert.Contains(t, single.Data.Processes, span.ProcessID,
				"and names a process the trace declares, which is what the span detail panel reads")
		}
	})
}

// tempoQuery is the search Kiali builds for a trace list, over the suite's
// window and carrying the cluster tag it appends to every query.
func tempoQuery() models.TracingQuery {
	return models.TracingQuery{
		Start: windowMin,
		End:   windowMax,
		Tags:  map[string]string{models.IstioClusterTag: clusterID},
		Limit: tempoLimit,
	}
}

// requireTempoHTTP returns the configured Tempo, or skips with the reason.
func requireTempoHTTP(t *testing.T) string {
	t.Helper()
	if tempoHTTP == "" {
		t.Skipf("%s is unset: no live Tempo to pin the api_v3 negative control against", envTempoHTTP)
	}
	return tempoHTTP
}

// requireTempoConn returns the Tempo connection, or skips this subtest alone.
func requireTempoConn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	if tempoGRPC == "" {
		t.Skipf("%s is unset: no live Tempo to run the shipped streaming search against", envTempoGRPC)
	}
	if tempoConn == nil {
		t.Skipf("Tempo gRPC at %s could not be dialled at startup", tempoGRPC)
	}
	return tempoConn
}

// tempoSearchIDs reports the trace IDs Tempo answers its own search with, as
// Tempo spells them: this is the unpadded form the shipped client has to
// normalise.
func tempoSearchIDs(t *testing.T, addr string) []string {
	t.Helper()
	params := url.Values{}
	params.Set("q", `{resource.service.name="`+service+`"}`)
	// Tempo's window is in unix seconds, where Jaeger's is RFC 3339.
	params.Set("start", strconv.FormatInt(windowMin.Unix(), 10))
	params.Set("end", strconv.FormatInt(windowMax.Unix(), 10))
	params.Set("limit", strconv.Itoa(tempoLimit))

	got := get(t, addr, tempoSearchPath, params)
	require.Equal(t, http.StatusOK, got.status, "%s", got)

	var response struct {
		Traces []struct {
			TraceID string `json:"traceID"`
		} `json:"traces"`
	}
	require.NoError(t, json.Unmarshal(got.body, &response), "%s", got)
	require.NotEmpty(t, response.Traces,
		"Tempo holds no trace for service.name %q in the window: drive traffic through the lab, or correct %s", service, envService)

	var ids []string
	for _, trace := range response.Traces {
		ids = append(ids, trace.TraceID)
	}
	return ids
}
