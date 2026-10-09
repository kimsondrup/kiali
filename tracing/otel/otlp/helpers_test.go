package otlp

import (
	"encoding/hex"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func fixture(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	return body
}

func spans(traces *tracepb.TracesData) []*tracepb.Span {
	var all []*tracepb.Span
	for _, resourceSpans := range traces.GetResourceSpans() {
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			all = append(all, scopeSpans.GetSpans()...)
		}
	}
	return all
}

func traceIDs(traces *tracepb.TracesData) []string {
	var ids []string
	for _, span := range spans(traces) {
		id := hex.EncodeToString(span.GetTraceId())
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}
