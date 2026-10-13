package jaeger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
	"github.com/kiali/kiali/tracing/otel/otlp"
)

// grpcEndpoint names the endpoint in error messages. The client is handed a stub, not the
// connection, so it cannot name the address; the message names the service it asked for instead.
const grpcEndpoint = "the gRPC endpoint"

// MaxReceiveMessageSize is the largest api_v3 gRPC message the client accepts. api_v3 sends a
// whole trace in one message, where api_v2 split a trace into messages of ten spans, so gRPC's
// default limit of 4 MiB, which api_v2 never came near, refuses a trace of about 10000 Envoy
// spans, and FindTraces fails as a whole when one trace in the window is that large.
const MaxReceiveMessageSize = 64 << 20

// DialOptions are the options a connection to jaeger.api_v3.QueryService needs beyond its
// transport credentials. Every dialler of the service appends them, so that the limit is the same
// for the client and for discovery.
func DialOptions() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxReceiveMessageSize))}
}

// GRPCClient reads Jaeger api_v3 through the gRPC service jaeger.api_v3.QueryService. Jaeger
// serves it on the same port as the api_v2 service it replaces (16685 by default), so a URL that
// reached api_v2 reaches this too.
type GRPCClient struct {
	client  apiv3.QueryServiceClient
	timeout time.Duration
}

// NewGRPCClient returns the api_v3 gRPC transport. timeout bounds every call, and is the
// configured external_services.tracing.query_timeout; zero or less leaves the caller's context
// as the only bound.
func NewGRPCClient(client apiv3.QueryServiceClient, timeout time.Duration) *GRPCClient {
	return &GRPCClient{client: client, timeout: timeout}
}

// FindTraces searches a service's traces.
//
// The tag filter is left to the backend: every measured version (2.17.0, 2.20.0, 2.21.0) honours
// the attributes over gRPC. The HTTP transport filters again after converting only because 2.17.0's
// gateway ignores them; doing it here too could only drop traces the backend chose by a predicate
// that differs from Kiali's in the details.
func (c *GRPCClient) FindTraces(ctx context.Context, serviceName string, q models.TracingQuery) (*models.TracingResponse, error) {
	tq, err := newTraceQuery(serviceName, q)
	if err != nil {
		// A window nothing can match is an empty answer, and is given without a request.
		return searchResponse(serviceName, nil, err)
	}
	request := tq.proto()

	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	zl := getLoggerFromContextGRPCJaeger(ctx)
	zl.Debug().Msgf("Jaeger api_v3 gRPC FindTraces request: %v", request)

	stream, err := c.client.FindTraces(ctx, request)
	if err != nil {
		return nil, grpcError(err, errEmptyResult)
	}
	// FindTraces sends one trace per message (measured: 345 traces in 345 messages), but nothing
	// in the protocol keeps a trace's spans in one message, so every message is accumulated and
	// the converter groups the spans by trace ID. ReadStream also refuses to call a stream cut
	// short by the deadline complete, so a timeout is an error rather than a partial answer.
	traces, err := otlp.ReadStream(ctx, stream)
	return searchResponse(serviceName, traces, grpcError(err, errEmptyResult))
}

// GetTrace reads one trace. A trace the store does not hold is (nil, nil), which the handler
// reports as a 404. Jaeger says so in three ways over gRPC: 1.54 to 1.64 and 2.0 to 2.1 answer
// codes.Unknown with oldTraceNotFound, 1.65 and 2.2 to 2.20 answer OK with an empty stream, and
// 2.21 answers codes.NotFound. All three arrive at the same nil here, as does the HTTP
// transport's 404, so the browser gets the same answer whichever transport is configured.
func (c *GRPCClient) GetTrace(ctx context.Context, traceID string) (*models.TracingSingleTrace, error) {
	request, err := getTraceRequest(traceID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	stream, err := c.client.GetTrace(ctx, request)
	if err != nil {
		return traceResponse(nil, getTraceError(err))
	}
	traces, err := otlp.ReadStream(ctx, stream)
	return traceResponse(traces, getTraceError(err))
}

// oldTraceNotFound is the whole status text with which Jaeger 1.54 to 1.64 and 2.0 to 2.1 answer
// GetTrace for a trace the store does not hold: the handler wraps the store's
// spanstore.ErrTraceNotFound as "cannot retrieve trace: %w" and returns it as a plain error,
// which gRPC sends as codes.Unknown. The HTTP gateway of the same versions answers that with a
// 404, so read as an outage it would make the two transports disagree.
const oldTraceNotFound = "cannot retrieve trace: trace not found"

// getTraceError is grpcError for GetTrace, which alone has the old versions' Unknown answer for
// an unknown trace. It matches the status text exactly, so that any other Unknown, a store that
// failed included ("cannot retrieve trace: " and its own error), still reaches the user.
func getTraceError(err error) error {
	// ReadStream wraps the status error, and status.FromError would then report the wrapped
	// text, so the status is read from the error that carries it.
	var carrier interface{ GRPCStatus() *status.Status }
	if errors.As(err, &carrier) {
		if s := carrier.GRPCStatus(); s.Code() == codes.Unknown && s.Message() == oldTraceNotFound {
			return errNotFound
		}
	}
	return grpcError(err, errNotFound)
}

// GetServicesList lists the services the store holds spans for.
func (c *GRPCClient) GetServicesList(ctx context.Context) ([]string, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	response, err := c.client.GetServices(ctx, &apiv3.GetServicesRequest{})
	if err != nil {
		// GetServices has no "nothing" answer, so no status is read as one.
		return nil, grpcError(err, nil)
	}
	return response.GetServices(), nil
}

// GetServices is the health probe: it asks api_v3 for its services, so a backend that answers
// but does not serve api_v3 fails here with a message saying so, instead of passing a probe and
// then failing every search.
func (c *GRPCClient) GetServices(ctx context.Context) (bool, error) {
	if _, err := c.GetServicesList(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// withTimeout bounds one call by the configured query timeout. A caller's earlier deadline still
// wins, because a derived context never outlives its parent.
func (c *GRPCClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// grpcError adds to classifyGRPCError the two failures whose status text does not say what is
// wrong. nothing is the sentinel codes.NotFound stands for; nil means the call has none.
//
// codes.Unknown gets no special case here: Jaeger answers a malformed trace ID and a refused
// search depth with it, so it is reported as the backend's own message rather than as an outage.
// The one Unknown that means nothing is GetTrace's on old versions, which getTraceError reads.
func grpcError(err error, nothing error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.Unimplemented:
		// What a Jaeger older than 1.54 answers, and what Tempo's gRPC port answers.
		return fmt.Errorf("%s does not serve jaeger.api_v3.QueryService (Jaeger 1.54 or newer is required, or the port points at another service): %w", grpcEndpoint, err)
	case codes.Unavailable:
		// gRPC dialled at an HTTP port reads the HTTP/1.1 answer as an HTTP/2 frame header. The
		// message names api_v3 over gRPC, as the others here do, so that the check and the
		// traces page say which client was pointed at the port.
		if msg := err.Error(); strings.Contains(msg, "server preface") || strings.Contains(msg, "frame too large") {
			return fmt.Errorf("Jaeger api_v3 gRPC error: %w", wrongTransportError(grpcEndpoint, err))
		}
	case codes.ResourceExhausted:
		// A message above MaxReceiveMessageSize, which over gRPC is one trace that large.
		if strings.Contains(err.Error(), "larger than max") {
			return fmt.Errorf("Jaeger api_v3 gRPC error: a trace is larger than the %d MiB Kiali reads in one gRPC message: %w", MaxReceiveMessageSize>>20, err)
		}
	case codes.NotFound:
		if nothing == nil {
			return fmt.Errorf("Jaeger api_v3 gRPC error: %w", err)
		}
	}
	return classifyGRPCError(err, nothing)
}
