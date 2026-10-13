package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kiali/kiali/business"
	"github.com/kiali/kiali/cache"
	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/istio"
	"github.com/kiali/kiali/kubernetes"
	"github.com/kiali/kiali/kubernetes/kubetest"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing"
	"github.com/kiali/kiali/util/certtest"
)

func setupTracingCheckEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	conf := config.NewConfig()
	// The endpoint answers only a user with access to a control plane namespace, so the fake
	// cluster holds an istiod for discovery to find.
	k8s := kubetest.NewFakeK8sClient(
		kubetest.FakeNamespace(config.IstioNamespaceDefault),
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "istiod",
				Namespace: config.IstioNamespaceDefault,
				Labels:    map[string]string{"app": "istiod", "istio.io/rev": "default"},
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "istiod", "istio.io/rev": "default"}},
				},
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "istio",
				Namespace: config.IstioNamespaceDefault,
				Labels:    map[string]string{"istio.io/rev": "default"},
			},
			Data: map[string]string{"mesh": ""},
		},
		certtest.FakeIstioCertificateConfigMap(config.IstioNamespaceDefault),
	)
	cf := kubetest.NewFakeClientFactoryWithClient(conf, k8s)
	kialiCache := cache.NewTestingCacheWithFactory(t, cf, *conf)
	discovery := istio.NewDiscovery(kubernetes.ConvertFromUserClients(cf.Clients), kialiCache, conf)
	traceLoader := func() tracing.ClientInterface { return nil }

	handler := WithFakeAuthInfo(conf, TracingConfigurationCheck(conf, kialiCache, cf, nil, &business.FakeControlPlaneMonitor{}, traceLoader, nil, discovery))
	mr := mux.NewRouter()
	mr.HandleFunc("/api/tracing/check", handler).Methods(http.MethodPost)

	ts := httptest.NewServer(mr)
	t.Cleanup(ts.Close)
	return ts
}

func postTracingCheck(t *testing.T, ts *httptest.Server, tracingConfig map[string]any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(tracingConfig)
	require.NoError(t, err)
	resp, err := http.Post(ts.URL+"/api/tracing/check", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, answer
}

// closedAddress returns an address nothing listens on, which refuses a connection at once.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

func TestTracingConfigurationCheckRefusesUnknownProvider(t *testing.T) {
	ts := setupTracingCheckEndpoint(t)

	code, answer := postTracingCheck(t, ts, map[string]any{"enabled": true, "internalURL": "http://" + closedAddress(t), "provider": "zipkin"})

	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, string(answer), "unknown provider [zipkin]")
}

// An empty provider is checked as Jaeger: the probe goes to api_v3 and reports the backend's own
// dial error.
func TestTracingConfigurationCheckDefaultsEmptyProviderToJaeger(t *testing.T) {
	ts := setupTracingCheckEndpoint(t)

	code, answer := postTracingCheck(t, ts, map[string]any{"enabled": true, "internalURL": "http://" + closedAddress(t)})

	require.Equal(t, http.StatusOK, code, string(answer))
	var validation models.ConfigurationValidation
	require.NoError(t, json.Unmarshal(answer, &validation))
	assert.Contains(t, validation.Error, "/api/v3/services")
	assert.Contains(t, validation.Error, "connection refused")
}
