package tracingcontract

import (
	"testing"

	"github.com/kiali/kiali/tracing/jaeger/apiv3"
)

// grpcPeer returns the api_v3 gRPC client of the same Jaeger a HTTP subtest is running against,
// so one subtest can assert both transports of one store. The two halves skip independently:
// a dead port-forward on one must not weaken the other, and must not be mistaken for a backend
// that stopped answering.
func grpcPeer(t *testing.T, version string) (apiv3.QueryServiceClient, bool) {
	t.Helper()
	if len(jaegerGRPC) == 0 {
		t.Logf("%s is unset: the gRPC half of Jaeger %s is not covered by this run", envJaegerGRPC, version)
		return nil, false
	}
	for _, store := range jaegerGRPC {
		if store.version == version {
			return queryClient(t, store), true
		}
	}
	t.Logf("%s names no %s: the gRPC half of that store is not covered by this run", envJaegerGRPC, version)
	return nil, false
}

// searchWithAttributes is the seeded search narrowed by span attributes, which is the only
// narrowing api_v3 offers beyond the service name and the window.
func searchWithAttributes(attributes map[string]string) *apiv3.TraceQueryParameters {
	query := seededQuery()
	query.Attributes = attributes
	return query
}
