package otlp

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeStream answers a fixed sequence of messages and then one final error.
type fakeStream struct {
	messages []*tracepb.TracesData
	end      error
	calls    int
}

func (f *fakeStream) Recv() (*tracepb.TracesData, error) {
	f.calls++
	if len(f.messages) == 0 {
		return nil, f.end
	}
	message := f.messages[0]
	f.messages = f.messages[1:]
	return message, nil
}

// api_v3 FindTraces streams chunks, so the spans of one trace can arrive in
// several messages. A reader that treated a message as a trace would report the
// one trace below twice, each with a third of its spans.
func TestReadStreamMergesTheChunksOfOneTrace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	whole, err := Decode(fixture(t, apiv3TraceGRPC), Options{IDEncoding: Base64})
	require.NoError(err)
	require.Len(whole.GetResourceSpans(), 2)

	stream := &fakeStream{
		messages: []*tracepb.TracesData{
			{ResourceSpans: whole.GetResourceSpans()[:1]},
			{ResourceSpans: whole.GetResourceSpans()[1:]},
		},
		end: io.EOF,
	}

	traces, err := ReadStream(context.Background(), stream)
	require.NoError(err)
	assert.Len(traces.GetResourceSpans(), 2)
	assert.Len(spans(traces), len(spans(whole)))
	assert.Len(traceIDs(traces), 1)
}

func TestReadStreamKeepsSeparateTracesSeparate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	search, err := Decode(fixture(t, apiv3SearchHTTP), Options{RootKey: RootKeyResult, IDEncoding: Hex})
	require.NoError(err)
	require.Len(traceIDs(search), 3)

	var messages []*tracepb.TracesData
	for _, resourceSpans := range search.GetResourceSpans() {
		messages = append(messages, &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{resourceSpans}})
	}

	traces, err := ReadStream(context.Background(), &fakeStream{messages: messages, end: io.EOF})
	require.NoError(err)
	assert.Len(traceIDs(traces), 3)
	assert.Len(spans(traces), len(spans(search)))
}

func TestReadStreamOnAnEmptyStream(t *testing.T) {
	traces, err := ReadStream(context.Background(), &fakeStream{end: io.EOF})
	require.NoError(t, err)
	assert.Empty(t, traces.GetResourceSpans())
}

// Only io.EOF ends a stream cleanly. Anything else has to be reported: a partial
// result handed back with a nil error is indistinguishable from a complete one
// that happens to be short.
func TestReadStreamReportsAnUncleanEnd(t *testing.T) {
	cases := map[string]error{
		"deadline exceeded": status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		"cancelled":         status.Error(codes.Canceled, "context canceled"),
		"unavailable":       status.Error(codes.Unavailable, "connection refused"),
		"not found":         status.Error(codes.NotFound, "trace not found"),
	}

	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			whole, err := Decode(fixture(t, apiv3TraceGRPC), Options{IDEncoding: Base64})
			require.NoError(t, err)

			stream := &fakeStream{messages: []*tracepb.TracesData{whole}, end: end}
			traces, err := ReadStream(context.Background(), stream)
			require.Error(t, err)
			assert.Nil(t, traces)
			assert.Equal(t, status.Code(end), status.Code(err))
		})
	}
}

func TestReadStreamStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stream := &fakeStream{end: io.EOF}
	traces, err := ReadStream(ctx, stream)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, traces)
	assert.Zero(t, stream.calls)
}
