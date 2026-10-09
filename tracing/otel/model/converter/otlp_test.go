package converter

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	zlog "github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	"github.com/kiali/kiali/tracing/otel/otlp"
)

// Captured live from Tempo's trace detail. One trace of three spans over a resource reporting
// through two instrumentation scopes, and one of two spans carrying intValue attributes.
const (
	tempoMultiScope = "../../../tracingtest/responseTraceMultiScope.json"
	tempoTypedAttrs = "../../../tracingtest/responseTraceTypedAttrs.json"

	// Captured live from Tempo's trace detail: an agentgateway trace of four spans over two
	// resources, carrying an arrayValue attribute and both client- and server-kind spans.
	tempoTypedTrace = "../../../tracingtest/responseTypedTrace.json"
)

// Captured live from Jaeger 2.21.0: one trace of three spans, and a search answering three such
// traces. Both carry a resource with two scope spans and a span with intValue attributes.
const (
	apiv3TraceHTTP  = "../../../tracingtest/apiv3_trace_http.json"
	apiv3SearchHTTP = "../../../tracingtest/apiv3_search_http.json"
)

// The third duration guard, a pre-computed value at or above 2^63, is not reachable from here:
// this path subtracts two timestamps itself. It lives on Tempo's search path and is pinned by
// TestConvertSpanSetDuration and TestConvertTraceMetadataDuration.

func TestTracesFromOTLPTraceDetail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	traces := TracesFromOTLP(decode(t, tempoMultiScope))
	require.Len(traces, 1)

	trace := traces[0]
	assert.Equal(jaegerModels.TraceID("3fed459787ea5c51070a19c4e6cd3040"), trace.TraceID)
	// Three spans over three scope spans of two resources: the waypoint's server span, and the
	// two the application reports from two different instrumentation libraries.
	require.Len(trace.Spans, 3)
	assert.Equal(3, trace.Matched)
	assert.Equal([]jaegerModels.SpanID{"f6758abc16eb61c0", "3e1860bba5a4ac3d", "e5c939e83dcbc699"},
		[]jaegerModels.SpanID{trace.Spans[0].SpanID, trace.Spans[1].SpanID, trace.Spans[2].SpanID})

	waypoint := trace.Spans[0]
	assert.Equal("podinfo.tp-ntegzp.svc.cluster.local:9898/*", waypoint.OperationName)
	assert.Equal(uint64(1791297712473475), waypoint.StartTime)
	assert.Equal(uint64(1001381), waypoint.Duration)
	assert.Empty(waypoint.References)

	// The parent of a span is a CHILD_OF reference, in the same hex the span IDs are in.
	assert.Equal([]jaegerModels.Reference{{
		RefType: jaegerModels.ChildOf,
		TraceID: "3fed459787ea5c51070a19c4e6cd3040",
		SpanID:  "f6758abc16eb61c0",
	}}, trace.Spans[2].References)

	assert.Equal([]jaegerModels.ProcessID{"p1", "p2"}, processIDs(trace))
	assert.Equal("waypoint.tp-ntegzp", trace.Processes["p1"].ServiceName)
	assert.Equal("podinfo.tp-ntegzp.svc.cluster.local", trace.Processes["p2"].ServiceName)
	// The resource attributes other than the service name are the process tags.
	assert.Contains(trace.Processes["p2"].Tags, jaegerModels.KeyValue{Key: "service.version", Type: jaegerModels.StringType, Value: "6.9.2"})

	// The span of the second scope span is the one carrying the typed attributes, so reading only
	// the first scope span loses them entirely.
	assert.Contains(trace.Spans[2].Tags, jaegerModels.KeyValue{Key: "http.response.status_code", Type: jaegerModels.Int64Type, Value: int64(200)})
	assert.Contains(trace.Spans[2].Tags, jaegerModels.KeyValue{Key: "server.port", Type: jaegerModels.Int64Type, Value: int64(9898)})
}

func TestTracesFromOTLPGroupsSpansByTrace(t *testing.T) {
	assert := assert.New(t)

	traces := TracesFromOTLP(decodeAPIV3(t, apiv3SearchHTTP))

	// api_v3 answers a search as one flat list of resource spans; the three traces it holds are
	// told apart by the trace ID of each span.
	assert.Len(traces, 3)
	for _, trace := range traces {
		assert.Len(trace.Spans, 3)
		assert.Equal([]jaegerModels.ProcessID{"p1", "p2"}, processIDs(trace))
		for _, span := range trace.Spans {
			assert.Equal(trace.TraceID, span.TraceID)
			assert.Len(span.TraceID, 32)
			assert.Len(span.SpanID, 16)
		}
	}
}

// TestSpanSelectionsAgree pins what makes one converter serve both providers. business/tracing.go
// selects the spans of a service either by the service name of the process embedded in a span, or
// by looking the span's process ID up in the trace's process table; a converter that fills in only
// one of the two leaves the other selecting nothing, with no error anywhere.
func TestSpanSelectionsAgree(t *testing.T) {
	const serviceName = "podinfo.tp-otlp-sf2ed7.svc.cluster.local"

	for _, trace := range TracesFromOTLP(decodeAPIV3(t, apiv3SearchHTTP)) {
		byEmbeddedProcess := []jaegerModels.SpanID{}
		for _, span := range trace.Spans {
			if span.Process.ServiceName == serviceName {
				byEmbeddedProcess = append(byEmbeddedProcess, span.SpanID)
			}
		}

		processes := map[jaegerModels.ProcessID]jaegerModels.Process{}
		for processID, process := range trace.Processes {
			if process.ServiceName == serviceName {
				processes[processID] = process
			}
		}
		byProcessTable := []jaegerModels.SpanID{}
		for _, span := range trace.Spans {
			if _, found := processes[span.ProcessID]; found {
				byProcessTable = append(byProcessTable, span.SpanID)
			}
		}

		assert.NotEmpty(t, byProcessTable)
		assert.Equal(t, byProcessTable, byEmbeddedProcess)
	}
}

// TestTracesFromOTLPAgreeAcrossProviders pins that the api_v3 trace detail reaches the same shape
// Tempo's does, so that the business layer above reads one answer whichever provider served it.
func TestTracesFromOTLPAgreeAcrossProviders(t *testing.T) {
	trace := TraceFromOTLP(decodeAPIV3(t, apiv3TraceHTTP))
	require.NotNil(t, trace)

	assert.Equal(t, jaegerModels.TraceID("2132caa05ca64c82454600288b27f3b4"), trace.TraceID)
	require.Len(t, trace.Spans, 3)
	assert.Equal(t, 3, trace.Matched)
	assert.Equal(t, []jaegerModels.SpanID{"b5333aff2933f5c5", "b307fa7d04e21497", "9af7408b8320f22f"},
		[]jaegerModels.SpanID{trace.Spans[0].SpanID, trace.Spans[1].SpanID, trace.Spans[2].SpanID})

	assert.Equal(t, []jaegerModels.ProcessID{"p1", "p2"}, processIDs(*trace))
	assert.Equal(t, "waypoint.tp-otlp-sf2ed7", trace.Processes["p1"].ServiceName)
	assert.Equal(t, "podinfo.tp-otlp-sf2ed7.svc.cluster.local", trace.Processes["p2"].ServiceName)

	// The span of the second scope span is the one carrying the typed attributes, so reading only
	// the first scope span loses them entirely.
	assert.Contains(t, trace.Spans[2].Tags, jaegerModels.KeyValue{Key: "http.response.status_code", Type: jaegerModels.Int64Type, Value: int64(200)})
}

// TestEveryScopeSpanContributes covers a resource reporting through two instrumentation
// libraries, which a proxied application does for well over a third of the resources it sends.
func TestEveryScopeSpanContributes(t *testing.T) {
	first, second := otlpSpan(), otlpSpan()
	second.SpanId = []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}

	resource := resourceSpans("reviews.bookinfo", first)
	resource.ScopeSpans = append(resource.ScopeSpans, &tracepb.ScopeSpans{Spans: []*tracepb.Span{second}})

	traces := TracesFromOTLP(tracesOf(resource))
	require.Len(t, traces, 1)
	assert.Equal(t, []jaegerModels.SpanID{"2e6ca056f5fc6fdc", "1122334455667788"},
		[]jaegerModels.SpanID{traces[0].Spans[0].SpanID, traces[0].Spans[1].SpanID})
	// One resource is one process however many scopes it reports through.
	assert.Len(t, traces[0].Processes, 1)
}

func TestSpanDuration(t *testing.T) {
	const start = 1693389472310270000

	cases := map[string]struct {
		end              uint64
		expectedDuration uint64
	}{
		"an ordinary span":               {end: 1693389472310916000, expectedDuration: 646},
		"an end equal to the start":      {end: start, expectedDuration: 0},
		"an end four milliseconds early": {end: 1693389472306270000, expectedDuration: 0},
		"an end one nanosecond early":    {end: start - 1, expectedDuration: 0},
		"no end time at all":             {end: 0, expectedDuration: 0},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			span := otlpSpan()
			span.EndTimeUnixNano = tc.end

			traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", span)))
			// An end that cannot be believed costs the span its duration, not its place in the
			// trace: it still carries its name, its service and its tags.
			require.Len(t, traces, 1)
			require.Len(t, traces[0].Spans, 1)
			assert.Equal(t, tc.expectedDuration, traces[0].Spans[0].Duration)
		})
	}
}

// TestSpanWithoutStartTimeIsDropped covers a span that cannot be placed on a timeline. Reported
// with a start time of zero it is dated to the Unix epoch, in the trace list, in the heatmap and
// in the Metrics-tab span overlay alike.
func TestSpanWithoutStartTimeIsDropped(t *testing.T) {
	dated, undated := otlpSpan(), otlpSpan()
	undated.SpanId = []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	undated.StartTimeUnixNano = 0

	traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", dated, undated)))
	require.Len(t, traces, 1)
	require.Len(t, traces[0].Spans, 1)
	assert.Equal(t, jaegerModels.SpanID("2e6ca056f5fc6fdc"), traces[0].Spans[0].SpanID)
	// The dropped span mints no process either.
	assert.Len(t, traces[0].Processes, 1)

	// A trace left with no span at all is not reported as an empty trace.
	assert.Empty(t, TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", undated))))
}

func TestTypedAttributes(t *testing.T) {
	span := otlpSpan()
	span.Attributes = []*commonpb.KeyValue{
		attribute("http.request.method", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}),
		attribute("http.response.status_code", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}),
		attribute("istio.canonical", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}),
		attribute("sampler.param", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.5}}),
		attribute("peer.address", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0x0a, 0x00, 0x00, 0x01}}}),
		attribute("http.request.header.accept", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
			Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_StringValue{StringValue: "*/*"}}},
		}}}),
		attribute("nothing", nil),
	}

	traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", span)))
	require.Len(t, traces, 1)
	require.Len(t, traces[0].Spans, 1)

	assert.Equal(t, []jaegerModels.KeyValue{
		{Key: "http.request.method", Type: jaegerModels.StringType, Value: "GET"},
		{Key: "http.response.status_code", Type: jaegerModels.Int64Type, Value: int64(200)},
		{Key: "istio.canonical", Type: jaegerModels.BoolType, Value: true},
		{Key: "sampler.param", Type: jaegerModels.Float64Type, Value: 0.5},
		{Key: "peer.address", Type: jaegerModels.BinaryType, Value: []byte{0x0a, 0x00, 0x00, 0x01}},
		{Key: "http.request.header.accept", Type: jaegerModels.StringType, Value: `["*/*"]`},
		{Key: "nothing", Type: jaegerModels.StringType, Value: ""},
	}, traces[0].Spans[0].Tags)

	// Bytes keep their type to the edge of the process and reach the UI as the base64 OTLP wrote
	// them in, which is also how they are rendered inside a composite.
	rendered, err := json.Marshal(traces[0].Spans[0].Tags[4])
	require.NoError(t, err)
	assert.JSONEq(t, `{"key":"peer.address","type":"binary","value":"CgAAAQ=="}`, string(rendered))
}

func TestSpanKindTag(t *testing.T) {
	cases := map[tracepb.Span_SpanKind]string{
		tracepb.Span_SPAN_KIND_CLIENT:      "client",
		tracepb.Span_SPAN_KIND_SERVER:      "server",
		tracepb.Span_SPAN_KIND_PRODUCER:    "producer",
		tracepb.Span_SPAN_KIND_CONSUMER:    "consumer",
		tracepb.Span_SPAN_KIND_INTERNAL:    "internal",
		tracepb.Span_SPAN_KIND_UNSPECIFIED: "",
	}

	for kind, expected := range cases {
		t.Run(kind.String(), func(t *testing.T) {
			span := otlpSpan()
			span.Kind = kind

			traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", span)))
			require.Len(t, traces, 1)
			require.Len(t, traces[0].Spans, 1)

			if expected == "" {
				assert.Empty(t, traces[0].Spans[0].Tags)
				return
			}
			assert.Equal(t, []jaegerModels.KeyValue{{Key: "span.kind", Type: jaegerModels.StringType, Value: expected}},
				traces[0].Spans[0].Tags)
		})
	}
}

// TestErrorTag pins that a span comes out with exactly ONE error tag however many of the three
// signals it carries. Envoy sends the error attribute and an error status together on about one
// span in ten, and a search answer adds the status attribute, so a converter that reports each
// of them separately gives the same span three tags keyed "error" with three different types.
func TestErrorTag(t *testing.T) {
	errorTag := jaegerModels.KeyValue{Key: "error", Type: jaegerModels.BoolType, Value: true}
	statusError := attribute("status", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "error"}})
	errorString := func(value string) *commonpb.KeyValue {
		return attribute("error", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}})
	}
	errorBool := func(value bool) *commonpb.KeyValue {
		return attribute("error", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: value}})
	}

	cases := map[string]struct {
		status       *tracepb.Status
		attributes   []*commonpb.KeyValue
		expectedTags []jaegerModels.KeyValue
	}{
		"an error status": {
			status:       &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			expectedTags: []jaegerModels.KeyValue{errorTag, statusCodeTag("ERROR")},
		},
		"a status attribute of error": {
			attributes:   []*commonpb.KeyValue{statusError},
			expectedTags: []jaegerModels.KeyValue{errorTag},
		},
		"both at once": {
			status:       &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			attributes:   []*commonpb.KeyValue{statusError},
			expectedTags: []jaegerModels.KeyValue{errorTag, statusCodeTag("ERROR")},
		},
		// Envoy sends both signals at once, so a span that passed the error attribute through would
		// come out carrying two tags keyed "error" with two different types.
		"an error attribute and an error status, which is what a proxy sends": {
			status:       &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			attributes:   []*commonpb.KeyValue{errorString("true")},
			expectedTags: []jaegerModels.KeyValue{errorTag, statusCodeTag("ERROR")},
		},
		"an error attribute alone": {
			attributes:   []*commonpb.KeyValue{errorString("true")},
			expectedTags: []jaegerModels.KeyValue{errorTag},
		},
		"a Boolean error attribute": {
			attributes:   []*commonpb.KeyValue{errorBool(true)},
			expectedTags: []jaegerModels.KeyValue{errorTag},
		},
		// The attribute is the only account of WHY the span failed, so it is kept alongside the
		// flag. Two tags keyed "error" are fine here: the frontend asks tags.some(isErrorTag),
		// which matches the Boolean, and the message is what a reader needs.
		"an error attribute carrying the error itself": {
			attributes: []*commonpb.KeyValue{errorString("upstream connect error")},
			expectedTags: []jaegerModels.KeyValue{
				{Key: "error", Type: jaegerModels.StringType, Value: "upstream connect error"},
				errorTag,
			},
		},
		// A value that is not a flag at all. Folding it away loses it, and an attribute that
		// reached the span list before must keep reaching it.
		"an integer error attribute": {
			attributes: []*commonpb.KeyValue{attribute("error", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}})},
			expectedTags: []jaegerModels.KeyValue{
				{Key: "error", Type: jaegerModels.Int64Type, Value: int64(1)},
			},
		},
		"an integer error attribute beside an error status": {
			status:     &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			attributes: []*commonpb.KeyValue{attribute("error", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}})},
			expectedTags: []jaegerModels.KeyValue{
				{Key: "error", Type: jaegerModels.Int64Type, Value: int64(1)},
				errorTag,
				statusCodeTag("ERROR"),
			},
		},
		"a floating-point error attribute": {
			attributes: []*commonpb.KeyValue{attribute("error", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1}})},
			expectedTags: []jaegerModels.KeyValue{
				{Key: "error", Type: jaegerModels.Float64Type, Value: float64(1)},
			},
		},
		"an error attribute denying the error": {
			attributes:   []*commonpb.KeyValue{errorString("false")},
			expectedTags: []jaegerModels.KeyValue{},
		},
		"a Boolean error attribute denying the error": {
			attributes:   []*commonpb.KeyValue{errorBool(false)},
			expectedTags: []jaegerModels.KeyValue{},
		},
		"an error attribute denying an error the status reports": {
			status:       &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			attributes:   []*commonpb.KeyValue{errorBool(false)},
			expectedTags: []jaegerModels.KeyValue{errorTag, statusCodeTag("ERROR")},
		},
		"an unset status": {
			status:       &tracepb.Status{Code: tracepb.Status_STATUS_CODE_UNSET},
			expectedTags: []jaegerModels.KeyValue{},
		},
		"a status attribute of something else": {
			attributes: []*commonpb.KeyValue{
				attribute("status", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "ok"}}),
			},
			expectedTags: []jaegerModels.KeyValue{{Key: "status", Type: jaegerModels.StringType, Value: "ok"}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			span := otlpSpan()
			span.Status = tc.status
			span.Attributes = tc.attributes

			traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", span)))
			require.Len(t, traces, 1)
			require.Len(t, traces[0].Spans, 1)
			assert.Equal(t, tc.expectedTags, traces[0].Spans[0].Tags)
		})
	}
}

func TestProcessTable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reviews, ratings, again := otlpSpan(), otlpSpan(), otlpSpan()
	ratings.SpanId = []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	again.SpanId = []byte{0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00}

	nameless := resourceSpans("", again)
	nameless.Resource.Attributes = nil

	traces := TracesFromOTLP(tracesOf(
		resourceSpans("reviews.bookinfo", reviews),
		resourceSpans("ratings.bookinfo", ratings),
		// The same resource sent twice is one entry of the table, not two.
		resourceSpans("reviews.bookinfo", again),
		nameless,
	))
	require.Len(traces, 1)

	trace := traces[0]
	assert.Equal([]jaegerModels.ProcessID{"p1", "p2", "p3"}, processIDs(trace))
	assert.Equal("reviews.bookinfo", trace.Processes["p1"].ServiceName)
	assert.Equal("ratings.bookinfo", trace.Processes["p2"].ServiceName)
	// A resource with no service name is still named, so a span is never nameless in the UI.
	assert.Equal(unknownService, trace.Processes["p3"].ServiceName)

	require.Len(trace.Spans, 4)
	assert.Equal(jaegerModels.ProcessID("p1"), trace.Spans[0].ProcessID)
	assert.Equal(jaegerModels.ProcessID("p2"), trace.Spans[1].ProcessID)
	assert.Equal(jaegerModels.ProcessID("p1"), trace.Spans[2].ProcessID)
	assert.Equal(jaegerModels.ProcessID("p3"), trace.Spans[3].ProcessID)
	for _, span := range trace.Spans {
		process, found := trace.Processes[span.ProcessID]
		require.True(found)
		require.NotNil(span.Process)
		assert.Equal(process.ServiceName, span.Process.ServiceName)
	}
}

func TestTraceFromOTLP(t *testing.T) {
	trace := TraceFromOTLP(decode(t, tempoTypedAttrs))
	require.NotNil(t, trace)
	assert.Equal(t, jaegerModels.TraceID("4ce7f3617b6b969cad880718687d652d"), trace.TraceID)
	assert.Len(t, trace.Spans, 2)

	// A payload holding no span is no trace. Whether the backend does not have it or answered
	// badly is the transport's status to tell, not this layer's.
	assert.Nil(t, TraceFromOTLP(&tracepb.TracesData{}))
}

// TestTraceFromOTLPReportsOneTraceOnly covers a trace detail body that holds spans of more than
// one trace. Grouping the spans by their trace ID is what keeps the answer to one of them: a
// converter that collected every span into one trace would report a trace whose spans belong to
// another, and the frontend would draw them on one timeline.
func TestTraceFromOTLPReportsOneTraceOnly(t *testing.T) {
	asked, other := otlpSpan(), otlpSpan()
	other.TraceId = []byte{0x3f, 0xed, 0x45, 0x97, 0x87, 0xea, 0x5c, 0x51, 0x07, 0x0a, 0x19, 0xc4, 0xe6, 0xcd, 0x30, 0x40}
	other.SpanId = []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}

	traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", asked, other)))
	require.Len(t, traces, 2)

	trace := TraceFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", asked, other)))
	require.NotNil(t, trace)
	assert.Equal(t, jaegerModels.TraceID("727a0d200236314473666c051e6f65f4"), trace.TraceID)
	require.Len(t, trace.Spans, 1)
	assert.Equal(t, jaegerModels.SpanID("2e6ca056f5fc6fdc"), trace.Spans[0].SpanID)
}

// decodeAPIV3 reads a body api_v3 serves over HTTP, which renders its IDs as hex under an
// envelope of its own.
func statusCodeTag(code string) jaegerModels.KeyValue {
	return jaegerModels.KeyValue{Key: "otel.status_code", Type: jaegerModels.StringType, Value: code}
}

func decodeAPIV3(t *testing.T, path string) *tracepb.TracesData {
	t.Helper()

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	traces, err := otlp.Decode(body, otlp.Options{RootKey: otlp.RootKeyResult, IDEncoding: otlp.Hex})
	require.NoError(t, err)
	return traces
}

func decode(t *testing.T, path string) *tracepb.TracesData {
	t.Helper()

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	traces, err := otlp.Decode(body, otlp.Options{RootKey: otlp.RootKeyBatches, IDEncoding: otlp.Base64})
	require.NoError(t, err)
	return traces
}

// processIDs are the keys of the trace's process table, in minting order.
func processIDs(trace jaegerModels.Trace) []jaegerModels.ProcessID {
	ids := []jaegerModels.ProcessID{}
	for i := 1; i <= len(trace.Processes); i++ {
		id := jaegerModels.ProcessID("p" + strconv.Itoa(i))
		if _, found := trace.Processes[id]; found {
			ids = append(ids, id)
		}
	}
	return ids
}

func otlpSpan() *tracepb.Span {
	return &tracepb.Span{
		TraceId:           []byte{0x72, 0x7a, 0x0d, 0x20, 0x02, 0x36, 0x31, 0x44, 0x73, 0x66, 0x6c, 0x05, 0x1e, 0x6f, 0x65, 0xf4},
		SpanId:            []byte{0x2e, 0x6c, 0xa0, 0x56, 0xf5, 0xfc, 0x6f, 0xdc},
		Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
		StartTimeUnixNano: 1693389472310270000,
		EndTimeUnixNano:   1693389472310916000,
	}
}

func resourceSpans(serviceName string, spans ...*tracepb.Span) *tracepb.ResourceSpans {
	return &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			attribute(serviceNameAttribute, &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: serviceName}}),
		}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}
}

func tracesOf(resourceSpans ...*tracepb.ResourceSpans) *tracepb.TracesData {
	return &tracepb.TracesData{ResourceSpans: resourceSpans}
}

func attribute(key string, value *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: value}
}

// TestStatusTags covers the two tags that say WHY a span failed, where the error tag above says
// only that it did. The mapping to non-OTLP formats prescribes the name of the status code rather
// than its number, and says a status of UNSET MUST NOT be reported at all.
func TestStatusTags(t *testing.T) {
	cases := map[string]struct {
		status *tracepb.Status
		want   []jaegerModels.KeyValue
	}{
		"an error with a message": {
			status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "upstream connect error"},
			want: []jaegerModels.KeyValue{
				{Key: "error", Type: jaegerModels.BoolType, Value: true},
				{Key: "otel.status_code", Type: jaegerModels.StringType, Value: "ERROR"},
				{Key: "otel.status_description", Type: jaegerModels.StringType, Value: "upstream connect error"},
			},
		},
		"an error with no message": {
			status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			want: []jaegerModels.KeyValue{
				{Key: "error", Type: jaegerModels.BoolType, Value: true},
				{Key: "otel.status_code", Type: jaegerModels.StringType, Value: "ERROR"},
			},
		},
		// OK is a status a span set deliberately, so it is reported, but it is not a failure and
		// gets no error tag.
		"a deliberate OK": {
			status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
			want: []jaegerModels.KeyValue{
				{Key: "otel.status_code", Type: jaegerModels.StringType, Value: "OK"},
			},
		},
		"unset, which must not be reported": {
			status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_UNSET},
			want:   []jaegerModels.KeyValue{},
		},
		// A message with no code is not a status the mapping names, and the code is what it keys
		// the report on.
		"a message with no code": {
			status: &tracepb.Status{Message: "nothing to go on"},
			want:   []jaegerModels.KeyValue{},
		},
		"no status at all": {status: nil, want: []jaegerModels.KeyValue{}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			span := otlpSpan()
			span.Status = tc.status

			traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", span)))
			require.Len(t, traces, 1)
			require.Len(t, traces[0].Spans, 1)
			assert.Equal(t, tc.want, traces[0].Spans[0].Tags)
		})
	}
}

// TestScopeTags covers the instrumentation scope, which identifies the library a span came from
// and which upstream read and discarded. The deprecated otel.library.* aliases the mapping also
// asks for are declined, so no span carries them.
func TestScopeTags(t *testing.T) {
	cases := map[string]struct {
		scope *commonpb.InstrumentationScope
		want  []jaegerModels.KeyValue
	}{
		// Measured on every waypoint span of a live Istio mesh, read back from both Tempo and
		// Jaeger: the version Envoy reports is its whole build string, not a bare number.
		"a name and a version, as Envoy exports them": {
			scope: &commonpb.InstrumentationScope{Name: "envoy", Version: "3bb16354647d089b9a79020c9fc86cbbc5fcf84b/1.39.2-dev/Clean/RELEASE/BoringSSL"},
			want: []jaegerModels.KeyValue{
				{Key: "otel.scope.name", Type: jaegerModels.StringType, Value: "envoy"},
				{Key: "otel.scope.version", Type: jaegerModels.StringType, Value: "3bb16354647d089b9a79020c9fc86cbbc5fcf84b/1.39.2-dev/Clean/RELEASE/BoringSSL"},
			},
		},
		"a name alone": {
			scope: &commonpb.InstrumentationScope{Name: "agentgateway"},
			want: []jaegerModels.KeyValue{
				{Key: "otel.scope.name", Type: jaegerModels.StringType, Value: "agentgateway"},
			},
		},
		// A scope with neither field set, which is the shape the 2023 Tempo capture in
		// tracing/tracingtest/responseTrace.json carries on all six of its resources. There is
		// nothing to report it as.
		"a scope with neither field set": {scope: &commonpb.InstrumentationScope{}, want: []jaegerModels.KeyValue{}},
		"no scope at all":                {scope: nil, want: []jaegerModels.KeyValue{}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource := resourceSpans("reviews.bookinfo", otlpSpan())
			resource.ScopeSpans[0].Scope = tc.scope

			traces := TracesFromOTLP(tracesOf(resource))
			require.Len(t, traces, 1)
			require.Len(t, traces[0].Spans, 1)
			assert.Equal(t, tc.want, traces[0].Spans[0].Tags)
			for _, tag := range traces[0].Spans[0].Tags {
				assert.NotContains(t, tag.Key, "otel.library.")
			}
		})
	}
}

// TestEveryScopeCarriesItsOwnTags pins that the scope is read inside the per-scope loop rather
// than once per resource. The second resource of this capture reports through two instrumentation
// libraries, so each of its two spans must name the one that produced it: a converter that read
// the resource's first scope would give both spans the same name and say nothing about it.
func TestEveryScopeCarriesItsOwnTags(t *testing.T) {
	trace := TraceFromOTLP(decode(t, tempoMultiScope))
	require.NotNil(t, trace)
	require.Len(t, trace.Spans, 3)
	// Both spans belong to the same resource, so the scope is the only thing telling them apart.
	require.Equal(t, trace.Spans[1].ProcessID, trace.Spans[2].ProcessID)

	assert.Contains(t, trace.Spans[0].Tags, jaegerModels.KeyValue{Key: "otel.scope.name", Type: jaegerModels.StringType, Value: "envoy"})
	assert.Contains(t, trace.Spans[1].Tags, jaegerModels.KeyValue{
		Key: "otel.scope.name", Type: jaegerModels.StringType,
		Value: "github.com/stefanprodan/podinfo/pkg/api",
	})
	assert.Contains(t, trace.Spans[1].Tags, jaegerModels.KeyValue{Key: "otel.scope.version", Type: jaegerModels.StringType, Value: "6.9.2"})
	assert.Contains(t, trace.Spans[2].Tags, jaegerModels.KeyValue{
		Key: "otel.scope.name", Type: jaegerModels.StringType,
		Value: "go.opentelemetry.io/contrib/instrumentation/github.com/gorilla/mux/otelmux",
	})
	assert.Contains(t, trace.Spans[2].Tags, jaegerModels.KeyValue{Key: "otel.scope.version", Type: jaegerModels.StringType, Value: "0.63.0"})
}

// TestNonFiniteDoubles covers a double that JSON has no literal for. Left as a float64, a bare
// NaN makes json.Marshal refuse the WHOLE Kiali response, and one nested in an array costs that
// attribute every element it had.
func TestNonFiniteDoubles(t *testing.T) {
	double := func(value float64) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: value}}
	}

	span := otlpSpan()
	span.Attributes = []*commonpb.KeyValue{
		attribute("queue.ratio", double(math.NaN())),
		attribute("sampler.param", double(0.25)),
		attribute("latencies", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
			Values: []*commonpb.AnyValue{double(1.5), double(math.Inf(1))},
		}}}),
		attribute("budget", &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
			Values: []*commonpb.KeyValue{
				{Key: "ok", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}},
				{Key: "over", Value: double(math.Inf(-1))},
			},
		}}}),
	}

	traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", span)))
	require.Len(t, traces, 1)
	require.Len(t, traces[0].Spans, 1)

	assert.Equal(t, []jaegerModels.KeyValue{
		{Key: "queue.ratio", Type: jaegerModels.StringType, Value: "NaN"},
		{Key: "sampler.param", Type: jaegerModels.Float64Type, Value: 0.25},
		// The sibling is the point: the infinity costs its own element and nothing else.
		{Key: "latencies", Type: jaegerModels.StringType, Value: `[1.5,"+Inf"]`},
		{Key: "budget", Type: jaegerModels.StringType, Value: `{"ok":7,"over":"-Inf"}`},
	}, traces[0].Spans[0].Tags)

	// The whole trace serialises, which is what one raw NaN takes away.
	_, err := json.Marshal(traces[0])
	require.NoError(t, err)
}

// TestUnreadAttributeVariant covers an attribute whose value uses an AnyValue variant this build
// cannot read. Today that is string_value_strindex, which the proto reserves for the Profiling
// signal and which Tempo 3.1.0 accepts and hands back unchanged. The proto's own comment on the
// field tells a receiver of any other signal to log about it, so the key is kept, the value left
// empty and the reason reported - one line per span, naming the span and every key.
func TestUnreadAttributeVariant(t *testing.T) {
	strindex := &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 3}}

	span := otlpSpan()
	span.Attributes = []*commonpb.KeyValue{
		attribute("probe.strindex", strindex),
		attribute("http.request.method", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}),
		attribute("probe.other", strindex),
		// An unset value is legal OTLP and must not be warned about.
		attribute("probe.unset", &commonpb.AnyValue{}),
	}

	logged := &bytes.Buffer{}
	restore := zlog.Logger
	zlog.Logger = zlog.Logger.Output(logged)
	defer func() { zlog.Logger = restore }()

	traces := TracesFromOTLP(tracesOf(resourceSpans("reviews.bookinfo", span)))
	require.Len(t, traces, 1)
	require.Len(t, traces[0].Spans, 1)

	tags := traces[0].Spans[0].Tags
	require.Len(t, tags, 4)
	assert.Equal(t, jaegerModels.KeyValue{Key: "probe.strindex", Type: jaegerModels.StringType, Value: ""}, tags[0])
	assert.Equal(t, jaegerModels.KeyValue{Key: "http.request.method", Type: jaegerModels.StringType, Value: "GET"}, tags[1])

	text := logged.String()
	assert.Equal(t, 1, strings.Count(text, "Could not read the value of attribute(s)"), text)
	assert.Contains(t, text, "probe.strindex, probe.other")
	assert.Contains(t, text, "span 2e6ca056f5fc6fdc")
	assert.NotContains(t, text, "probe.unset")
	assert.NotContains(t, text, "http.request.method")
}

// TestTracesFromOTLPAgentGatewayTrace reads a second live trace detail end to end. It is the only
// committed fixture carrying an arrayValue attribute and a client-kind span from a real backend,
// and its two resources report through scopes with and without a version.
func TestTracesFromOTLPAgentGatewayTrace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	trace := TraceFromOTLP(decode(t, tempoTypedTrace))
	require.NotNil(trace)
	require.Len(trace.Spans, 4)

	assert.Equal(jaegerModels.TraceID("60bad1536be1cd2f5b39bf02883c9656"), trace.TraceID)
	assert.Equal([]jaegerModels.ProcessID{"p1", "p2"}, processIDs(*trace))
	assert.Equal("api-gateway.agentgateway-system", trace.Processes["p1"].ServiceName)
	assert.Equal("orders-api.api-orders", trace.Processes["p2"].ServiceName)

	assert.Contains(trace.Spans[0].Tags, jaegerModels.KeyValue{Key: "span.kind", Type: jaegerModels.StringType, Value: "server"})
	assert.Contains(trace.Spans[1].Tags, jaegerModels.KeyValue{Key: "span.kind", Type: jaegerModels.StringType, Value: "client"})
	assert.Contains(trace.Spans[1].Tags, jaegerModels.KeyValue{Key: "grpc.status", Type: jaegerModels.Int64Type, Value: int64(0)})
	assert.Contains(trace.Spans[3].Tags, jaegerModels.KeyValue{Key: "otel.scope.version", Type: jaegerModels.StringType, Value: "1.31.6"})
	assert.Contains(trace.Spans[3].Tags, jaegerModels.KeyValue{
		Key: "@jaeger@warnings", Type: jaegerModels.StringType,
		Value: `["clock skew adjustment disabled; not applying calculated delta of 408.5µs","clock skew adjustment disabled; not applying calculated delta of 408.5µs","clock skew adjustment disabled; not applying calculated delta of 408.5µs"]`,
	})
}
