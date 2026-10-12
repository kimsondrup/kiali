package jaeger

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/kiali/kiali/tracing/jaeger/model"
)

// scriptedStream is a FindTraces stream that sends the chunks it was given, in order, and then
// ends with io.EOF, which is how a gRPC stream reports that the server is done sending.
type scriptedStream struct {
	grpc.ClientStream

	chunks []*model.SpansResponseChunk
}

func (s *scriptedStream) Recv() (*model.SpansResponseChunk, error) {
	if len(s.chunks) > 0 {
		chunk := s.chunks[0]
		s.chunks = s.chunks[1:]
		return chunk, nil
	}
	return nil, io.EOF
}

func chunk(traceID []byte, spanIDs ...[]byte) *model.SpansResponseChunk {
	spans := make([]*model.Span, 0, len(spanIDs))
	for _, spanID := range spanIDs {
		spans = append(spans, &model.Span{TraceId: traceID, SpanId: spanID})
	}
	return &model.SpansResponseChunk{Spans: spans}
}

var (
	traceOne = []byte{0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11}
	spanOne  = []byte{0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}
	spanTwo  = []byte{0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02}
)

// TestReadSpansStreamAccumulatesChunks covers the framing Jaeger's FindTraces uses. It answers a
// sequence of chunks and one trace straddles them, so the spans of a trace are gathered across
// messages rather than taken from the last one.
func TestReadSpansStreamAccumulatesChunks(t *testing.T) {
	traces, err := readSpansStream(context.Background(), &scriptedStream{chunks: []*model.SpansResponseChunk{
		chunk(traceOne, spanOne),
		chunk(traceOne, spanTwo),
	}})

	require.NoError(t, err)
	require.Len(t, traces, 1)
	for _, trace := range traces {
		assert.Len(t, trace.Spans, 2)
	}
}
