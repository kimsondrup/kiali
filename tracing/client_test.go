package tracing

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger"
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
