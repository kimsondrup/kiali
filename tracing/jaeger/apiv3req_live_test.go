package jaeger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
)

// The environment tests/integration/tracingcontract reads, read here too. That suite pins what a
// Jaeger accepts; these two tests pin that what Kiali renders is what a Jaeger accepts, which is
// the half a hand-built query cannot cover because it drifts from the renderer the moment one of
// the two is edited.
//
// This file lives in tracing/jaeger, inside `make test`, so it is compiled and vetted on every
// PR and skips there with the reason below.
const (
	liveEnvClusterID  = "KIALI_CONTRACT_CLUSTER_ID"
	liveEnvJaegerHTTP = "KIALI_CONTRACT_JAEGER_HTTP"
	liveEnvJaegerGRPC = "KIALI_CONTRACT_JAEGER_GRPC"
	liveEnvService    = "KIALI_CONTRACT_SERVICE"
)

// liveDefaultClusterID is the istio.cluster_id istiod stamps when the mesh names no cluster,
// which is what the tracing lab runs. A value no span carries empties the answer on every
// Jaeger that honours the filter, so it has to be the one the seeded spans carry.
const liveDefaultClusterID = "Kubernetes"

// liveStore is one configured Jaeger: the version label it was given and its address.
type liveStore struct {
	addr    string
	version string
}

// liveStores parses the version=address pairs, or skips with the reason.
func liveStores(t *testing.T, env string) ([]liveStore, string) {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(env))
	if value == "" {
		t.Skipf("%s is unset: no live Jaeger to send Kiali's own api_v3 request to", env)
	}
	service := strings.TrimSpace(os.Getenv(liveEnvService))
	if service == "" {
		// Not a skip: a search that narrows to nothing cannot tell an accepted request from a
		// rejected one, because both answer with no traces.
		t.Fatalf("%s is set but %s is not: these tests assert that Kiali's request narrows to a seeded service, which needs its name", env, liveEnvService)
	}

	var stores []liveStore
	for _, entry := range strings.Split(value, ",") {
		version, addr, found := strings.Cut(strings.TrimSpace(entry), "=")
		require.True(t, found, "%s entry %q is not a version=address pair", env, entry)
		stores = append(stores, liveStore{addr: addr, version: version})
	}
	return stores, service
}

// liveQuery is the search Kiali builds for a trace list: a window, the cluster tag it appends to
// every query, and the limit the handler defaults to.
func liveQuery() models.TracingQuery {
	clusterID := strings.TrimSpace(os.Getenv(liveEnvClusterID))
	if clusterID == "" {
		clusterID = liveDefaultClusterID
	}
	now := time.Now()
	return models.TracingQuery{
		End:   now.Add(1 * time.Minute),
		Limit: 100,
		Start: now.Add(-1 * time.Hour),
		Tags:  map[string]string{models.IstioClusterTag: clusterID},
	}
}

// TestValuesAreAcceptedByALiveJaeger sends exactly what values() renders.
//
// The assertion is acceptance and narrowing together. Acceptance alone would pass on a rendering
// whose filters the gateway silently drops, since that answers 200 with the whole window, so the
// seeded service has to be found and a service that does not exist has to be absent.
func TestValuesAreAcceptedByALiveJaeger(t *testing.T) {
	stores, service := liveStores(t, liveEnvJaegerHTTP)

	for _, store := range stores {
		t.Run(store.version, func(t *testing.T) {
			tq, err := newTraceQuery(service, liveQuery())
			require.NoError(t, err)
			values, err := tq.values()
			require.NoError(t, err)

			status, body := liveGet(t, "http://"+store.addr+searchPath+"?"+values.Encode())
			require.Equal(t, http.StatusOK, status,
				"Jaeger %s refused the request values() renders: %s", store.version, liveSnippet(body))
			assert.NotEmpty(t, liveTraceIDs(t, body),
				"Jaeger %s accepted the request and answered no trace: %s", store.version, liveSnippet(body))

			absent, err := newTraceQuery(service+".no-such-service", liveQuery())
			require.NoError(t, err)
			absentValues, err := absent.values()
			require.NoError(t, err)
			absentStatus, absentBody := liveGet(t, "http://"+store.addr+searchPath+"?"+absentValues.Encode())
			assert.NotEqual(t, http.StatusOK, absentStatus,
				"a service name nothing carries must not answer traces, or the service name is being dropped: %s", liveSnippet(absentBody))
		})
	}
}

// TestProtoIsAcceptedByALiveJaeger sends exactly what proto() renders, over the transport the
// shipped default configures.
func TestProtoIsAcceptedByALiveJaeger(t *testing.T) {
	stores, service := liveStores(t, liveEnvJaegerGRPC)

	for _, store := range stores {
		t.Run(store.version, func(t *testing.T) {
			client, ok := liveClient(t, store)
			if !ok {
				return
			}

			tq, err := newTraceQuery(service, liveQuery())
			require.NoError(t, err)

			ids, err := liveFindTraces(t, client, tq.proto())
			require.NoError(t, err, "Jaeger %s refused the request proto() renders", store.version)
			assert.NotEmpty(t, ids, "Jaeger %s accepted the request and answered no trace", store.version)

			absent, err := newTraceQuery(service+".no-such-service", liveQuery())
			require.NoError(t, err)
			absentIDs, err := liveFindTraces(t, client, absent.proto())
			require.NoError(t, err, "an unmatched search is an empty stream, not an error")
			assert.Empty(t, absentIDs,
				"a service name nothing carries must not answer traces, or the service name is being dropped")
		})
	}
}

// liveClient dials the endpoint and proves it speaks api_v3 before anything depends on it. A
// kubectl port-forward to a gRPC port dies permanently on a connection reset, so an endpoint that
// does not answer is a lab fault and skips this subtest alone.
func liveClient(t *testing.T, store liveStore) (apiv3.QueryServiceClient, bool) {
	t.Helper()
	conn, err := grpc.NewClient(store.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Skipf("Jaeger %s gRPC endpoint %s could not be dialled: %v", store.version, store.addr, err)
		return nil, false
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := apiv3.NewQueryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.GetServices(ctx, &apiv3.GetServicesRequest{}); err != nil {
		t.Skipf("Jaeger %s gRPC endpoint %s not reachable: %v", store.version, store.addr, err)
		return nil, false
	}
	return client, true
}

// liveFindTraces runs the search and reports the distinct trace IDs, accumulating across the
// stream because FindTraces answers chunks.
func liveFindTraces(t *testing.T, client apiv3.QueryServiceClient, request *apiv3.FindTracesRequest) ([]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.FindTraces(ctx, request)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for {
		message, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return ids, nil
		}
		if recvErr != nil {
			return ids, recvErr
		}
		for _, rs := range message.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, span := range ss.GetSpans() {
					id := string(span.GetTraceId())
					if id != "" && !seen[id] {
						seen[id] = true
						ids = append(ids, id)
					}
				}
			}
		}
	}
}

func liveGet(t *testing.T, target string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "%s did not answer", target)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}

func liveTraceIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var envelope struct {
		Result struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						TraceID string `json:"traceId"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope), "the answer is not an api_v3 search body: %s", liveSnippet(body))

	seen := map[string]bool{}
	var ids []string
	for _, rs := range envelope.Result.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				if span.TraceID != "" && !seen[span.TraceID] {
					seen[span.TraceID] = true
					ids = append(ids, span.TraceID)
				}
			}
		}
	}
	return ids
}

func liveSnippet(body []byte) string {
	if len(body) > bodySnippet {
		body = body[:bodySnippet]
	}
	return string(body)
}
