package tempo

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/tracing/tempo/querypb"
)

// scriptedQuerier is a StreamingQuerier that sends the messages it was given, in order, and then
// ends the stream or fails it. That is all a test needs to drive the client over a stream of
// several messages, over an empty one, and over one that stops partway.
type scriptedQuerier struct {
	querypb.UnimplementedStreamingQuerierServer

	failWith error
	messages []*querypb.SearchResponse
}

func (s *scriptedQuerier) Search(req *querypb.SearchRequest, srv querypb.StreamingQuerier_SearchServer) error {
	for _, message := range s.messages {
		if err := srv.Send(message); err != nil {
			return err
		}
	}
	return s.failWith
}

func dialer(querier querypb.StreamingQuerierServer) func(context.Context, string) (net.Conn, error) {
	return dialerTo(func(server *grpc.Server) {
		querypb.RegisterStreamingQuerierServer(server, querier)
	})
}

// dialerTo serves whatever register registers, which for the empty register is nothing at all -
// the backend that answers Unimplemented because it has never heard of StreamingQuerier.
func dialerTo(register func(*grpc.Server)) func(context.Context, string) (net.Conn, error) {
	listener := bufconn.Listen(1024 * 1024)

	server := grpc.NewServer()
	register(server)

	go func() {
		if err := server.Serve(listener); err != nil {
			log.Fatal(err)
		}
	}()

	return func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
}

// clientOver dials a real gRPC connection over d, so the client code under test speaks the
// protocol it speaks to Tempo.
func clientOver(t *testing.T, d func(context.Context, string) (net.Conn, error)) *TempoGRPCClient {
	t.Helper()

	clientConn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(d))
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientConn.Close() })

	client, err := NewgRPCClient(clientConn)
	require.NoError(t, err)
	require.NotNil(t, client)
	return client
}

// searchStream opens a real gRPC stream against querier, so the client code under test reads the
// messages the way it reads Tempo's.
func searchStream(t *testing.T, querier querypb.StreamingQuerierServer) querypb.StreamingQuerier_SearchClient {
	t.Helper()

	stream, err := clientOver(t, dialer(querier)).StreamingClient.Search(context.Background(), &querypb.SearchRequest{})
	require.NoError(t, err)
	return stream
}

// searchResponse builds one message of a search stream. Each trace is given the spans named, so a
// later record of the same trace can be told from an earlier one by its span count.
func searchResponse(traces ...*querypb.TraceSearchMetadata) *querypb.SearchResponse {
	return &querypb.SearchResponse{Traces: traces, Metrics: &querypb.SearchMetrics{}}
}

func searchTrace(traceID string, spanIDs ...string) *querypb.TraceSearchMetadata {
	spans := make([]*querypb.Span, 0, len(spanIDs))
	for _, spanID := range spanIDs {
		spans = append(spans, &querypb.Span{SpanID: spanID, StartTimeUnixNano: 1700000000000000000, DurationNanos: 1000})
	}
	return &querypb.TraceSearchMetadata{
		TraceID:       traceID,
		RootTraceName: "reviews.default",
		SpanSet:       &querypb.SpanSet{Spans: spans, Matched: uint32(len(spans))},
	}
}

// TestGetServicesJudgesTheBackendOnWhatItReceives covers the tracing health check. Opening the
// stream proves only that something is listening, because gRPC resolves the method name when the
// first message travels, so a backend serving no streaming search - which every Tempo before
// 2.2.0 is - is only distinguishable once the stream is read. A stream that ends straight away is
// a healthy, empty answer.
func TestGetServicesJudgesTheBackendOnWhatItReceives(t *testing.T) {
	cases := map[string]struct {
		dial       func(context.Context, string) (net.Conn, error)
		expectedOk bool
	}{
		"a backend that answers with traces": {
			dial: dialer(&scriptedQuerier{messages: []*querypb.SearchResponse{
				searchResponse(searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936")),
			}}),
			expectedOk: true,
		},
		"a backend that answers with nothing": {
			dial:       dialer(&scriptedQuerier{}),
			expectedOk: true,
		},
		"a backend that does not serve the streaming querier": {
			dial:       dialerTo(func(*grpc.Server) {}),
			expectedOk: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := clientOver(t, tc.dial).GetServices(context.Background())

			assert.Equal(t, tc.expectedOk, ok)
			if tc.expectedOk {
				assert.NoError(t, err)
			} else {
				require.Error(t, err, "an unhealthy answer has to carry its reason")
				assert.Contains(t, err.Error(), "tempopb.StreamingQuerier")
			}
		})
	}
}

// TestProcessStreamReportsAnUpdatedTraceOnce covers a trace ID that repeats across the stream,
// which is a trace Tempo updated between two diffs - found again in a later block, say. The two
// records are one trace, and the later one is the one to keep, because it has the fuller span set.
func TestProcessStreamReportsAnUpdatedTraceOnce(t *testing.T) {
	first := searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936")
	second := searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936", "2e6ca056f5fc6fdc")
	other := searchTrace("1c6e4bf1d1c0ac3d91f9b2e5a0d7c418", "aa11bb22cc33dd44")

	stream := searchStream(t, &scriptedQuerier{messages: []*querypb.SearchResponse{
		searchResponse(first),
		searchResponse(second, other),
	}})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.NoError(t, err)
	require.Len(t, traces, 2)

	assert.Equal(t, "02e299711ce47710289dc6640727404f", string(traces[0].TraceID))
	// The updated record of the trace won: it reports both of its matched spans.
	assert.Len(t, traces[0].Spans, 2)
	assert.Equal(t, "1c6e4bf1d1c0ac3d91f9b2e5a0d7c418", string(traces[1].TraceID))
	assert.Len(t, traces[1].Spans, 1)
}

// TestProcessStreamAccumulatesDisjointMessages covers the framing Tempo uses for a response too
// large for one message, which partitions the trace list so each message carries traces the
// others do not. Keeping the latest version of each trace ID accumulates these the same way
// appending did, so collapsing a repeated trace costs nothing here.
func TestProcessStreamAccumulatesDisjointMessages(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{messages: []*querypb.SearchResponse{
		searchResponse(searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936")),
		searchResponse(searchTrace("1c6e4bf1d1c0ac3d91f9b2e5a0d7c418", "aa11bb22cc33dd44")),
		searchResponse(searchTrace("3f7a90b4c5d6e7f801928374655aa0bb", "bb22cc33dd44ee55")),
	}})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.NoError(t, err)
	require.Len(t, traces, 3)
	assert.Equal(t, "02e299711ce47710289dc6640727404f", string(traces[0].TraceID))
	assert.Equal(t, "1c6e4bf1d1c0ac3d91f9b2e5a0d7c418", string(traces[1].TraceID))
	assert.Equal(t, "3f7a90b4c5d6e7f801928374655aa0bb", string(traces[2].TraceID))
}

// TestProcessStreamCountsAnUnpaddedTraceOnce covers Tempo stripping the leading zeros of a trace
// ID: the same trace can arrive spelled two ways across two messages, and the two spellings are
// one trace. The padded ID is what the rest of Kiali compares, so it is what counts here.
func TestProcessStreamCountsAnUnpaddedTraceOnce(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{messages: []*querypb.SearchResponse{
		searchResponse(searchTrace("2e299711ce47710289dc6640727404f", "49cd77d1f9dcd936")),
		searchResponse(searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936", "2e6ca056f5fc6fdc")),
	}})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.NoError(t, err)
	require.Len(t, traces, 1)
	assert.Equal(t, "02e299711ce47710289dc6640727404f", string(traces[0].TraceID))
	assert.Len(t, traces[0].Spans, 2)
}

// TestProcessStreamEndsOnEOF covers a search that matched nothing: the server ends the stream
// without sending a message, which is not a failure and must not read as one.
func TestProcessStreamEndsOnEOF(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.NoError(t, err)
	// Not nil even when empty: a nil slice serialises as JSON null.
	assert.NotNil(t, traces)
	assert.Empty(t, traces)
}

// TestProcessStreamReportsAStreamError covers a stream that fails partway. The traces already
// received are not an answer - they are the beginning of one - so they are not returned beside a
// nil error, which would read as a complete search that happened to match little.
func TestProcessStreamReportsAStreamError(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{
		messages: []*querypb.SearchResponse{searchResponse(searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936"))},
		failWith: status.Error(codes.Internal, "block scheduler unavailable"),
	})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.Error(t, err)
	assert.Nil(t, traces)
	assert.Contains(t, err.Error(), "block scheduler unavailable")
}
