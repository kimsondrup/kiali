package tempo

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiali/kiali/tracing/tempo/tempopb"
)

// fakeSearchStream replays a prepared sequence of search responses. An error set on it is
// returned in place of the message at errAt; past the last message the stream ends with io.EOF,
// which is how a gRPC stream reports that the server is done sending.
type fakeSearchStream struct {
	grpc.ClientStream
	responses []*tempopb.SearchResponse
	err       error
	errAt     int
	next      int
}

func (f *fakeSearchStream) Recv() (*tempopb.SearchResponse, error) {
	i := f.next
	f.next++
	if f.err != nil && i == f.errAt {
		return nil, f.err
	}
	if i >= len(f.responses) {
		return nil, io.EOF
	}
	return f.responses[i], nil
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

// TestProcessStreamCollapsesSnapshots covers the shape of Tempo's streaming search: every message
// is a cumulative snapshot of everything the search has found so far, so the trace a message
// carries is not a new trace but a replacement for the one the previous message reported, with
// however many more spans have since been matched. One row per trace must come out, carrying the
// last snapshot's spans.
func TestProcessStreamCollapsesSnapshots(t *testing.T) {
	stream := &fakeSearchStream{responses: []*tempopb.SearchResponse{
		searchResponse(searchedTrace("aaaa", 1)),
		searchResponse(searchedTrace("aaaa", 2), searchedTrace("bbbb", 1)),
		searchResponse(searchedTrace("aaaa", 3), searchedTrace("bbbb", 2), searchedTrace("cccc", 1)),
	}}

	traces, err := processStream(context.Background(), stream, "reviews.bookinfo")
	require.NoError(t, err)
	require.Len(t, traces, 3)

	ids := []string{}
	for _, trace := range traces {
		ids = append(ids, string(trace.TraceID))
	}
	assert.Equal(t, []string{"aaaa", "bbbb", "cccc"}, ids)
	assert.Len(t, traces[0].Spans, 3, "the last snapshot of trace aaaa")
	assert.Len(t, traces[1].Spans, 2, "the last snapshot of trace bbbb")
	assert.Len(t, traces[2].Spans, 1, "the last snapshot of trace cccc")
}

// TestProcessStreamReportsATimeout covers a search that runs out of time part way through its
// stream. What arrived before the deadline is a truncated result set and not the answer to the
// query, so returning it on its own leaves the caller unable to tell the two apart.
func TestProcessStreamReportsATimeout(t *testing.T) {
	stream := &fakeSearchStream{
		responses: []*tempopb.SearchResponse{searchResponse(searchedTrace("aaaa", 1))},
		err:       status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		errAt:     1,
	}

	_, err := processStream(context.Background(), stream, "reviews.bookinfo")
	require.Error(t, err)
	assert.ErrorContains(t, err, "timed out")
}

// TestProcessServicesReportsATimeout covers the same deadline on the stream that the service list
// reads.
func TestProcessServicesReportsATimeout(t *testing.T) {
	stream := &fakeSearchStream{
		responses: []*tempopb.SearchResponse{searchResponse(searchedTrace("aaaa", 1))},
		err:       status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		errAt:     1,
	}

	_, err := processServices(context.Background(), stream)
	require.Error(t, err)
	assert.ErrorContains(t, err, "timed out")
}
