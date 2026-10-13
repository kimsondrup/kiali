package tracingcontract

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAPIV3GRPCSearchDepth pins the search depth api_v3 gRPC requires, which is
// the mirror image of the HTTP picture: over HTTP 2.17.0 is the lone version
// that refuses a search carrying no depth, while over gRPC it is 2.21.0 that is
// alone in accepting one.
//
// So a depth cannot be defaulted in the HTTP rendering alone. Both renderings
// are built from one query, and two of the three backends refuse the gRPC one
// without it.
func TestAPIV3GRPCSearchDepth(t *testing.T) {
	for _, store := range requireJaegerGRPC(t) {
		t.Run(store.version, func(t *testing.T) {
			client := queryClient(t, store)

			t.Run("no depth at all", func(t *testing.T) {
				query := seededQuery()
				query.SearchDepth = 0
				ids, messages, err := findTraces(t, client, query)

				if store.caps.grpcDepthOptional {
					require.NoError(t, err,
						"Jaeger %s accepts a search with no depth, and it refused one", store.version)
					assert.NotEmpty(t, ids,
						"an accepted depthless search answers the whole window, not nothing: %d messages", messages)
					return
				}

				require.Error(t, err,
					"Jaeger %s requires a positive search depth over gRPC, and it answered %d traces in %d messages instead",
					store.version, len(ids), messages)
				assert.Equal(t, codes.Unknown, status.Code(err),
					"the refusal arrives as codes.Unknown and not as an argument fault, so it is not distinguishable from a backend failure: %v", err)
				assert.Contains(t, status.Convert(err).Message(), "search depth must be greater than 0",
					"the refusal must name the depth, which is the only thing that says a defaulted depth is the fix: %v", err)
			})

			t.Run("a positive depth bounds the answer on every version", func(t *testing.T) {
				// The reason the default is a positive number and not merely a
				// present one: the depth is the only bound api_v3 offers on a
				// search, so Kiali's own limit has to arrive as this field or
				// the whole window comes back.
				full := seededQuery()
				fullIDs, _, err := findTraces(t, client, full)
				require.NoError(t, err)
				require.Greater(t, len(fullIDs), 1,
					"the seeded window holds %d traces, so a depth of 1 cannot be shown to bound anything", len(fullIDs))

				one := seededQuery()
				one.SearchDepth = 1
				ids, _, err := findTraces(t, client, one)
				require.NoError(t, err, "a depth of 1 is accepted on every version")
				assert.Len(t, ids, 1,
					"a depth of 1 must answer one trace of the %d in the window", len(fullIDs))
			})

			t.Run("the depth ceiling is the HTTP gateway's, not the service's", func(t *testing.T) {
				// Two independent bounds share one field, and only one of them
				// is a property of the version: the gateway validates the
				// parameter, while the store refuses a depth above its own
				// capacity. Kiali's clamp to models.MaxTracingLimit answers the
				// first and cannot answer the second.
				if store.caps.depthHardCeiling == 0 {
					t.Skipf("Jaeger %s validates no depth ceiling of its own, so there is no gateway refusal here to contrast with", store.version)
				}
				peer, covered := httpPeer(t, store.version)
				if !covered {
					t.Skipf("Jaeger %s is configured for gRPC only, so the pair this row compares is not available", store.version)
				}

				above := store.caps.depthHardCeiling + 1
				refused := search(t, peer, url.Values{"query.num_traces": {strconv.Itoa(above)}})
				require.Equal(t, http.StatusBadRequest, refused.status,
					"a depth above %d must be refused by the gateway: %s", store.caps.depthHardCeiling, refused)

				query := seededQuery()
				query.SearchDepth = int32(above)
				ids, _, err := findTraces(t, client, query)
				require.NoError(t, err,
					"the same depth the gateway refuses is accepted by the service, so the ceiling is the gateway's alone")
				assert.NotEmpty(t, ids)
			})
		})
	}
}

// httpPeer returns the api_v3 HTTP store of the same Jaeger a gRPC subtest is
// running against, so one subtest can contrast the two transports of one store.
// The halves skip independently, as in grpcPeer.
func httpPeer(t *testing.T, version string) (jaegerStore, bool) {
	t.Helper()
	for _, store := range jaegerHTTP {
		if store.version == version {
			return store, true
		}
	}
	t.Logf("%s names no %s: the HTTP half of that store is not covered by this run", envJaegerHTTP, version)
	return jaegerStore{}, false
}
