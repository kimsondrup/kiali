package tracing

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/prometheus/internalmetrics"
	"github.com/kiali/kiali/tracing/jaeger"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
	"github.com/kiali/kiali/tracing/tempo"
	"github.com/kiali/kiali/util/grpcutil"
	"github.com/kiali/kiali/util/httputil"
)

const (
	newClientRetryInterval = 30 * time.Second
)

// ClientInterface for mocks (only mocked function are necessary here)
type ClientInterface interface {
	GetAppTraces(ctx context.Context, ns, app string, query models.TracingQuery) (traces *models.TracingResponse, err error)
	GetTraceDetail(ctx context.Context, traceId string) (*models.TracingSingleTrace, error)
	GetErrorTraces(ctx context.Context, ns, app string, duration time.Duration) (errorTraces int, err error)
	GetServiceStatus(ctx context.Context) (available bool, err error)
	GetServices(ctx context.Context) ([]string, error)
}

// HTTPClientInterface for Mocks, also for Tempo or Jaeger
type HTTPClientInterface interface {
	GetAppTracesHTTP(ctx context.Context, client http.Client, baseURL *url.URL, serviceName string, q models.TracingQuery) (response *models.TracingResponse, err error)
	GetTraceDetailHTTP(ctx context.Context, client http.Client, endpoint *url.URL, traceID string) (*models.TracingSingleTrace, error)
	GetServiceStatusHTTP(ctx context.Context, client http.Client, baseURL *url.URL) (bool, error)
	GetServices(ctx context.Context, client http.Client, baseURL *url.URL) ([]string, error)
}

// GRPCClientInterface for Mocks, also for Tempo or Jaeger
type GRPCClientInterface interface {
	FindTraces(ctx context.Context, app string, q models.TracingQuery) (response *models.TracingResponse, err error)
	GetTrace(ctx context.Context, traceID string) (*models.TracingSingleTrace, error)
	GetServices(ctx context.Context) (bool, error)
	GetServicesList(ctx context.Context) ([]string, error)
}

// transport is the protocol a Client sends its requests over. It is recorded when the client is
// built rather than inferred from which clients happen to be set, because Tempo with use_grpc holds
// both a gRPC and an HTTP client and still reads some requests over HTTP.
type transport int

const (
	transportHTTP transport = iota
	transportGRPC
)

// Client for Tracing API.
type Client struct {
	ClientInterface
	baseURL           *url.URL
	conn              *grpc.ClientConn
	customHeaders     map[string]string
	grpcClient        GRPCClientInterface
	httpClient        http.Client
	httpTracingClient HTTPClientInterface
	provider          config.TracingProvider
	transport         transport
}

// NewClient creates a tracing Client. If it fails to create the client for any reason,
// it will retry indefinitely until the context is cancelled.
// Unless retry is false
func NewClient(ctx context.Context, conf *config.Config, token string, retry bool) (*Client, error) {
	var (
		client *Client
		err    error
	)

	// prepare the client logger and put it in the context
	// this context and logger is only used during the instantiation process
	zl := log.WithGroup(log.TracingLogName)
	ctx = log.ToContext(ctx, zl)

	retryErr := wait.PollUntilContextCancel(ctx, newClientRetryInterval, true, func(ctx context.Context) (bool, error) {
		client, err = newClient(ctx, conf, token)
		if err != nil {
			zl.Error().Msgf("Error creating tracing client: [%v]. Retrying in [%s]", err, newClientRetryInterval)
			if retry {
				return false, nil
			}
		}

		return true, nil
	})
	if retryErr != nil {
		zl.Error().Msgf("Error creating tracing client: [%v]. Will not retry.", err)
		return nil, err
	}

	// Without retry the poll ends on the first attempt whatever its outcome, so its error is
	// the caller's: /api/tracing/check reports it instead of a bare "Error creating tracing client".
	return client, err
}

func newClient(ctx context.Context, conf *config.Config, token string) (*Client, error) {
	cfgTracing := conf.ExternalServices.Tracing
	if !cfgTracing.Enabled {
		return nil, errors.New("tracing is not enabled")
	}

	// Config load rejects an unknown provider, but a configuration posted to
	// /api/tracing/check, and many tests, can carry an empty one. Reading that as Jaeger is
	// a decision, so it is made here rather than left to whichever branch is the else.
	provider := cfgTracing.Provider
	if provider == "" {
		provider = config.JaegerProvider
	}
	if provider != config.JaegerProvider && provider != config.TempoProvider {
		return nil, fmt.Errorf("unknown tracing provider [%s]: it must be [%s] or [%s]", provider, config.JaegerProvider, config.TempoProvider)
	}

	zl := log.FromContext(ctx)

	var httpTracingClient HTTPClientInterface
	auth := cfgTracing.Auth
	if auth.UseKialiToken {
		auth.Token = config.Credential(token)
	}

	var u *url.URL
	var errParse error
	isInternalURL := false
	if cfgTracing.InternalURL != "" {
		u, errParse = url.Parse(cfgTracing.InternalURL)
		if errParse != nil {
			zl.Error().Msgf("Error parsing tracing InternalURL: %s", errParse)
			return nil, errParse
		}
		isInternalURL = true
	} else {
		u, errParse = url.Parse(cfgTracing.ExternalURL)
		if errParse != nil {
			zl.Error().Msgf("Error parsing tracing ExternalURL: %s", errParse)
			return nil, errParse
		}
	}

	port := u.Port()
	if port == "" {
		p, _ := net.LookupPort("tcp", u.Scheme)
		port = strconv.Itoa(p)
	}
	host := u.Hostname()

	// For Tempo provider with tenant, construct tenant-specific URL if needed
	// Just for internal URL, the external URL can be exposed on a different URL, hidden internal path
	tempo.ConstructTempoTenantURL(u, &cfgTracing, isInternalURL)

	address := host + ":" + port
	zl.Trace().Msgf("[%s] client info: address=[%s], auth.type=[%s]", cfgTracing.Provider, address, auth.Type)

	if cfgTracing.UseGRPC && provider == config.JaegerProvider {
		opts, err := grpcutil.GetAuthDialOptions(conf, host, u.Scheme == "https", &auth)
		if err != nil {
			zl.Error().Msgf("Error while building GRPC dial options: %v", err)
			return nil, err
		}

		// Note: jaeger-query does not have built-in secured communication, at the moment it is only achieved through reverse proxies (cf https://github.com/jaegertracing/jaeger/issues/1718).
		// When using the GRPC client, if a proxy is used it has to support GRPC.
		// Basic and Token auth are in theory implemented for the GRPC client (see package grpcutil) but were not tested because openshift's oauth-proxy doesn't support GRPC at the time.
		//
		// api_v3 is dialled at the URL's host and port, which serve it next to api_v2 (16685 by
		// default). The URL's path means nothing to gRPC, and grpc_port belongs to Tempo, whose
		// gRPC API listens apart from its HTTP one.
		conn, err := grpc.NewClient(address, append(opts, jaeger.DialOptions()...)...)
		if err != nil {
			zl.Error().Msgf("Error creating client: %v", err)
			return nil, err
		}
		// The timeout is the running config's, as on the HTTP branch: a config posted to
		// /api/tracing/check may leave query_timeout out, and a zero would leave the health probe
		// with no deadline at all.
		timeout := time.Duration(config.Get().ExternalServices.Tracing.QueryTimeout) * time.Second
		client := jaeger.NewGRPCClient(apiv3.NewQueryServiceClient(conn), timeout)
		zl.Info().Msgf("Created Jaeger api_v3 gRPC client address=[%s]", address)
		return &Client{conn: conn, customHeaders: cfgTracing.CustomHeaders, grpcClient: client, provider: config.JaegerProvider, transport: transportGRPC}, nil
	} else {
		// HTTP client
		zl.Trace().Msgf("Using HTTP client for Tracing: url=[%v], auth.type=[%s]", u, auth.Type)
		timeout := time.Duration(config.Get().ExternalServices.Tracing.QueryTimeout) * time.Second
		transport, err := httputil.CreateTransport(conf, &auth, &http.Transport{}, timeout, cfgTracing.CustomHeaders)
		if err != nil {
			return nil, err
		}
		client := http.Client{Transport: transport, Timeout: timeout}
		zl.Info().Msgf("Create Tracing HTTP client [%s]", u)

		if provider == config.TempoProvider {
			httpTracingClient, err = tempo.NewOtelClient(ctx)
			if err != nil {
				zl.Error().Msgf("Error creating HTTP client: %v", err)
				return nil, err
			}

			// Tempo uses gRPC stream client just for search
			// Get a single trace requires the http client
			if cfgTracing.UseGRPC {
				dialOps, err := grpcutil.GetAuthDialOptions(conf, host, u.Scheme == "https", &auth)
				if err != nil {
					zl.Error().Msgf("Error creating gRPC dial options: %v", err)
					return nil, err
				}
				grpcAddress := fmt.Sprintf("%s:%d", host, conf.ExternalServices.Tracing.GrpcPort)
				clientConn, err := grpc.NewClient(grpcAddress, dialOps...)
				if err != nil {
					zl.Error().Msgf("Error creating gRPC client connection: %v", err)
					return nil, err
				}
				streamClient, err := tempo.NewgRPCClient(clientConn)
				if err != nil {
					zl.Error().Msgf("Error creating gRPC Tempo Client: %v", err)
					return nil, err
				}
				return &Client{baseURL: u, conn: clientConn, customHeaders: cfgTracing.CustomHeaders, grpcClient: streamClient, httpClient: client, httpTracingClient: httpTracingClient, provider: config.TempoProvider, transport: transportGRPC}, nil
			}
			return &Client{baseURL: u, customHeaders: cfgTracing.CustomHeaders, httpClient: client, httpTracingClient: httpTracingClient, provider: config.TempoProvider, transport: transportHTTP}, nil
		}
		zl.Debug().Msgf("Jaeger api_v3 HTTP base: [%s]", u.JoinPath("/api/v3").Redacted())
		httpTracingClient = jaeger.NewHTTPClient()
		return &Client{baseURL: u, customHeaders: cfgTracing.CustomHeaders, httpClient: client, httpTracingClient: httpTracingClient, provider: config.JaegerProvider, transport: transportHTTP}, nil
	}
}

// Close releases the gRPC connection the client dialled, if it dialled one. The client Kiali
// keeps runs for the process's lifetime and is never closed; a client built for one request, as
// /api/tracing/check builds one, must be.
func (in *Client) Close() error {
	if in == nil || in.conn == nil {
		return nil
	}
	return in.conn.Close()
}

// GetAppTraces fetches traces of an app
func (in *Client) GetAppTraces(ctx context.Context, namespace, app string, q models.TracingQuery) (*models.TracingResponse, error) {
	ctx = in.prepareContextForClient(ctx)

	serviceName := app

	// create a timer to time the tracing query. Note that we will always take the measurement even on failure
	promtimer := internalmetrics.GetTracingProcessingTimePrometheusTimer("AppTraces")
	defer internalmetrics.ObserveDurationAndLogResults(ctx, config.Get(), promtimer, "TracingProcessingTime", nil, "AppTraces")

	if in.transport == transportHTTP {
		return in.httpTracingClient.GetAppTracesHTTP(ctx, in.httpClient, in.baseURL, serviceName, q)
	}
	return in.grpcClient.FindTraces(ctx, serviceName, q)
}

// GetTraceDetail fetches a specific trace from its ID
func (in *Client) GetTraceDetail(ctx context.Context, strTraceID string) (*models.TracingSingleTrace, error) {
	ctx = in.prepareContextForClient(ctx)

	// create a timer to time the tracing query. Note that we will always take the measurement even on failure
	promtimer := internalmetrics.GetTracingProcessingTimePrometheusTimer("TraceDetail")
	defer internalmetrics.ObserveDurationAndLogResults(ctx, config.Get(), promtimer, "TracingProcessingTime", nil, "TraceDetail")

	// Tempo reads a single trace over HTTP even with use_grpc: its gRPC streaming API only
	// searches, so a Tempo client always carries an HTTP client for the detail.
	if in.transport == transportGRPC && in.provider != config.TempoProvider {
		return in.grpcClient.GetTrace(ctx, strTraceID)
	}
	if in.httpTracingClient == nil {
		return nil, fmt.Errorf("error getting trace details")
	}
	return in.httpTracingClient.GetTraceDetailHTTP(ctx, in.httpClient, in.baseURL, strTraceID)
}

// GetErrorTraces fetches number of traces in error for the given app
func (in *Client) GetErrorTraces(ctx context.Context, ns, app string, duration time.Duration) (int, error) {
	// Note: grpc vs http switch is performed in subsequent call 'GetAppTraces'
	now := time.Now()
	query := models.TracingQuery{
		Start: now.Add(-duration),
		End:   now,
		Tags:  map[string]string{"error": "true"},
	}
	for key, value := range config.Get().ExternalServices.Tracing.QueryScope {
		query.Tags[key] = value
	}

	// create a timer to time the tracing query. Note that we will always take the measurement even on failure
	promtimer := internalmetrics.GetTracingProcessingTimePrometheusTimer("ErrorTraces")
	defer internalmetrics.ObserveDurationAndLogResults(ctx, config.Get(), promtimer, "TracingProcessingTime", nil, "ErrorTraces")

	traces, err := in.GetAppTraces(ctx, ns, app, query)
	if err != nil {
		return 0, err
	}
	return len(traces.Data), nil
}

func (in *Client) GetServiceStatus(ctx context.Context) (bool, error) {
	ctx = in.prepareContextForClient(ctx)

	// create a timer to time the tracing query. Note that we will always take the measurement even on failure
	promtimer := internalmetrics.GetTracingProcessingTimePrometheusTimer("ServiceStatus")
	defer internalmetrics.ObserveDurationAndLogResults(ctx, config.Get(), promtimer, "TracingProcessingTime", nil, "ServiceStatus")

	// Check Service Status using HTTP when gRPC is not enabled
	if in.transport == transportHTTP {
		return in.httpTracingClient.GetServiceStatusHTTP(ctx, in.httpClient, in.baseURL)
	}

	return in.grpcClient.GetServices(ctx)
}

func (in *Client) GetServices(ctx context.Context) ([]string, error) {
	ctx = in.prepareContextForClient(ctx)

	// create a timer to time the tracing query. Note that we will always take the measurement even on failure
	promtimer := internalmetrics.GetTracingProcessingTimePrometheusTimer("ServiceStatus")
	defer internalmetrics.ObserveDurationAndLogResults(ctx, config.Get(), promtimer, "TracingProcessingTime", nil, "ServiceStatus")

	// Check Service Status using HTTP when gRPC is not enabled
	if in.transport == transportHTTP {
		return in.httpTracingClient.GetServices(ctx, in.httpClient, in.baseURL)
	}

	return in.grpcClient.GetServicesList(ctx)
}

// prepareContextForClient puts things in the given context that will be needed by the client to do its job.
// For example, the custom headers are added to the context so the clients pass them on to the server when making requests.
func (in *Client) prepareContextForClient(ctx context.Context) context.Context {
	if in != nil && len(in.customHeaders) > 0 {
		log.FromContext(ctx).Trace().Msgf("Adding [%v] custom headers to Tracing client", len(in.customHeaders))
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(in.customHeaders))
	}
	return ctx
}
