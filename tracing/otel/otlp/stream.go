package otlp

import (
	"context"
	"errors"
	"fmt"
	"io"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Receiver is the one method a server-streaming OTLP response exposes.
type Receiver interface {
	Recv() (*tracepb.TracesData, error)
}

// ReadStream accumulates a server-streaming OTLP response into one TracesData.
//
// api_v3 FindTraces streams chunks rather than snapshots: the spans of one trace
// can arrive in several messages, so the resource spans of every message are
// kept. io.EOF is the only clean end of the stream — a cancelled or timed-out
// Recv is reported, because a partial result returned with a nil error reads as
// a complete answer that happens to be short.
func ReadStream(ctx context.Context, stream Receiver) (*tracepb.TracesData, error) {
	traces := &tracepb.TracesData{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("OTLP stream: %w", err)
		}
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return traces, nil
		}
		if err != nil {
			return nil, fmt.Errorf("OTLP stream: %w", err)
		}
		traces.ResourceSpans = append(traces.ResourceSpans, message.GetResourceSpans()...)
	}
}
