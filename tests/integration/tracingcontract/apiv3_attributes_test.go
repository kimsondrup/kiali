package tracingcontract

import (
	"bytes"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/models"
)

// The two attribute filters Kiali sends. The cluster id goes on every trace query, so a span
// without it is invisible to Kiali even when the service name matches; the error flag is what
// GetErrorTraces narrows by, and Envoy emits it as a span attribute on a failing request.
var (
	absentCluster  = map[string]string{models.IstioClusterTag: "nope-no-such"}
	errorAttribute = map[string]string{"error": "true"}
)

// clusterAttribute is the cluster filter carrying the value the seeded spans carry. It is
// a function because the value is read from the environment in TestMain, after package
// variables are initialised.
func clusterAttribute() map[string]string {
	return map[string]string{models.IstioClusterTag: clusterID}
}

// TestAPIV3AttributeFilterPerTransport pins the attribute filter of both api_v3 transports of one
// Jaeger in one subtest, because the difference between the two is the reason Kiali's HTTP
// transport re-applies the tags to what it converted and its gRPC transport does not.
//
// Measured: Jaeger 2.17.0 — the version Istio 1.31's
// add-on ships — ignores query.attributes over its HTTP gateway under every spelling, answering
// 200 with the unfiltered body, while the same store honours the same filter over gRPC. 2.20.0
// and 2.21.0 honour it on both.
//
// Every row asserts the narrowing and not the status, because an ignored filter answers 200 and
// reads exactly like a working one.
func TestAPIV3AttributeFilterPerTransport(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			baseline := search(t, store, url.Values{"query.num_traces": {seedDepth}})
			require.Equal(t, http.StatusOK, baseline.status, "%s", baseline)
			baselineIDs := baseline.traceIDs(t)
			require.NotEmpty(t, baselineIDs, "the seeded window is empty, so no row below means anything")

			// Both halves need a window holding failures, because a filter that narrows nothing
			// is indistinguishable from one that is ignored.
			t.Run("http", func(t *testing.T) {
				cluster := search(t, store, url.Values{
					"query.num_traces": {seedDepth},
					"query.attributes": {jsonMap(t, clusterAttribute())},
				})
				require.Equal(t, http.StatusOK, cluster.status,
					"the cluster id Kiali appends to every query must not break the search: %s", cluster)
				assert.Equal(t, baselineIDs, cluster.traceIDs(t),
					"every Envoy span carries the cluster id, so this filter must keep all %d seeded traces", len(baselineIDs))

				absent := search(t, store, url.Values{
					"query.num_traces": {seedDepth},
					"query.attributes": {jsonMap(t, absentCluster)},
				})
				failing := search(t, store, url.Values{
					"query.num_traces": {seedDepth},
					"query.attributes": {jsonMap(t, errorAttribute)},
				})

				if store.caps.httpAttributeFilter {
					assert.Equal(t, http.StatusNotFound, absent.status,
						"a value nothing carries must answer nothing here: %s", absent)
					assert.Contains(t, string(absent.body), "No traces found",
						"the empty answer must carry the api_v3 envelope, which is how it is told apart from a mistyped path: %s", absent)

					require.Equal(t, http.StatusOK, failing.status, "%s", failing)
					failingIDs := failing.traceIDs(t)
					assert.NotEmpty(t, failingIDs,
						"no trace in the window carries error=true: drive failing traffic, or this row proves nothing")
					assert.Less(t, len(failingIDs), len(baselineIDs),
						"error=true must narrow the %d seeded traces: %s", len(baselineIDs), failing)
					return
				}

				// The filter is inert: not refused, not partially applied, simply unread. Byte
				// identity to the unfiltered body is the assertion, because a count alone would
				// also hold for a filter that happens to exclude nothing.
				assert.Equal(t, http.StatusOK, absent.status,
					"an ignored filter answers the whole window, which is why Kiali has to filter after converting: %s", absent)
				assert.True(t, bytes.Equal(baseline.body, absent.body),
					"query.attributes must be inert here, answering the unfiltered body byte for byte: %s", absent)
				assert.True(t, bytes.Equal(baseline.body, failing.body),
					"query.attributes must be inert here for the error flag too: %s", failing)
			})

			t.Run("the grpc-gateway bracket form is ignored everywhere", func(t *testing.T) {
				// query.attributes[key]=value is the spelling grpc-gateway documents for a map,
				// and api_v3 accepts and discards it on every version. It answers 200 with
				// everything, so adopting it would read in review as a working filter and in
				// production as no filter at all.
				bracket := search(t, store, url.Values{
					"query.num_traces": {seedDepth},
					"query.attributes[" + models.IstioClusterTag + "]": {"nope-no-such"},
				})
				assert.Equal(t, http.StatusOK, bracket.status, "%s", bracket)
				assert.True(t, bytes.Equal(baseline.body, bracket.body),
					"the bracket form must answer the unfiltered body byte for byte: %s", bracket)
			})

			t.Run("grpc", func(t *testing.T) {
				client, covered := grpcPeer(t, store.version)
				if !covered {
					t.Skipf("Jaeger %s is configured for HTTP only, so the pair this test exists to compare is not available", store.version)
				}

				cluster, _, err := findTraces(t, client, searchWithAttributes(clusterAttribute()))
				require.NoError(t, err)
				assert.Equal(t, len(baselineIDs), len(cluster),
					"every Envoy span carries the cluster id, so this filter must keep all %d seeded traces", len(baselineIDs))

				absent, messages, err := findTraces(t, client, searchWithAttributes(absentCluster))
				require.NoError(t, err,
					"an unmatched api_v3 gRPC search is OK with no message, not an error, so nothing reaches the error classifier")
				assert.Empty(t, absent, "a value nothing carries must answer no trace: %d messages", messages)
				assert.Zero(t, messages, "and no message at all, which is what an empty stream means")

				failing, _, err := findTraces(t, client, searchWithAttributes(errorAttribute))
				require.NoError(t, err)
				require.NotEmpty(t, failing,
					"no trace in the window carries error=true: drive failing traffic, or this row proves nothing")
				assert.Less(t, len(failing), len(baselineIDs),
					"error=true must narrow the %d seeded traces over gRPC", len(baselineIDs))

				if !store.caps.httpAttributeFilter {
					// The pair, asserted as a pair, and a property of one store: the same
					// spans, the same filter, narrowed over one transport and not the other.
					assert.Less(t, len(absent), len(baselineIDs),
						"Jaeger %s ignores this filter over HTTP and honours it over gRPC", store.version)
				}
			})
		})
	}
}
