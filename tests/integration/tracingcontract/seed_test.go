package tracingcontract

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSeedPreconditionHTTP and TestSeedPreconditionGRPC fail, and deliberately
// never skip, when a store is configured and the named service has no spans in
// the window.
//
// Every other test in this suite reads a 200 as proof that a parameter was
// understood. A 200 carrying an empty answer would satisfy most of them, and
// that is exactly what a wrong KIALI_CONTRACT_SERVICE produces: each Jaeger
// answers 404 "No traces found" and Tempo answers 200 {"traces":[]}, neither of
// which says the name was nonsense. So the window is proven non-empty once,
// here, and the rest of the suite may then read an empty answer as a filter
// that worked.
// The two transports are separate tests rather than halves of one, so a run that configures only
// one of them says so for the other instead of quietly having nothing to seed.
func TestSeedPreconditionHTTP(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			got := search(t, store, url.Values{"query.num_traces": {seedDepth}})
			require.Equal(t, http.StatusOK, got.status,
				"Jaeger %s has no spans for service.name %q in the window %s..%s (%s): drive traffic through the lab, or correct %s",
				store.version, service, formatTime(windowMin), formatTime(windowMax), got, envService)
			require.NotEmpty(t, got.traceIDs(t),
				"Jaeger %s answered 200 with no trace at all for service.name %q: every 200 in this suite would then be vacuous",
				store.version, service)
		})
	}
}

func TestSeedPreconditionGRPC(t *testing.T) {
	for _, store := range requireJaegerGRPC(t) {
		t.Run(store.version, func(t *testing.T) {
			ids, messages, err := findTraces(t, queryClient(t, store), seededQuery())
			require.NoError(t, err, "Jaeger %s refused the api_v3 gRPC search after %d messages", store.version, messages)
			require.NotEmpty(t, ids,
				"Jaeger %s answered the api_v3 gRPC search with no trace at all for service.name %q", store.version, service)
		})
	}
}
