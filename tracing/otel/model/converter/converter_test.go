package converter

import (
	"bytes"
	"math"
	"strings"
	"testing"

	zlog "github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	jsonModel "github.com/kiali/kiali/models/tracing/json"
	"github.com/kiali/kiali/tracing/tempo/querypb"
)

func TestConvertId(t *testing.T) {
	assert := assert.New(t)

	id := getId()
	jaegerId := ConvertId(id)
	assert.Equal(jsonModel.TraceID(id), jaegerId)
}

// TestConvertIdPadsTempoId covers the trace IDs Tempo answers a search with, which it strips the
// leading zeros of. Tempo serves the detail of a padded ID as readily as of an unpadded one -
// measured, both answer 200 for the same trace - so the padded form is the one Kiali carries.
func TestConvertIdPadsTempoId(t *testing.T) {
	for name, tc := range map[string]struct{ id, expected string }{
		"a full width id":            {id: "b70b02766cb83333631f6654cc518265", expected: "b70b02766cb83333631f6654cc518265"},
		"one leading zero gone":      {id: "b70b02766cb83333631f6654cc51826", expected: "0b70b02766cb83333631f6654cc51826"},
		"several leading zeros":      {id: "b70b02766cb83333631f6654cc5", expected: "00000b70b02766cb83333631f6654cc5"},
		"a 64 bit id, zero extended": {id: "631f6654cc518265", expected: "0000000000000000631f6654cc518265"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, jsonModel.TraceID(tc.expected), ConvertId(tc.id))
		})
	}
}

func TestConvertSpanId(t *testing.T) {
	assert := assert.New(t)

	id := getId()
	jaegerId := convertSpanId(id)
	assert.Equal(jsonModel.SpanID(id), jaegerId)
}

// TestConvertTraceMetadataDuration covers the duration of a span matched by Tempo's search API,
// which Tempo computes itself and sends as one number. The subtraction behind it is unsigned too,
// so a span whose end precedes its start arrives already wrapped - and this path cannot recompute
// it, because it never receives the end time. Anything at or above 2^63 nanoseconds is a wrap:
// the longest duration an honest span can have is bounded by the time since the epoch, which is
// under 2^61.
func TestConvertTraceMetadataDuration(t *testing.T) {
	cases := map[string]struct {
		duration         uint64
		expectedDuration uint64
	}{
		"an ordinary span":          {duration: 646000, expectedDuration: 646},
		"a wrapped negative":        {duration: 18446744073705551616, expectedDuration: 0},
		"the largest wrap there is": {duration: 1<<64 - 1, expectedDuration: 0},
		"just under the threshold":  {duration: 1<<63 - 1, expectedDuration: (1<<63 - 1) / 1000},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			trace := &querypb.TraceSearchMetadata{
				TraceID:       getId(),
				RootTraceName: "reviews.bookinfo.svc.cluster.local:9080/*",
				SpanSet: &querypb.SpanSet{
					Spans: []*querypb.Span{{
						SpanID:            "2e6ca056f5fc6fdc",
						Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
						StartTimeUnixNano: 1693389472310270000,
						DurationNanos:     tc.duration,
					}},
				},
			}

			converted := ConvertTraceMetadata(trace, "reviews.bookinfo")
			require.Len(t, converted.Spans, 1)
			assert.Equal(t, tc.expectedDuration, converted.Spans[0].Duration)
		})
	}
}

// TestTraceIdIsTheSameOnBothTempoTransports covers the trace ID Tempo answers a search with, which
// reaches the browser twice over: as the ID of the trace and as the ID its spans carry. The two
// drive different things - the spans' one is what a click on a span navigates with, the trace's one
// is what the trace detail is then compared against - so an unpadded ID in either place makes one
// trace into two. Both of Tempo's transports reach this function, so the two agree by construction.
func TestTraceIdIsTheSameOnBothTempoTransports(t *testing.T) {
	// A 31 character ID is Tempo's own answer, not a malformed one: it strips the leading zeros of
	// every trace ID it reports, so roughly one search result in sixteen arrives a character short.
	for name, tc := range map[string]struct{ id, expected string }{
		"a full width id":       {id: "b70b02766cb83333631f6654cc518265", expected: "b70b02766cb83333631f6654cc518265"},
		"one leading zero gone": {id: "2e299711ce47710289dc6640727404f", expected: "02e299711ce47710289dc6640727404f"},
		"several leading zeros": {id: "b70b02766cb83333631f6654cc5", expected: "00000b70b02766cb83333631f6654cc5"},
	} {
		t.Run(name, func(t *testing.T) {
			trace := &querypb.TraceSearchMetadata{
				TraceID:       tc.id,
				RootTraceName: "reviews.bookinfo.svc.cluster.local:9080/*",
				SpanSet: &querypb.SpanSet{
					Spans: []*querypb.Span{{
						SpanID:            "2e6ca056f5fc6fdc",
						Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
						StartTimeUnixNano: 1693389472310270000,
						DurationNanos:     646000,
					}},
				},
			}

			converted := ConvertTraceMetadata(trace, "reviews.bookinfo")
			require.Len(t, converted.Spans, 1)
			assert.Equal(t, jsonModel.TraceID(tc.expected), converted.TraceID)
			assert.Equal(t, converted.TraceID, converted.Spans[0].TraceID)
		})
	}
}

// TestConvertTraceMetadataMatched covers the matched count a trace reports, which the scatter plot
// sizes a trace's bubble by. Tempo bounds the spans it sends per trace with SpansPerSpanSet, which
// Kiali sets to ten, so the count is the span set's own and not the number of spans that arrived.
func TestConvertTraceMetadataMatched(t *testing.T) {
	spans := make([]*querypb.Span, 3)
	for i := range spans {
		spans[i] = &querypb.Span{
			SpanID:            "2e6ca056f5fc6fdc",
			StartTimeUnixNano: 1693389472310270000,
			DurationNanos:     646000,
		}
	}

	converted := ConvertTraceMetadata(&querypb.TraceSearchMetadata{
		TraceID: "b70b02766cb83333631f6654cc518265",
		SpanSet: &querypb.SpanSet{Matched: 6, Spans: spans},
	}, "reviews.bookinfo")

	require.Len(t, converted.Spans, 3)
	assert.Equal(t, 6, converted.Matched)
}

// TestConvertTraceMetadataOperationName covers the operation name a matched span reports.
// business.getFilteredSpans keeps a span whose operation name starts with the service it queried,
// so a span named after the trace's root is a span that service does not see - and Tempo names the
// root after whichever service started the trace, which is not the one being queried.
func TestConvertTraceMetadataOperationName(t *testing.T) {
	span := func(name string) *querypb.Span {
		return &querypb.Span{
			SpanID:            "2e6ca056f5fc6fdc",
			Name:              name,
			StartTimeUnixNano: 1693389472310270000,
			DurationNanos:     646000,
		}
	}

	converted := ConvertTraceMetadata(&querypb.TraceSearchMetadata{
		TraceID:       "b70b02766cb83333631f6654cc518265",
		RootTraceName: "productpage.bookinfo.svc.cluster.local:9080/productpage",
		SpanSet: &querypb.SpanSet{Spans: []*querypb.Span{
			span("reviews.bookinfo.svc.cluster.local:9080/*"),
			span(""),
		}},
	}, "reviews.bookinfo")

	require.Len(t, converted.Spans, 2)
	assert.Equal(t, "reviews.bookinfo.svc.cluster.local:9080/*", converted.Spans[0].OperationName)
	// A span that reported no name of its own falls back to the trace's root name.
	assert.Equal(t, "productpage.bookinfo.svc.cluster.local:9080/productpage", converted.Spans[1].OperationName)
}

// TestConvertTraceMetadataProcessTable covers the process table a Tempo search result reports.
// Tempo's search answer carries no resource, so the table has to be minted from the one service
// name the answer names - and business.tracesToSpans selects a service's spans by looking a
// span's process ID up in it, so a trace without one contributes no span at all.
func TestConvertTraceMetadataProcessTable(t *testing.T) {
	span := func(id string) *querypb.Span {
		return &querypb.Span{
			SpanID:            id,
			Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
			StartTimeUnixNano: 1693389472310270000,
			DurationNanos:     646000,
		}
	}

	converted := ConvertTraceMetadata(&querypb.TraceSearchMetadata{
		TraceID: "b70b02766cb83333631f6654cc518265",
		SpanSet: &querypb.SpanSet{Spans: []*querypb.Span{span("2e6ca056f5fc6fdc"), span("5d7ab9e039d25ebd")}},
	}, "reviews.bookinfo")

	// One service name is one entry, however many spans report it.
	require.Equal(t, []jsonModel.ProcessID{"p1"}, processIDs(*converted))
	assert.Equal(t, "reviews.bookinfo", converted.Processes["p1"].ServiceName)

	require.Len(t, converted.Spans, 2)
	for _, span := range converted.Spans {
		process, found := converted.Processes[span.ProcessID]
		require.True(t, found, "span [%s] refers to no process of the table", span.SpanID)
		assert.Equal(t, "reviews.bookinfo", process.ServiceName)
	}
}

// TestConvertTraceMetadataWithoutSpanSet covers the gRPC half of the same answer. Its span set is
// a pointer, so a TraceSearchMetadata that carries none is a nil dereference rather than an empty
// range - a SIGSEGV on the path every Tempo traces list takes, reached from processStream.
func TestConvertTraceMetadataWithoutSpanSet(t *testing.T) {
	converted := ConvertTraceMetadata(&querypb.TraceSearchMetadata{
		TraceID:       "cafe9bc0903e18f6b914752f8ee577a5",
		RootTraceName: "reviews.bookinfo.svc.cluster.local:9080/*",
	}, "reviews.bookinfo")
	require.NotNil(t, converted)
	assert.Equal(t, jsonModel.TraceID("cafe9bc0903e18f6b914752f8ee577a5"), converted.TraceID)
	assert.Equal(t, 0, converted.Matched)

	// Both assertions are needed: a nil slice or map satisfies Empty as readily as an empty one,
	// and it is the nil that serialises as JSON null and takes the Traces tab down.
	assert.NotNil(t, converted.Spans)
	assert.Empty(t, converted.Spans)
	assert.NotNil(t, converted.Processes)
	assert.Empty(t, converted.Processes)
	assert.NotNil(t, converted.Warnings)
	assert.Empty(t, converted.Warnings)
}

// TestConvertTraceMetadataWithAnEmptySpanSet covers the same answer written with the key present
// and holding nothing, which is what Tempo's HTTP transport sends for such a trace.
func TestConvertTraceMetadataWithAnEmptySpanSet(t *testing.T) {
	converted := ConvertTraceMetadata(&querypb.TraceSearchMetadata{
		TraceID: "cafe9bc0903e18f6b914752f8ee577a5",
		SpanSet: &querypb.SpanSet{},
	}, "reviews.bookinfo")
	assert.NotNil(t, converted.Spans)
	assert.Empty(t, converted.Spans)
}

// TestConvertAttributes covers the attributes of a span matched by Tempo's search API. They are
// OTLP attributes embedded in a document that is not OTLP, and they reach Jaeger's typed
// key-value through the same decision the trace detail path makes, so a port or a status code
// arrives as the number it is rather than as an empty string.
func TestConvertAttributes(t *testing.T) {
	cases := map[string]struct {
		value     *commonpb.AnyValue
		wantValue any
		wantType  jsonModel.ValueType
	}{
		"a string": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "HTTP/1.1"}},
			wantValue: "HTTP/1.1", wantType: jsonModel.StringType,
		},
		"an int": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 503}},
			wantValue: int64(503), wantType: jsonModel.Int64Type,
		},
		"a bool": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}},
			wantValue: true, wantType: jsonModel.BoolType,
		},
		"a double": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}},
			wantValue: 1.5, wantType: jsonModel.Float64Type,
		},
		"a double that is not a number": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.NaN()}},
			wantValue: "NaN", wantType: jsonModel.StringType,
		},
		"bytes": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{10, 0, 0, 1}}},
			wantValue: []byte{10, 0, 0, 1}, wantType: jsonModel.BinaryType,
		},
		"an array": {
			value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
				Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_StringValue{StringValue: "*/*"}}},
			}}},
			wantValue: `["*/*"]`, wantType: jsonModel.StringType,
		},
		"an array holding a double that is not a number": {
			value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
				Values: []*commonpb.AnyValue{
					{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}},
					{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}},
				},
			}}},
			wantValue: `[1.5,"+Inf"]`, wantType: jsonModel.StringType,
		},
		"a map": {
			value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
				Values: []*commonpb.KeyValue{{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}}},
			}}},
			wantValue: `{"k":7}`, wantType: jsonModel.StringType,
		},
		"no variant set":  {value: &commonpb.AnyValue{}, wantValue: "", wantType: jsonModel.StringType},
		"no value at all": {wantValue: "", wantType: jsonModel.StringType},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tags := convertAttributes([]*commonpb.KeyValue{{Key: "k", Value: tc.value}}, "2e6ca056f5fc6fdc")
			require.Len(t, tags, 1)
			assert.Equal(t, jsonModel.KeyValue{Key: "k", Value: tc.wantValue, Type: tc.wantType}, tags[0])
		})
	}
}

// TestConvertAttributesStatus covers the "status" attribute, which is the TraceQL status intrinsic
// prepareTraceQL selects rather than an attribute of the span. A matched span that failed reports
// the boolean tag Kiali's frontend reads; any other value of it is an ordinary tag.
func TestConvertAttributesStatus(t *testing.T) {
	text := func(value string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
	}

	failed := convertAttributes([]*commonpb.KeyValue{{Key: "status", Value: text("error")}}, "2e6ca056f5fc6fdc")
	assert.Equal(t, []jsonModel.KeyValue{{Key: "error", Value: true, Type: jsonModel.BoolType}}, failed)

	for _, value := range []string{"ok", "unset"} {
		tags := convertAttributes([]*commonpb.KeyValue{{Key: "status", Value: text(value)}}, "2e6ca056f5fc6fdc")
		assert.Equal(t, []jsonModel.KeyValue{{Key: "status", Value: value, Type: jsonModel.StringType}}, tags)
	}
}

// TestConvertAttributesNamesAnUnreadVariant covers an attribute of a matched span whose value was
// written as a variant this build cannot read. The key is kept so the span still says the
// attribute was there, and the reason is logged once per span, naming the span so it can be found
// in Grafana.
func TestConvertAttributesNamesAnUnreadVariant(t *testing.T) {
	strindex := &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValueStrindex{StringValueStrindex: 3}}

	logged := &bytes.Buffer{}
	restore := zlog.Logger
	zlog.Logger = zlog.Logger.Output(logged)
	defer func() { zlog.Logger = restore }()

	tags := convertAttributes([]*commonpb.KeyValue{
		{Key: "probe.strindex", Value: strindex},
		{Key: "http.method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}},
		{Key: "probe.other", Value: strindex},
		// An unset value is legal OTLP and must not be warned about.
		{Key: "probe.unset", Value: &commonpb.AnyValue{}},
	}, "2e6ca056f5fc6fdc")

	require.Len(t, tags, 4)
	assert.Equal(t, jsonModel.KeyValue{Key: "probe.strindex", Value: "", Type: jsonModel.StringType}, tags[0])
	assert.Equal(t, jsonModel.KeyValue{Key: "http.method", Value: "GET", Type: jsonModel.StringType}, tags[1])

	text := logged.String()
	assert.Equal(t, 1, strings.Count(text, "Could not read the value of attribute(s)"), text)
	assert.Contains(t, text, "probe.strindex, probe.other")
	assert.Contains(t, text, "span 2e6ca056f5fc6fdc")
	assert.NotContains(t, text, "probe.unset")
	assert.NotContains(t, text, "http.method")
}

// TestConvertTraceMetadataNoStartTime covers a span that Tempo's search API answers with and that
// carries no start time. Reported with a start time of zero it is dated to the Unix epoch;
// ConvertSpans drops such a span on the trace detail path, and this is the same rule on the path
// that feeds the traces list and the Metrics-tab span overlay.
//
// Kiali's frontend drops a span with no start time in both places that read them, so the backend
// agrees with the frontend.
func TestConvertTraceMetadataNoStartTime(t *testing.T) {
	converted := ConvertTraceMetadata(&querypb.TraceSearchMetadata{
		TraceID: getId(),
		SpanSet: &querypb.SpanSet{Spans: []*querypb.Span{
			{SpanID: "2e6ca056f5fc6fdc", DurationNanos: 646000},
			{SpanID: "e5c939e83dcbc699", StartTimeUnixNano: 1693389472310270000, DurationNanos: 646000},
		}},
	}, "reviews.bookinfo")

	// The span that can be placed on a timeline is kept, so one unplaceable span does not cost
	// the trace the rest of its spans.
	require.Len(t, converted.Spans, 1)
	assert.Equal(t, jsonModel.SpanID("e5c939e83dcbc699"), converted.Spans[0].SpanID)
}

func getId() string {
	id := "727a0d200236314473666c051e6f65f4"
	return id
}
