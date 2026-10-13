package tests

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tests/integration/utils/kiali"
)

func TestServiceTraces(t *testing.T) {
	require := require.New(t)
	name := "details"
	traces, statusCode, err := kiali.Traces("services", name, kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
}

func TestWorkloadTraces(t *testing.T) {
	require := require.New(t)
	name := "details-v1"
	traces, statusCode, err := kiali.Traces("workloads", name, kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
}

func TestAppTraces(t *testing.T) {
	require := require.New(t)
	name := "details"
	traces, statusCode, err := kiali.Traces("apps", name, kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
}

// TestTraceDetail fetches the detail of a trace the search reported. Each provider builds the
// detail on a path of its own, separate from the search, so the search tests above say nothing
// about it: the IDs have to come back in hex at the width the search reported them, or the UI
// cannot match the detail to the row it was opened from, and the spans have to carry their
// parent references, or the trace renders as unrelated spans.
func TestTraceDetail(t *testing.T) {
	require := require.New(t)
	traces, statusCode, err := kiali.Traces("services", "details", kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
	traceID := string(traces.Data[0].TraceID)

	trace, statusCode, err := kiali.TraceDetail(traceID)
	require.NoError(err)
	require.Equal(200, statusCode)
	require.NotNil(trace)
	require.Regexp(`^[0-9a-f]{32}$`, string(trace.Data.TraceID))
	require.Equal(traceID, string(trace.Data.TraceID))
	require.NotEmpty(trace.Data.Spans)

	// The root span has no parent, so only some span has to carry a reference.
	referenced := false
	for _, span := range trace.Data.Spans {
		require.Regexp(`^[0-9a-f]{16}$`, string(span.SpanID))
		referenced = referenced || len(span.References) > 0
	}
	require.True(referenced, "no span of trace %s references its parent", traceID)
}

func TestWrongTracesType(t *testing.T) {
	require := require.New(t)
	name := "details"
	traces, statusCode, err := kiali.Traces("wrong", name, kiali.BOOKINFO)
	require.NotEqual(200, statusCode)
	require.NotNil(err)
	require.Empty(traces)
}

func TestWrongNamespaceTraces(t *testing.T) {
	require := require.New(t)
	name := "details"
	traces, _, _ := kiali.Traces("apps", name, "wrong")
	require.Empty(traces.Data)
	require.Empty(traces.Errors)
}

func TestServiceSpans(t *testing.T) {
	require := require.New(t)
	name := "details"
	spans, statusCode, err := kiali.Spans("services", name, kiali.BOOKINFO)
	assertSpans(spans, statusCode, err, require)
}

func TestAppSpans(t *testing.T) {
	require := require.New(t)
	name := "details"
	spans, statusCode, err := kiali.Spans("apps", name, kiali.BOOKINFO)
	assertSpans(spans, statusCode, err, require)
}

func TestWorkloadSpans(t *testing.T) {
	require := require.New(t)
	name := "details-v1"
	spans, statusCode, err := kiali.Spans("workloads", name, kiali.BOOKINFO)
	assertSpans(spans, statusCode, err, require)
}

func TestWrongTypeSpans(t *testing.T) {
	require := require.New(t)
	name := "details-v1"
	spans, statusCode, err := kiali.Spans("wrong", name, kiali.BOOKINFO)
	require.NotEqual(200, statusCode)
	require.NotNil(err)
	require.Empty(spans)
}

func TestWrongNamespaceSpans(t *testing.T) {
	require := require.New(t)
	name := "details-v1"
	spans, _, _ := kiali.Spans("apps", name, "wrong")
	require.Empty(spans)
}

func assertTraces(traces *models.TracingResponse, statusCode int, err error, require *require.Assertions) {
	if statusCode == 200 {
		require.NoError(err)
		require.NotNil(traces)
		require.NotEmpty(traces.Data)
		require.NotNil(traces.Data[0].TraceID)
		require.NotEmpty(traces.Data[0].Spans)
		for _, span := range traces.Data[0].Spans {
			require.NotNil(span.TraceID)
			require.Equal(span.TraceID, traces.Data[0].TraceID)
		}
	} else {
		require.Fail(fmt.Sprintf("Status code should be '200' but was: %d and error: %s", statusCode, err))
	}
}

func assertSpans(spans []models.TracingSpan, statusCode int, err error, require *require.Assertions) {
	tracing, _, errTracing := kiali.TracingConfig()
	require.NoError(errTracing)

	if statusCode == 200 {
		require.NoError(err)
		require.NotEmpty(spans)
		require.NotNil(spans[0].TraceID)
		// References in Tempo are converted from the SpanTraceId, which is not available from the /api/search in Tempo
		if tracing.Provider == "jaeger" {
			require.NotEmpty(spans[0].References)
			require.NotNil(spans[0].References[0].TraceID)
			require.Equal(spans[0].TraceID, spans[0].References[0].TraceID)
		}
	} else {
		require.Fail(fmt.Sprintf("Status code should be '200' but was: %d and error: %s", statusCode, err))
	}
}
