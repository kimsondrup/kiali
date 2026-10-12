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
// ends the stream or fails it. That is all a test needs to drive the client over a stream of
// several messages, over an empty one, and over one that stops partway.
type scriptedQuerier struct {
	tempopb.UnimplementedStreamingQuerierServer

	failWith error
	messages []*tempopb.SearchResponse
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
	return dialerTo(func(server *grpc.Server) {
		tempopb.RegisterStreamingQuerierServer(server, querier)
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
func searchStream(t *testing.T, querier tempopb.StreamingQuerierServer) tempopb.StreamingQuerier_SearchClient {
	t.Helper()

	stream, err := clientOver(t, dialer(querier)).StreamingClient.Search(context.Background(), &tempopb.SearchRequest{})
	require.NoError(t, err)
	return stream
}

// searchResponse builds one stream message out of the traces it is given.
func searchResponse(traces ...*tempopb.TraceSearchMetadata) *tempopb.SearchResponse {
	return &tempopb.SearchResponse{Traces: traces}
}

// searchedTrace builds a search result for one trace, carrying the given number of spans.
func searchedTrace(traceID string, spans int) *tempopb.TraceSearchMetadata {
	spanSet := &tempopb.SpanSet{Matched: uint32(spans)}
	for i := 0; i < spans; i++ {
		spanSet.Spans = append(spanSet.Spans, &tempopb.Span{
			SpanID:            traceID + string(rune('a'+i)),
			Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
			StartTimeUnixNano: 1693389472310270000,
			DurationNanos:     646000,
		})
	}
	return &tempopb.TraceSearchMetadata{
		TraceID:         traceID,
		RootServiceName: "reviews.bookinfo",
		RootTraceName:   "reviews.bookinfo.svc.cluster.local:9080/*",
		SpanSet:         spanSet,
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
			dial: dialer(&scriptedQuerier{messages: []*tempopb.SearchResponse{
				searchResponse(searchedTrace("02e299711ce47710289dc6640727404f", 1)),
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

// TestProcessStreamAccumulatesDisjointMessages covers the framing Tempo's StreamingQuerier.Search
// uses. A response too large for one message is split by partitioning the trace list, so every
// message carries traces the others do not and all of them belong in the result.
func TestProcessStreamAccumulatesDisjointMessages(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{messages: []*tempopb.SearchResponse{
		searchResponse(searchedTrace("02e299711ce47710289dc6640727404f", 1)),
		searchResponse(searchedTrace("1c6e4bf1d1c0ac3d91f9b2e5a0d7c418", 1)),
		searchResponse(searchedTrace("3f7a90b4c5d6e7f801928374655aa0bb", 1)),
	}})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.NoError(t, err)
	require.Len(t, traces, 3)
	assert.Equal(t, "02e299711ce47710289dc6640727404f", string(traces[0].TraceID))
	assert.Equal(t, "1c6e4bf1d1c0ac3d91f9b2e5a0d7c418", string(traces[1].TraceID))
	assert.Equal(t, "3f7a90b4c5d6e7f801928374655aa0bb", string(traces[2].TraceID))
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
// received are the beginning of an answer rather than an answer, so the error is what reaches the
// caller.
func TestProcessStreamReportsAStreamError(t *testing.T) {
	stream := searchStream(t, &scriptedQuerier{
		messages: []*tempopb.SearchResponse{searchResponse(searchedTrace("02e299711ce47710289dc6640727404f", 1))},
		failWith: status.Error(codes.Internal, "block scheduler unavailable"),
	})

	traces, err := processStream(context.Background(), stream, "reviews.default")
	require.Error(t, err)
	assert.Nil(t, traces)
	assert.Contains(t, err.Error(), "block scheduler unavailable")
}
