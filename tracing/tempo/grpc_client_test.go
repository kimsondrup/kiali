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
	"github.com/kiali/kiali/tracing/tempo/tempopb"
)

// scriptedQuerier is a StreamingQuerier that sends the messages it was given, in order, and then
// ends the stream or fails it. That is the one thing a mock has to do here, because what the
// client gets wrong is how it treats a stream of more than one message.
type scriptedQuerier struct {
	messages []*tempopb.SearchResponse
	failWith error
}

func (s *scriptedQuerier) Search(req *tempopb.SearchRequest, srv tempopb.StreamingQuerier_SearchServer) error {
	for _, message := range s.messages {
		if err := srv.Send(message); err != nil {
			return err
		}
	}
	return s.failWith
}

func dialer(querier tempopb.StreamingQuerierServer) func(context.Context, string) (net.Conn, error) {
	listener := bufconn.Listen(1024 * 1024)

	server := grpc.NewServer()
	tempopb.RegisterStreamingQuerierServer(server, querier)

	go func() {
		if err := server.Serve(listener); err != nil {
			log.Fatal(err)
		}
	}()

	return func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
}

// searchStream opens a real gRPC stream against querier, so the client code under test reads the
// messages the way it reads Tempo's.
func searchStream(t *testing.T, querier tempopb.StreamingQuerierServer) tempopb.StreamingQuerier_SearchClient {
	t.Helper()

	clientConn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialer(querier)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientConn.Close() })

	client, err := NewgRPCClient(clientConn)
	require.NoError(t, err)
	require.NotNil(t, client)

	stream, err := client.StreamingClient.Search(context.Background(), &tempopb.SearchRequest{})
	require.NoError(t, err)
	return stream
}

// searchResponse builds one message of a search stream. Each trace is given the spans named, so a
// later snapshot of the same trace can be told from an earlier one by its span count.
func searchResponse(traces ...*tempopb.TraceSearchMetadata) *tempopb.SearchResponse {
	return &tempopb.SearchResponse{Traces: traces, Metrics: &tempopb.SearchMetrics{}}
}

func searchTrace(traceID string, spanIDs ...string) *tempopb.TraceSearchMetadata {
	spans := make([]*tempopb.Span, 0, len(spanIDs))
	for _, spanID := range spanIDs {
		spans = append(spans, &tempopb.Span{SpanID: spanID, StartTimeUnixNano: 1700000000000000000, DurationNanos: 1000})
	}
	return &tempopb.TraceSearchMetadata{
		TraceID:       traceID,
		RootTraceName: "reviews.default",
		SpanSet:       &tempopb.SpanSet{Spans: spans, Matched: uint32(len(spans))},
	}
}

// TestProcessStreamCountsACumulativeTraceOnce covers the framing Tempo's StreamingQuerier.Search
// actually uses. Each message repeats the traces the earlier ones carried, with whatever more it
// has found for them since, so a stream of [t1] and then [t1, t2] describes two traces and not
// three - and the later message is the one to keep, because it has the fuller span set.
func TestProcessStreamCountsACumulativeTraceOnce(t *testing.T) {
	first := searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936")
	second := searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936", "2e6ca056f5fc6fdc")
	other := searchTrace("1c6e4bf1d1c0ac3d91f9b2e5a0d7c418", "aa11bb22cc33dd44")

	stream := searchStream(t, &scriptedQuerier{messages: []*tempopb.SearchResponse{
		searchResponse(first),
		searchResponse(second, other),
	}})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.NoError(t, err)
	require.Len(t, traces, 2)

	assert.Equal(t, "02e299711ce47710289dc6640727404f", string(traces[0].TraceID))
	// The second snapshot of the trace won: it reports both of its matched spans.
	assert.Len(t, traces[0].Spans, 2)
	assert.Equal(t, "1c6e4bf1d1c0ac3d91f9b2e5a0d7c418", string(traces[1].TraceID))
	assert.Len(t, traces[1].Spans, 1)
}

// TestProcessStreamAccumulatesDisjointMessages covers the other framing a server-streaming search
// could use, where each message carries traces the others do not. Keeping the latest version of
// each trace ID accumulates these the same way appending did, so reading cumulative snapshots
// correctly does not cost anything here.
func TestProcessStreamAccumulatesDisjointMessages(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{messages: []*tempopb.SearchResponse{
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
// ID: the same trace can arrive spelled two ways across two snapshots, and the two spellings are
// one trace. The padded ID is what the rest of Kiali compares, so it is what counts here.
func TestProcessStreamCountsAnUnpaddedTraceOnce(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{messages: []*tempopb.SearchResponse{
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
	assert.NotNil(t, traces)
	assert.Empty(t, traces)
}

// TestProcessStreamReportsAStreamError covers a stream that fails partway. The traces already
// received are not an answer - they are the beginning of one - so they are not returned beside a
// nil error, which would read as a complete search that happened to match little.
func TestProcessStreamReportsAStreamError(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{
		messages: []*tempopb.SearchResponse{searchResponse(searchTrace("02e299711ce47710289dc6640727404f", "49cd77d1f9dcd936"))},
		failWith: status.Error(codes.Internal, "block scheduler unavailable"),
	})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.Error(t, err)
	assert.Nil(t, traces)
	assert.Contains(t, err.Error(), "block scheduler unavailable")
}
