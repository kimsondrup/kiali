package tracing

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
)

const token = "1234567890"

// The api_v3 transports implement the tracing interfaces. The assertions live here because
// package jaeger cannot import package tracing, which imports it.
var (
	_ GRPCClientInterface = (*jaeger.GRPCClient)(nil)
	_ HTTPClientInterface = (*jaeger.HTTPClient)(nil)
)

func TestCreateJaegerClient(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.UseGRPC = false

	tracingClient, err := NewClient(context.Background(), conf, token, true)

	assert.Nil(t, err)
	assert.NotNil(t, tracingClient)
}

func TestCreateTempogGRPCClient(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.Provider = "tempo"
	conf.ExternalServices.Tracing.UseGRPC = true

	tracingClient, err := NewClient(context.Background(), conf, token, true)

	assert.Nil(t, err)
	assert.NotNil(t, tracingClient)
}

func TestCreateTempoHTTPClient(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.Provider = "tempo"
	conf.ExternalServices.Tracing.UseGRPC = false

	tracingClient, err := NewClient(context.Background(), conf, token, true)

	assert.Nil(t, err)
	assert.NotNil(t, tracingClient)
}

func TestTempoURLConstructionWithTenant(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.Provider = "tempo"
	conf.ExternalServices.Tracing.UseGRPC = false
	conf.ExternalServices.Tracing.InternalURL = "http://tempo-server:8080"
	conf.ExternalServices.Tracing.TempoConfig.Tenant = "my-tenant"

	// The client should be created successfully with the tenant URL constructed
	client, err := NewClient(context.Background(), conf, token, false)

	// The client should be created successfully
	assert.Nil(t, err)
	assert.NotNil(t, client)

	assert.Equal(t, client.baseURL.String(), "http://tempo-server:8080/api/traces/v1/my-tenant/tempo")
}

func TestTempoURLConstructionJaegerWithTenant(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.Provider = "jaeger"
	conf.ExternalServices.Tracing.UseGRPC = false
	conf.ExternalServices.Tracing.InternalURL = "http://tempo-server:8080"
	conf.ExternalServices.Tracing.TempoConfig.Tenant = "my-tenant"

	// The client should be created successfully with the tenant URL constructed
	client, err := NewClient(context.Background(), conf, token, false)

	// The client should be created successfully
	assert.Nil(t, err)
	assert.NotNil(t, client)

	assert.Equal(t, client.baseURL.String(), "http://tempo-server:8080")
}

func TestTempoURLConstructionWithoutTenant(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.Provider = "tempo"
	conf.ExternalServices.Tracing.UseGRPC = false
	conf.ExternalServices.Tracing.InternalURL = "http://tempo-server:8080"
	// No tenant configured

	// The client should be created successfully without URL modification
	client, err := NewClient(context.Background(), conf, token, false)

	// The client should be created successfully
	assert.Nil(t, err)
	assert.NotNil(t, client)

	assert.Equal(t, "http://tempo-server:8080", client.baseURL.String())
}

func TestTempoURLConstructionWithTenantAndCompleteURL(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.Provider = "tempo"
	conf.ExternalServices.Tracing.UseGRPC = false
	conf.ExternalServices.Tracing.InternalURL = "http://tempo-server:8080/api/traces/v1/my-tenant/tempo"
	// No tenant configured

	// The client should be created successfully without URL modification
	client, err := NewClient(context.Background(), conf, token, false)

	// The client should be created successfully
	assert.Nil(t, err)
	assert.NotNil(t, client)

	assert.Equal(t, "http://tempo-server:8080/api/traces/v1/my-tenant/tempo", client.baseURL.String())
}

func TestCreateTempoHTTPClient_OAuth2(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.Provider = "tempo"
	conf.ExternalServices.Tracing.UseGRPC = false
	conf.ExternalServices.Tracing.InternalURL = "http://tempo-server:8080"
	conf.ExternalServices.Tracing.Auth.Type = config.AuthTypeOAuth2
	conf.ExternalServices.Tracing.Auth.OAuth2.TokenURL = "https://idp.example.com/token"
	conf.ExternalServices.Tracing.Auth.OAuth2.ClientID = "my-client"
	conf.ExternalServices.Tracing.Auth.OAuth2.ClientSecret = "my-secret"
	conf.ExternalServices.Tracing.Auth.OAuth2.AuthStyle = "header"

	tracingClient, err := NewClient(context.Background(), conf, token, false)

	assert.Nil(t, err)
	assert.NotNil(t, tracingClient)
}

func TestNewClientRecordsProviderAndTransport(t *testing.T) {
	cases := map[string]struct {
		expectedProvider  config.TracingProvider
		expectedTransport transport
		provider          config.TracingProvider
		useGRPC           bool
	}{
		"jaeger over HTTP": {provider: config.JaegerProvider, useGRPC: false, expectedProvider: config.JaegerProvider, expectedTransport: transportHTTP},
		"jaeger over gRPC": {provider: config.JaegerProvider, useGRPC: true, expectedProvider: config.JaegerProvider, expectedTransport: transportGRPC},
		"tempo over HTTP":  {provider: config.TempoProvider, useGRPC: false, expectedProvider: config.TempoProvider, expectedTransport: transportHTTP},
		"tempo over gRPC":  {provider: config.TempoProvider, useGRPC: true, expectedProvider: config.TempoProvider, expectedTransport: transportGRPC},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			conf := config.NewConfig()
			conf.ExternalServices.Tracing.Enabled = true
			conf.ExternalServices.Tracing.Provider = tc.provider
			conf.ExternalServices.Tracing.UseGRPC = tc.useGRPC

			client, err := NewClient(context.Background(), conf, token, false)
			require.NoError(t, err)
			require.NotNil(t, client)

			assert.Equal(t, tc.expectedProvider, client.provider)
			assert.Equal(t, tc.expectedTransport, client.transport)
		})
	}
}

// recordingHTTPClient and recordingGRPCClient record which transport a request reached, so the
// dispatch test can tell them apart without a backend.
type recordingHTTPClient struct {
	calls []string
}

func (r *recordingHTTPClient) GetAppTracesHTTP(_ context.Context, _ http.Client, _ *url.URL, _ string, _ models.TracingQuery) (*models.TracingResponse, error) {
	r.calls = append(r.calls, "traces")
	return &models.TracingResponse{}, nil
}

func (r *recordingHTTPClient) GetTraceDetailHTTP(_ context.Context, _ http.Client, _ *url.URL, _ string) (*models.TracingSingleTrace, error) {
	r.calls = append(r.calls, "detail")
	return nil, nil
}

func (r *recordingHTTPClient) GetServiceStatusHTTP(_ context.Context, _ http.Client, _ *url.URL) (bool, error) {
	r.calls = append(r.calls, "status")
	return true, nil
}

func (r *recordingHTTPClient) GetServices(_ context.Context, _ http.Client, _ *url.URL) ([]string, error) {
	r.calls = append(r.calls, "services")
	return nil, nil
}

type recordingGRPCClient struct {
	calls []string
}

func (r *recordingGRPCClient) FindTraces(_ context.Context, _ string, _ models.TracingQuery) (*models.TracingResponse, error) {
	r.calls = append(r.calls, "traces")
	return &models.TracingResponse{}, nil
}

func (r *recordingGRPCClient) GetTrace(_ context.Context, _ string) (*models.TracingSingleTrace, error) {
	r.calls = append(r.calls, "detail")
	return nil, nil
}

func (r *recordingGRPCClient) GetServices(_ context.Context) (bool, error) {
	r.calls = append(r.calls, "status")
	return true, nil
}

func (r *recordingGRPCClient) GetServicesList(_ context.Context) ([]string, error) {
	r.calls = append(r.calls, "services")
	return nil, nil
}

func TestClientDispatchesOnTransport(t *testing.T) {
	cases := map[string]struct {
		expectedGRPC []string
		expectedHTTP []string
		provider     config.TracingProvider
		transport    transport
	}{
		// Tempo's gRPC API only searches, so its detail stays on HTTP.
		"tempo over gRPC":  {provider: config.TempoProvider, transport: transportGRPC, expectedGRPC: []string{"traces", "status", "services"}, expectedHTTP: []string{"detail"}},
		"jaeger over gRPC": {provider: config.JaegerProvider, transport: transportGRPC, expectedGRPC: []string{"traces", "detail", "status", "services"}},
		"tempo over HTTP":  {provider: config.TempoProvider, transport: transportHTTP, expectedHTTP: []string{"traces", "detail", "status", "services"}},
		"jaeger over HTTP": {provider: config.JaegerProvider, transport: transportHTTP, expectedHTTP: []string{"traces", "detail", "status", "services"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			httpClient := &recordingHTTPClient{}
			grpcClient := &recordingGRPCClient{}
			client := &Client{grpcClient: grpcClient, httpTracingClient: httpClient, provider: tc.provider, transport: tc.transport}
			ctx := context.Background()

			_, err := client.GetAppTraces(ctx, "ns", "app", models.TracingQuery{})
			require.NoError(t, err)
			_, err = client.GetTraceDetail(ctx, "0123456789abcdef")
			require.NoError(t, err)
			_, err = client.GetServiceStatus(ctx)
			require.NoError(t, err)
			_, err = client.GetServices(ctx)
			require.NoError(t, err)

			assert.Equal(t, tc.expectedGRPC, grpcClient.calls)
			assert.Equal(t, tc.expectedHTTP, httpClient.calls)
		})
	}
}

func TestNewClientReadsJaegerAPIv3(t *testing.T) {
	cases := map[string]struct {
		provider config.TracingProvider
		useGRPC  bool
	}{
		"jaeger over HTTP": {provider: config.JaegerProvider, useGRPC: false},
		"jaeger over gRPC": {provider: config.JaegerProvider, useGRPC: true},
		// A configuration posted to /api/tracing/check can leave the provider out.
		"empty provider over HTTP": {provider: "", useGRPC: false},
		"empty provider over gRPC": {provider: "", useGRPC: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			conf := config.NewConfig()
			conf.ExternalServices.Tracing.Enabled = true
			conf.ExternalServices.Tracing.Provider = tc.provider
			conf.ExternalServices.Tracing.UseGRPC = tc.useGRPC

			client, err := NewClient(context.Background(), conf, token, false)
			require.NoError(t, err)
			require.NotNil(t, client)

			assert.Equal(t, config.JaegerProvider, client.provider)
			if tc.useGRPC {
				assert.Equal(t, transportGRPC, client.transport)
				assert.IsType(t, &jaeger.GRPCClient{}, client.grpcClient)
				assert.Nil(t, client.httpTracingClient)
			} else {
				assert.Equal(t, transportHTTP, client.transport)
				assert.IsType(t, &jaeger.HTTPClient{}, client.httpTracingClient)
				assert.Nil(t, client.grpcClient)
			}
		})
	}
}

func TestNewClientRefusesUnknownProvider(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.Provider = "zipkin"
	conf.ExternalServices.Tracing.UseGRPC = false

	client, err := NewClient(context.Background(), conf, token, false)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "zipkin")
}

// The status probe of /api/tracing/check reports a dial error in the backend's own words, which
// business.TestValidateConfiguration relies on.
func TestJaegerHTTPStatusKeepsDialError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.InternalURL = "http://" + address
	conf.ExternalServices.Tracing.UseGRPC = false

	client, err := NewClient(context.Background(), conf, token, false)
	require.NoError(t, err)

	ok, err := client.GetServiceStatus(context.Background())
	assert.False(t, ok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

// bigTraceServer answers GetTrace with one trace of n spans in a single message, the way api_v3
// sends a trace.
type bigTraceServer struct {
	apiv3.UnimplementedQueryServiceServer

	spans int
}

func (s *bigTraceServer) GetTrace(_ *apiv3.GetTraceRequest, stream grpc.ServerStreamingServer[tracepb.TracesData]) error {
	traceID := make([]byte, 16)
	traceID[15] = 1
	spans := make([]*tracepb.Span, 0, s.spans)
	for i := range s.spans {
		spanID := make([]byte, 8)
		spanID[6], spanID[7] = byte(i>>8), byte(i)
		spans = append(spans, &tracepb.Span{
			EndTimeUnixNano:   2_000,
			Name:              strings.Repeat("a long operation name ", 20),
			SpanId:            spanID,
			StartTimeUnixNano: 1_000,
			TraceId:           traceID,
		})
	}
	return stream.Send(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}})
}

// The connection the Jaeger gRPC client dials reads a trace above gRPC's default 4 MiB receive
// limit, and Close releases it.
func TestJaegerGRPCClientReadsATraceAboveFourMiB(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	apiv3.RegisterQueryServiceServer(server, &bigTraceServer{spans: 10000})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.InternalURL = "http://" + listener.Addr().String()
	conf.ExternalServices.Tracing.UseGRPC = true
	config.Set(conf)

	client, err := NewClient(context.Background(), conf, token, false)
	require.NoError(t, err)

	trace, err := client.GetTraceDetail(context.Background(), "00000000000000000000000000000001")
	require.NoError(t, err)
	require.NotNil(t, trace)
	assert.Len(t, trace.Data.Spans, 10000)

	require.NoError(t, client.Close())
	_, err = client.GetTraceDetail(context.Background(), "00000000000000000000000000000001")
	assert.Equal(t, codes.Canceled, status.Code(err), "a closed connection answers no call")
}

// A client that dialled nothing has nothing to close.
func TestCloseOfAnHTTPClient(t *testing.T) {
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	conf.ExternalServices.Tracing.UseGRPC = false

	client, err := NewClient(context.Background(), conf, token, false)
	require.NoError(t, err)
	assert.NoError(t, client.Close())
	assert.NoError(t, (*Client)(nil).Close())
}
