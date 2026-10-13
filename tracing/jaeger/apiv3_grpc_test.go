package jaeger

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
)

// Captured live from Jaeger 2.21.0 over jaeger.api_v3.QueryService/GetTrace and rendered as
// OTLP/JSON by grpcurl: one trace of three spans. tracing/tracingtest/README.adoc records how.
const (
	apiv3TraceGRPC        = "../tracingtest/apiv3_trace_grpc.json"
	apiv3TraceGRPCTraceID = "2132caa05ca64c82454600288b27f3b4"
)

// fakeQueryService is a scripted jaeger.api_v3.QueryService. Each test sets the answers it needs;
// a method left unset answers Unimplemented, as a server without it would. It records the
// requests it receives, so a test can check what went on the wire.
type fakeQueryService struct {
	apiv3.UnimplementedQueryServiceServer

	findTraces  func(*apiv3.FindTracesRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error
	getServices func(context.Context, *apiv3.GetServicesRequest) (*apiv3.GetServicesResponse, error)
	getTrace    func(*apiv3.GetTraceRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error
	mu          sync.Mutex
	received    []proto.Message
}

func (f *fakeQueryService) record(m proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, m)
}

func (f *fakeQueryService) requests() []proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.Message(nil), f.received...)
}

func (f *fakeQueryService) FindTraces(r *apiv3.FindTracesRequest, s grpc.ServerStreamingServer[tracepb.TracesData]) error {
	f.record(r)
	if f.findTraces == nil {
		return f.UnimplementedQueryServiceServer.FindTraces(r, s)
	}
	return f.findTraces(r, s)
}

func (f *fakeQueryService) GetServices(ctx context.Context, r *apiv3.GetServicesRequest) (*apiv3.GetServicesResponse, error) {
	f.record(r)
	if f.getServices == nil {
		return f.UnimplementedQueryServiceServer.GetServices(ctx, r)
	}
	return f.getServices(ctx, r)
}

func (f *fakeQueryService) GetTrace(r *apiv3.GetTraceRequest, s grpc.ServerStreamingServer[tracepb.TracesData]) error {
	f.record(r)
	if f.getTrace == nil {
		return f.UnimplementedQueryServiceServer.GetTrace(r, s)
	}
	return f.getTrace(r, s)
}

// dialFake serves the fake in process and returns a client to it, dialled with the options
// production dials with.
func dialFake(t *testing.T, fake *fakeQueryService, timeout time.Duration) *GRPCClient {
	t.Helper()
	return dialFakeWith(t, fake, timeout, DialOptions()...)
}

// dialFakeWith is dialFake with the dial options given instead of production's.
func dialFakeWith(t *testing.T, fake *fakeQueryService, timeout time.Duration, opts ...grpc.DialOption) *GRPCClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	apiv3.RegisterQueryServiceServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn", append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return NewGRPCClient(apiv3.NewQueryServiceClient(conn), timeout)
}

// grpcFixture reads the captured GetTrace message. grpcurl renders it the way protojson does, so
// it unmarshals straight into the message the server sends.
func grpcFixture(t *testing.T) *tracepb.TracesData {
	t.Helper()
	traces := &tracepb.TracesData{}
	require.NoError(t, protojson.Unmarshal(readFixture(t, apiv3TraceGRPC), traces))
	require.NotEmpty(t, traces.GetResourceSpans())
	return traces
}

// oneSpanTrace is a message holding a single span of the trace whose ID ends in last.
func oneSpanTrace(last, span byte) *tracepb.TracesData {
	traceID := make([]byte, 16)
	traceID[15] = last
	spanID := make([]byte, 8)
	spanID[7] = span
	return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			EndTimeUnixNano:   2_000,
			Name:              "span",
			SpanId:            spanID,
			StartTimeUnixNano: 1_000,
			TraceId:           traceID,
		}}}},
	}}}
}

func sendAll(messages ...*tracepb.TracesData) func(*apiv3.FindTracesRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error {
	return func(_ *apiv3.FindTracesRequest, s grpc.ServerStreamingServer[tracepb.TracesData]) error {
		for _, m := range messages {
			if err := s.Send(m); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestGRPCFindTracesReadsEveryMessage(t *testing.T) {
	client := dialFake(t, &fakeQueryService{findTraces: sendAll(oneSpanTrace(1, 1), oneSpanTrace(2, 1), oneSpanTrace(3, 1))}, time.Minute)

	response, err := client.FindTraces(context.Background(), "waypoint.tracegen", searchQuery())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"00000000000000000000000000000001",
		"00000000000000000000000000000002",
		"00000000000000000000000000000003",
	}, traceIDs(response.Data))
}

// Nothing in the protocol keeps a trace's spans in one message.
func TestGRPCFindTracesJoinsATraceSplitAcrossMessages(t *testing.T) {
	client := dialFake(t, &fakeQueryService{findTraces: sendAll(oneSpanTrace(7, 1), oneSpanTrace(7, 2))}, time.Minute)

	response, err := client.FindTraces(context.Background(), "waypoint.tracegen", searchQuery())
	require.NoError(t, err)
	require.Len(t, response.Data, 1)
	assert.Len(t, response.Data[0].Spans, 2)
}

// An unmatched search is OK with no message on every version.
func TestGRPCFindTracesOfNothing(t *testing.T) {
	client := dialFake(t, &fakeQueryService{findTraces: sendAll()}, time.Minute)

	response, err := client.FindTraces(context.Background(), "waypoint.tracegen", searchQuery())
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.NotNil(t, response.Data)
	assert.Empty(t, response.Data)
	assert.Equal(t, "waypoint.tracegen", response.TracingServiceName)
}

func TestGRPCFindTracesRequest(t *testing.T) {
	fake := &fakeQueryService{findTraces: sendAll()}
	client := dialFake(t, fake, time.Minute)

	q := searchQuery()
	q.Start = time.Date(2026, 10, 13, 0, 56, 17, 123_456_789, time.UTC)
	q.End = time.Date(2026, 10, 13, 0, 56, 18, 987_654_321, time.UTC)
	q.Limit = 0 // what GetErrorTraces sends
	q.MinDuration = 1500 * time.Microsecond
	q.Tags = map[string]string{"error": "true", "istio.cluster_id": "Kubernetes"}

	_, err := client.FindTraces(context.Background(), "waypoint.tracegen", q)
	require.NoError(t, err)

	received := fake.requests()
	require.Len(t, received, 1)
	query := received[0].(*apiv3.FindTracesRequest).GetQuery()
	// Jaeger 2.17 and 2.20 refuse a zero depth with codes.Unknown.
	assert.Equal(t, int32(100), query.GetSearchDepth())
	assert.Equal(t, "waypoint.tracegen", query.GetServiceName())
	assert.Equal(t, q.Tags, query.GetAttributes())
	assert.Equal(t, q.Start, query.GetStartTimeMin().AsTime())
	assert.Equal(t, q.End, query.GetStartTimeMax().AsTime())
	assert.Equal(t, q.MinDuration, query.GetDurationMin().AsDuration())
}

// A search cut short by its deadline is an error, never the part that arrived in time.
func TestGRPCFindTracesPastTheDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	fake := &fakeQueryService{findTraces: func(_ *apiv3.FindTracesRequest, s grpc.ServerStreamingServer[tracepb.TracesData]) error {
		if err := s.Send(oneSpanTrace(1, 1)); err != nil {
			return err
		}
		select {
		case <-release:
		case <-s.Context().Done():
		}
		return nil
	}}

	for name, tc := range map[string]struct {
		callerDeadline time.Duration
		queryTimeout   time.Duration
	}{
		"the caller's deadline": {callerDeadline: 50 * time.Millisecond, queryTimeout: time.Minute},
		"the query timeout":     {queryTimeout: 50 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			client := dialFake(t, fake, tc.queryTimeout)
			ctx := context.Background()
			if tc.callerDeadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.callerDeadline)
				defer cancel()
			}
			response, err := client.FindTraces(ctx, "waypoint.tracegen", searchQuery())
			require.Error(t, err)
			assert.Nil(t, response)
			assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
		})
	}
}

func TestGRPCTraceDetail(t *testing.T) {
	fixture := grpcFixture(t)
	fake := &fakeQueryService{getTrace: func(_ *apiv3.GetTraceRequest, s grpc.ServerStreamingServer[tracepb.TracesData]) error {
		return s.Send(fixture)
	}}
	client := dialFake(t, fake, time.Minute)

	trace, err := client.GetTrace(context.Background(), apiv3TraceGRPCTraceID)
	require.NoError(t, err)
	require.NotNil(t, trace)
	assert.Equal(t, apiv3TraceGRPCTraceID, string(trace.Data.TraceID))
	assert.Len(t, trace.Data.Spans, 3)

	received := fake.requests()
	require.Len(t, received, 1)
	// api_v3 takes the ID as hex text over gRPC too.
	assert.Equal(t, apiv3TraceGRPCTraceID, received[0].(*apiv3.GetTraceRequest).GetTraceId())
}

// Jaeger 2.21 answers an unknown trace with NotFound, 1.65 and 2.2 to 2.20 with OK and no
// message, and 1.54 to 1.64 and 2.0 to 2.1 with Unknown and the store's "trace not found". All
// three are (nil, nil), which the handler reports as a 404, as the HTTP transport's 404 is.
func TestGRPCTraceDetailOfAnUnknownTrace(t *testing.T) {
	for name, getTrace := range map[string]func(*apiv3.GetTraceRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error{
		"NotFound (2.21)": func(*apiv3.GetTraceRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error {
			return status.Error(codes.NotFound, "trace not found")
		},
		"an empty stream (1.65, 2.2 to 2.20)": func(*apiv3.GetTraceRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error { return nil },
		"Unknown (1.54 to 1.64, 2.0 to 2.1)": func(*apiv3.GetTraceRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error {
			// What those versions' handler returns: a plain error, which gRPC sends as Unknown.
			return fmt.Errorf("cannot retrieve trace: %w", errors.New("trace not found"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := dialFake(t, &fakeQueryService{getTrace: getTrace}, time.Minute)
			trace, err := client.GetTrace(context.Background(), "0123456789abcdef0123456789abcdef")
			require.NoError(t, err)
			assert.Nil(t, trace)
		})
	}
}

// Only the old versions' exact answer for an unknown trace is read as one. A store that fails
// under the same handler, or the same text under another code, is still a failure carrying its
// message, so a broken backend is never shown as a trace that does not exist.
func TestGRPCTraceDetailUnknownFailureIsAnError(t *testing.T) {
	for name, answer := range map[string]error{
		"a failed store":                  errors.New("cannot retrieve trace: context deadline exceeded"),
		"the store's text alone":          errors.New("trace not found"),
		"the old text with more after it": errors.New("cannot retrieve trace: trace not found in archive"),
		"the old text under Internal":     status.Error(codes.Internal, "cannot retrieve trace: trace not found"),
	} {
		t.Run(name, func(t *testing.T) {
			client := dialFake(t, &fakeQueryService{getTrace: func(*apiv3.GetTraceRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error {
				return answer
			}}, time.Minute)
			trace, err := client.GetTrace(context.Background(), "0123456789abcdef0123456789abcdef")
			require.Error(t, err)
			assert.Nil(t, trace)
			assert.Contains(t, err.Error(), status.Convert(answer).Message())
		})
	}
}

// Jaeger answers a 33-character ID with codes.Unknown, which reads as a backend failure.
func TestGRPCTraceDetailRefusesAMalformedID(t *testing.T) {
	fake := &fakeQueryService{}
	client := dialFake(t, fake, time.Minute)

	for _, id := range []string{"0123456789abcdef0123456789abcdef0", "not-hex", ""} {
		trace, err := client.GetTrace(context.Background(), id)
		require.Error(t, err, id)
		assert.Nil(t, trace)
	}
	assert.Empty(t, fake.requests())
}

func TestGRPCSearchGuardsSendNothing(t *testing.T) {
	fake := &fakeQueryService{}
	client := dialFake(t, fake, time.Minute)

	inverted := searchQuery()
	inverted.Start, inverted.End = inverted.End, inverted.Start

	_, err := client.FindTraces(context.Background(), "", searchQuery())
	require.Error(t, err)
	_, err = client.FindTraces(context.Background(), "waypoint.tracegen", models.TracingQuery{})
	require.Error(t, err)
	_, err = client.FindTraces(context.Background(), "waypoint.tracegen", inverted)
	require.Error(t, err)
	assert.Empty(t, fake.requests())
}

func TestGRPCServices(t *testing.T) {
	client := dialFake(t, &fakeQueryService{getServices: func(context.Context, *apiv3.GetServicesRequest) (*apiv3.GetServicesResponse, error) {
		return &apiv3.GetServicesResponse{Services: []string{"jaeger", "waypoint.tracegen"}}, nil
	}}, time.Minute)

	services, err := client.GetServicesList(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"jaeger", "waypoint.tracegen"}, services)

	ok, err := client.GetServices(context.Background())
	require.NoError(t, err)
	assert.True(t, ok)
}

// A Jaeger older than 1.54, and Tempo's gRPC port, do not serve jaeger.api_v3.QueryService.
func TestGRPCNotAPIv3(t *testing.T) {
	client := dialFake(t, &fakeQueryService{}, time.Minute)

	ok, err := client.GetServices(context.Background())
	assert.False(t, ok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_v3")
	assert.Contains(t, err.Error(), "1.54")
	assert.Equal(t, codes.Unimplemented, status.Code(err))

	_, err = client.FindTraces(context.Background(), "waypoint.tracegen", searchQuery())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1.54")

	_, err = client.GetTrace(context.Background(), apiv3TraceGRPCTraceID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1.54")
}

// Jaeger answers a malformed ID or a refused depth with codes.Unknown; its message is the only
// thing that says which, so it reaches the user, and it is not mistaken for "nothing".
func TestGRPCBackendFailureCarriesItsMessage(t *testing.T) {
	const message = "search depth must be greater than 0 and less than max traces"
	client := dialFake(t, &fakeQueryService{
		findTraces: func(*apiv3.FindTracesRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error {
			return status.Error(codes.Unknown, message)
		},
		getServices: func(context.Context, *apiv3.GetServicesRequest) (*apiv3.GetServicesResponse, error) {
			return nil, status.Error(codes.Unknown, message)
		},
		getTrace: func(*apiv3.GetTraceRequest, grpc.ServerStreamingServer[tracepb.TracesData]) error {
			return status.Error(codes.Unknown, message)
		},
	}, time.Minute)

	_, err := client.GetServices(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), message)

	response, err := client.FindTraces(context.Background(), "waypoint.tracegen", searchQuery())
	require.Error(t, err)
	assert.Nil(t, response)
	assert.Contains(t, err.Error(), message)

	trace, err := client.GetTrace(context.Background(), apiv3TraceGRPCTraceID)
	require.Error(t, err)
	assert.Nil(t, trace)
	assert.Contains(t, err.Error(), message)
}

// The services route has no "nothing" to report, so a NotFound there is an error.
func TestGRPCServicesNotFoundIsAnError(t *testing.T) {
	client := dialFake(t, &fakeQueryService{getServices: func(context.Context, *apiv3.GetServicesRequest) (*apiv3.GetServicesResponse, error) {
		return nil, status.Error(codes.NotFound, "no such thing")
	}}, time.Minute)

	_, err := client.GetServicesList(context.Background())
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func dialAddress(t *testing.T, addr string) *GRPCClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return NewGRPCClient(apiv3.NewQueryServiceClient(conn), 5*time.Second)
}

// A dial error keeps its own text.
func TestGRPCConnectionRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	listener.Close()

	_, err = dialAddress(t, addr).GetServices(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	assert.NotContains(t, err.Error(), "wrong port or transport")
}

// gRPC dialled at an HTTP port reads the HTTP/1.1 answer as a frame header.
func TestGRPCToAnHTTPPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	_, err := dialAddress(t, server.Listener.Addr().String()).GetServices(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Jaeger api_v3 gRPC error: the gRPC endpoint answered in another protocol, which means the wrong port or transport")
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

// bigTrace is one trace of n spans in a single message, the way api_v3 sends a trace.
func bigTrace(n int) *tracepb.TracesData {
	traceID := make([]byte, 16)
	traceID[15] = 1
	spans := make([]*tracepb.Span, 0, n)
	for i := range n {
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
	return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
}

// api_v3 sends a whole trace in one message, so a trace of 10000 spans is above gRPC's default
// receive limit of 4 MiB. With the options production dials with, both calls read it.
func TestGRPCReadsATraceAboveTheDefaultMessageLimit(t *testing.T) {
	trace := bigTrace(10000)
	require.Greater(t, proto.Size(trace), 4<<20, "the trace must be above gRPC's default limit to test anything")
	send := func(s grpc.ServerStreamingServer[tracepb.TracesData]) error { return s.Send(trace) }
	fake := &fakeQueryService{
		findTraces: func(_ *apiv3.FindTracesRequest, s grpc.ServerStreamingServer[tracepb.TracesData]) error {
			return send(s)
		},
		getTrace: func(_ *apiv3.GetTraceRequest, s grpc.ServerStreamingServer[tracepb.TracesData]) error { return send(s) },
	}

	client := dialFake(t, fake, time.Minute)
	detail, err := client.GetTrace(context.Background(), "00000000000000000000000000000001")
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Len(t, detail.Data.Spans, 10000)

	response, err := client.FindTraces(context.Background(), "waypoint.tracegen", searchQuery())
	require.NoError(t, err)
	require.Len(t, response.Data, 1)
	assert.Len(t, response.Data[0].Spans, 10000)

	// Without them the trace is refused, and the message says why.
	defaults := dialFakeWith(t, fake, time.Minute)
	_, err = defaults.GetTrace(context.Background(), "00000000000000000000000000000001")
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Contains(t, err.Error(), "a trace is larger than the 64 MiB")
}
