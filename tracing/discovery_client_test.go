package tracing

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/kubernetes"
	"github.com/kiali/kiali/kubernetes/kubetest"
	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
	"github.com/kiali/kiali/tracing/otel"
)

func fakeK8sNs() kubernetes.UserClientInterface {
	objects := []runtime.Object{
		kubetest.FakeNamespace("bookinfo"),
	}

	k8s := kubetest.NewFakeK8sClient(objects...)
	k8s.OpenShift = false
	return k8s
}

func TestCreateClient(t *testing.T) {

	k8s := fakeK8sNs()
	conf := config.Get()
	tc, err := DiagnoseTracingConfig(context.TODO(), conf, k8s.GetToken())

	assert.Nil(t, err)
	assert.NotNil(t, tc)
}

func TestParseUrlValidWithPort(t *testing.T) {
	url := "http://localhost:8080/test/path"

	parsed, logs, err := parseUrl(url)

	assert.Nil(t, err)
	assert.Equal(t, "localhost", parsed.Host)
	assert.Equal(t, "8080", parsed.Port)
	assert.Equal(t, "http://localhost:8080", parsed.BaseUrl)
	assert.Equal(t, "/test/path", parsed.Path)
	assert.Equal(t, "http", parsed.Scheme)

	assert.Len(t, logs, 1)
	assert.Contains(t, logs[0].Result, "[Ok]")
}

func TestParseUrlValidWithoutPort(t *testing.T) {
	url := "https://example.com/api"

	parsed, logs, err := parseUrl(url)

	assert.Nil(t, err)
	assert.Equal(t, "example.com", parsed.Host)
	assert.Equal(t, "", parsed.Port)
	assert.Equal(t, "https://example.com", parsed.BaseUrl)
	assert.Equal(t, "/api", parsed.Path)
	assert.Equal(t, "https", parsed.Scheme)

	assert.Len(t, logs, 1)
	assert.Contains(t, logs[0].Result, "[Ok]")
}

type mockConn struct{}

func (m *mockConn) Read(b []byte) (n int, err error)   { return 0, nil }
func (m *mockConn) Write(b []byte) (n int, err error)  { return len(b), nil }
func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

func TestDiscoverPortsWithDial(t *testing.T) {
	mockDial := func(network, address string, timeout time.Duration) (net.Conn, error) {
		// Simulate only port 80 and 443 being open
		if address == "localhost:80" || address == "localhost:443" {
			return &mockConn{}, nil
		}
		return nil, errors.New("connection refused")
	}
	zl := log.WithGroup(log.TracingLogName)
	openPorts, logs := discoverPortsWithDial(zl, "localhost", mockDial)

	expectedPorts := map[string]bool{
		"80":  true,
		"443": true,
	}

	assert.Equal(t, len(expectedPorts), 2)
	assert.Equal(t, expectedPorts["80"], true)

	for _, port := range openPorts {
		assert.True(t, expectedPorts[port])
		assert.True(t, expectedPorts[port])
	}
	assert.Equal(t, len(logs), 2)
}

func mockMakeRequest(ctx context.Context, client http.Client, endpoint string, body io.Reader) ([]byte, int, error) {
	switch endpoint {
	case "http://tempo-host:3100/api/search?q={}":
		resp := []byte(`{"traces":[{"rootServiceName":"service.namespace"}]}`)
		return resp, 200, nil
	case "http://tempo-host:3100/api/v3/services":
		resp := []byte(`{"services":["service.namespace"]}`)
		return resp, 200, nil
	default:
		return nil, 500, errors.New("mock error")
	}
}

func TestValidateSimpleTempoHTTP(t *testing.T) {
	// Override the global MakeRequest with the mock version
	MakeRequestFunc = mockMakeRequest
	defer func() { MakeRequestFunc = otel.MakeRequest }() // restore after test

	client := http.Client{}
	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{
		Scheme: "http",
		Host:   "tempo-host",
	}
	port := "3100"

	validConfigs, logs := validateSimpleTempoHTTP(context.TODO(), client, zl, parsedUrl, port)

	assert.Len(t, validConfigs, 2, "Should detect two valid configs")
	assert.Equal(t, "tempo", validConfigs[0].Provider)
	assert.Equal(t, "jaeger", validConfigs[1].Provider)

	assert.True(t, *validConfigs[0].NamespaceSelector)
	assert.True(t, *validConfigs[1].NamespaceSelector)

	assert.Equal(t, "http://tempo-host:3100", validConfigs[0].Url)
	assert.Equal(t, "http://tempo-host:3100", validConfigs[1].Url)
	assert.Greater(t, len(logs), 0)
}

// Mock for MakeRequest that simulates HTTP responses
func mockMakeRequestTempo(ctx context.Context, client http.Client, endpoint string, body io.Reader) ([]byte, int, error) {
	switch endpoint {
	case "http://gateway-host:3100/api/traces/v1/tenantA/tempo/api/search?q={}":
		return []byte(`{"traces":[{"rootServiceName":"svc.namespace"}]}`), 200, nil
	case "http://gateway-host:3100/api/traces/v1/tenantA/api/v3/services":
		return []byte(`{"services":["svc.namespace"]}`), 200, nil
	case "http://tempo-host:3100/api/traces/v1/tempo/api/search?q={}":
		return []byte(`{"traces":[{"rootServiceName":"svc.namespace"}]}`), 200, nil
	case "http://tempo-host:3100/api/search?q={}":
		return []byte(`{"traces":[{"rootServiceName":"svc.namespace"}]}`), 200, nil
	case "http://tempo-host:3100/api/v3/services":
		return []byte(`{"services":["svc.namespace"]}`), 200, nil
	case "http://tempo-host:3100/api/traces/v1/api/v3/services":
		return []byte(`{"services":["svc.namespace"]}`), 200, nil
	default:
		return nil, 500, errors.New("not found")
	}
}

func TestValidateTempoHTTP_WithGateway(t *testing.T) {
	MakeRequestFunc = mockMakeRequestTempo
	defer func() { MakeRequestFunc = otel.MakeRequest }()

	client := http.Client{}
	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{
		Scheme: "http",
		Host:   "gateway-host",
		Path:   "/api/traces/v1/tenantA", // path includes tenant
	}
	port := "3100"

	validConfigs, logs := validateTempoHTTP(context.TODO(), client, zl, parsedUrl, port)

	assert.Len(t, validConfigs, 2)
	assert.Equal(t, "tempo", validConfigs[0].Provider)
	assert.Equal(t, "jaeger", validConfigs[1].Provider)
	assert.Greater(t, len(logs), 0)
}

func TestValidateTempoHTTP_WithoutGateway(t *testing.T) {
	MakeRequestFunc = mockMakeRequestTempo
	defer func() { MakeRequestFunc = otel.MakeRequest }()

	client := http.Client{}
	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{
		Scheme: "http",
		Host:   "tempo-host",
		Path:   "",
	}
	port := "3100"

	validConfigs, logs := validateTempoHTTP(context.TODO(), client, zl, parsedUrl, port)

	assert.Len(t, validConfigs, 2)
	assert.Equal(t, "tempo", validConfigs[0].Provider)
	assert.Equal(t, "jaeger", validConfigs[1].Provider)
	assert.Greater(t, len(logs), 0)
}

func TestValidateTempoHTTP_GatewayMissingTenant(t *testing.T) {
	MakeRequestFunc = mockMakeRequestTempo
	defer func() { MakeRequestFunc = otel.MakeRequest }()

	client := http.Client{}
	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{
		Scheme: "http",
		Host:   "gateway-host",
		Path:   "/api/traces/v1", // No tenant segment
	}
	port := "3100"

	validConfigs, logs := validateTempoHTTP(context.TODO(), client, zl, parsedUrl, port)

	// Should fail due to missing tenant
	assert.Len(t, validConfigs, 0)
	assert.True(t, anyLogContains(logs, "tenant name not found"))
}

// helper to check logs
func anyLogContains(logs []models.LogLine, substr string) bool {
	for _, l := range logs {
		if strings.Contains(l.Result, substr) {
			return true
		}
	}
	return false
}

// Mock MakeRequest for Jaeger scenarios
func mockMakeRequestJaeger(ctx context.Context, client http.Client, endpoint string, body io.Reader) ([]byte, int, error) {
	switch endpoint {
	case "http://jaeger-host:16686/jaeger/api/v3/services":
		return []byte(`{"services":["svc.namespace"]}`), 200, nil
	case "http://jaeger-host:16686/api/v3/services":
		return []byte(`{"services":["svc.namespace"]}`), 200, nil
	default:
		return nil, 404, errors.New("not found")
	}
}

func TestValidateJaegerHTTP_SuccessfulBothEndpoints(t *testing.T) {
	MakeRequestFunc = mockMakeRequestJaeger
	defer func() { MakeRequestFunc = otel.MakeRequest }()

	client := http.Client{}
	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{
		Scheme: "http",
		Host:   "jaeger-host",
	}
	port := "16686"

	validConfigs, logs := validateJaegerHTTP(context.TODO(), client, zl, parsedUrl, port)

	assert.Len(t, validConfigs, 2)
	assert.Equal(t, "jaeger", validConfigs[0].Provider)
	assert.Equal(t, "jaeger", validConfigs[1].Provider)
	assert.GreaterOrEqual(t, len(logs), 2)
	assert.True(t, *validConfigs[0].NamespaceSelector)
	assert.True(t, *validConfigs[1].NamespaceSelector)
}

func TestValidateJaegerHTTP_OnlyOneSuccess(t *testing.T) {
	MakeRequestFunc = func(ctx context.Context, client http.Client, endpoint string, body io.Reader) ([]byte, int, error) {
		if endpoint == "http://jaeger-host:16686/jaeger/api/v3/services" {
			return nil, 500, errors.New("fail")
		}
		if endpoint == "http://jaeger-host:16686/api/v3/services" {
			return []byte(`{"services":["svc"]}`), 200, nil
		}
		return nil, 404, errors.New("not found")
	}
	defer func() { MakeRequestFunc = otel.MakeRequest }()

	client := http.Client{}
	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{
		Scheme: "http",
		Host:   "jaeger-host",
	}
	port := "16686"

	validConfigs, logs := validateJaegerHTTP(context.TODO(), client, zl, parsedUrl, port)

	assert.Len(t, validConfigs, 1)
	assert.Equal(t, "jaeger", validConfigs[0].Provider)
	assert.Equal(t, "http://jaeger-host:16686", validConfigs[0].Url)
	assert.False(t, *validConfigs[0].NamespaceSelector)
	assert.GreaterOrEqual(t, len(logs), 1)
}

func TestValidateJaegerHTTP_NoSuccess(t *testing.T) {
	MakeRequestFunc = func(ctx context.Context, client http.Client, endpoint string, body io.Reader) ([]byte, int, error) {
		return nil, 500, errors.New("mock failure")
	}
	defer func() { MakeRequestFunc = otel.MakeRequest }()

	client := http.Client{}
	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{
		Scheme: "http",
		Host:   "jaeger-host",
	}
	port := "16686"

	validConfigs, logs := validateJaegerHTTP(context.TODO(), client, zl, parsedUrl, port)

	assert.Len(t, validConfigs, 0)
	assert.GreaterOrEqual(t, len(logs), 2)
}

// An endpoint answering something other than api_v3 at /api/v3/services is never suggested as a
// Jaeger configuration, because the client would refuse it.
func TestValidateJaegerHTTP_NotAPIv3(t *testing.T) {
	cases := map[string][]byte{
		"pre-api_v3 shape":         []byte(`{"data":["svc.namespace"]}`),
		"UI document":              []byte(`<!doctype html><html><head><title>Jaeger UI</title></head></html>`),
		"JSON without services":    []byte(`{"result":{}}`),
		"router's plain-text page": []byte(`404 page not found`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			MakeRequestFunc = func(ctx context.Context, client http.Client, endpoint string, _ io.Reader) ([]byte, int, error) {
				return body, 200, nil
			}
			defer func() { MakeRequestFunc = otel.MakeRequest }()

			zl := log.WithGroup(log.TracingLogName)
			parsedUrl := models.ParsedUrl{Scheme: "http", Host: "jaeger-host"}

			validConfigs, logs := validateJaegerHTTP(context.TODO(), http.Client{}, zl, parsedUrl, "16686")

			assert.Empty(t, validConfigs)
			assert.True(t, anyLogContains(logs, "api_v3"))
			assert.True(t, anyLogContains(logs, "1.54"))
		})
	}
}

// HTTP sent to a gRPC port fails reading a "malformed HTTP response", and the diagnose log says
// that the port or transport is wrong, as the client's own error does.
func TestValidateJaegerHTTP_GRPCPort(t *testing.T) {
	MakeRequestFunc = func(ctx context.Context, client http.Client, endpoint string, _ io.Reader) ([]byte, int, error) {
		return nil, 0, errors.New(`Get "` + endpoint + `": net/http: HTTP/1.x transport connection broken: malformed HTTP response "\x00\x00\x06\x04"`)
	}
	defer func() { MakeRequestFunc = otel.MakeRequest }()

	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{Scheme: "http", Host: "jaeger-host"}

	validConfigs, logs := validateJaegerHTTP(context.TODO(), http.Client{}, zl, parsedUrl, "16685")

	assert.Empty(t, validConfigs)
	assert.True(t, anyLogContains(logs, "wrong port or transport"))
	assert.True(t, anyLogContains(logs, "malformed HTTP response"))
}

// Tempo's own /api/v3 answers 404, so a Tempo host yields only the Tempo configuration.
func TestValidateSimpleTempoHTTP_NoJaegerAPIv3(t *testing.T) {
	MakeRequestFunc = func(ctx context.Context, client http.Client, endpoint string, _ io.Reader) ([]byte, int, error) {
		switch endpoint {
		case "http://tempo-host:3200/api/search?q={}":
			return []byte(`{"traces":[{"rootServiceName":"svc"}]}`), 200, nil
		case "http://tempo-host:3200/api/v3/services":
			return []byte("404 page not found"), 404, nil
		default:
			return nil, 500, errors.New("unexpected endpoint " + endpoint)
		}
	}
	defer func() { MakeRequestFunc = otel.MakeRequest }()

	zl := log.WithGroup(log.TracingLogName)
	parsedUrl := models.ParsedUrl{Scheme: "http", Host: "tempo-host"}

	validConfigs, _ := validateSimpleTempoHTTP(context.TODO(), http.Client{}, zl, parsedUrl, "3200")

	assert.Len(t, validConfigs, 1)
	assert.Equal(t, "tempo", validConfigs[0].Provider)
}

type fakeAPIv3Services struct {
	apiv3.UnimplementedQueryServiceServer
	services []string
}

func (f *fakeAPIv3Services) GetServices(context.Context, *apiv3.GetServicesRequest) (*apiv3.GetServicesResponse, error) {
	return &apiv3.GetServicesResponse{Services: f.services}, nil
}

// startGRPCServer serves the given registrations on a loopback port and returns its port.
func startGRPCServer(t *testing.T, register func(*grpc.Server)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	return port
}

func TestValidateJaegerGRPC_APIv3(t *testing.T) {
	port := startGRPCServer(t, func(s *grpc.Server) {
		apiv3.RegisterQueryServiceServer(s, &fakeAPIv3Services{services: []string{"productpage.bookinfo"}})
	})
	conf := config.NewConfig()
	parsedUrl := models.ParsedUrl{Scheme: "http", Host: "127.0.0.1"}

	validConfigs, logs := validateJaegerGRPC(context.TODO(), conf, &config.Auth{}, parsedUrl, port)

	require.Len(t, validConfigs, 1)
	assert.Equal(t, "jaeger", validConfigs[0].Provider)
	assert.True(t, validConfigs[0].UseGRPC)
	assert.Equal(t, "http://127.0.0.1:"+port, validConfigs[0].Url)
	require.NotNil(t, validConfigs[0].NamespaceSelector)
	assert.True(t, *validConfigs[0].NamespaceSelector)
	assert.True(t, anyLogContains(logs, "Valid Jaeger api_v3 gRPC Client found"))
}

// A gRPC server without jaeger.api_v3.QueryService (Jaeger before 1.54, or another service on the
// port) is not suggested, and the log says what is missing.
func TestValidateJaegerGRPC_NotAPIv3(t *testing.T) {
	port := startGRPCServer(t, func(*grpc.Server) {})
	conf := config.NewConfig()
	parsedUrl := models.ParsedUrl{Scheme: "http", Host: "127.0.0.1"}

	validConfigs, logs := validateJaegerGRPC(context.TODO(), conf, &config.Auth{}, parsedUrl, port)

	assert.Empty(t, validConfigs)
	assert.True(t, anyLogContains(logs, "jaeger.api_v3.QueryService"))
	assert.True(t, anyLogContains(logs, "1.54"))
}
