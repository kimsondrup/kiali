package converter

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	zlog "github.com/rs/zerolog/log"

	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	otel "github.com/kiali/kiali/tracing/otel/model"
	"github.com/kiali/kiali/tracing/tempo/tempopb"
	v1 "github.com/kiali/kiali/tracing/tempo/tempopb/common/v1"
)

func TestConvertId(t *testing.T) {
	assert := assert.New(t)

	id := getId()
	jaegerId := ConvertId(id)
	assert.Equal(jaegerModels.TraceID(id), jaegerId)
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
			assert.Equal(t, jaegerModels.TraceID(tc.expected), ConvertId(tc.id))
		})
	}
}

func TestConvertSpanId(t *testing.T) {
	assert := assert.New(t)

	id := getId()
	jaegerId := convertSpanId(id)
	assert.Equal(jaegerModels.SpanID(id), jaegerId)
}

// TestConvertSpanSetDuration covers the duration of a span matched by Tempo's search API, which
// Tempo computes itself and sends as one number. The subtraction behind it is unsigned too, so a
// span whose end precedes its start arrives already wrapped - and this path cannot recompute it,
// because it never receives the end time. Anything at or above 2^63 nanoseconds is a wrap: the
// longest duration an honest span can have is bounded by the time since the epoch, which is
// under 2^61.
func TestConvertSpanSetDuration(t *testing.T) {
	cases := map[string]struct {
		duration         string
		expectedDuration uint64
	}{
		"an ordinary span":               {duration: "646000", expectedDuration: 646},
		"an end four milliseconds early": {duration: "18446744073705551616", expectedDuration: 0},
		"the largest wrap there is":      {duration: strconv.FormatUint(1<<64-1, 10), expectedDuration: 0},
		"just under the threshold":       {duration: strconv.FormatUint(1<<63-1, 10), expectedDuration: (1<<63 - 1) / 1000},
		"a duration that cannot be read": {duration: "", expectedDuration: 0},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			span := otel.Span{
				SpanID:            "2e6ca056f5fc6fdc",
				StartTimeUnixNano: "1693389472310270000",
				DurationNanos:     tc.duration,
				Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
			}

			converted := ConvertSpanSet(span, "reviews.bookinfo", getId(), "root")
			require.Len(t, converted, 1)
			assert.Equal(t, tc.expectedDuration, converted[0].Duration)
		})
	}
}

// TestConvertTraceMetadataDuration checks the duration rule on the gRPC search path, where the value
// arrives as a number Tempo has already computed.
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
			trace := tempopb.TraceSearchMetadata{
				TraceID:       getId(),
				RootTraceName: "reviews.bookinfo.svc.cluster.local:9080/*",
				SpanSet: &tempopb.SpanSet{
					Spans: []*tempopb.Span{{
						SpanID:            "2e6ca056f5fc6fdc",
						Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
						StartTimeUnixNano: 1693389472310270000,
						DurationNanos:     tc.duration,
					}},
				},
			}

			converted, err := ConvertTraceMetadata(trace, "reviews.bookinfo")
			require.NoError(t, err)
			require.Len(t, converted.Spans, 1)
			assert.Equal(t, tc.expectedDuration, converted.Spans[0].Duration)
		})
	}
}

// TestTraceIdIsTheSameOnBothTempoTransports covers the trace ID Tempo answers a search with, which
// reaches the browser twice over: as the ID of the trace and as the ID its spans carry. The two
// drive different things - the spans' one is what a click on a span navigates with, the trace's one
// is what the trace detail is then compared against - so an unpadded ID in either place makes one
// trace into two, and the gRPC transport has to pad both to agree with the HTTP one.
func TestTraceIdIsTheSameOnBothTempoTransports(t *testing.T) {
	// A 31 character ID is Tempo's own answer, not a malformed one: it strips the leading zeros of
	// every trace ID it reports, so roughly one search result in sixteen arrives a character short.
	for name, tc := range map[string]struct{ id, expected string }{
		"a full width id":       {id: "b70b02766cb83333631f6654cc518265", expected: "b70b02766cb83333631f6654cc518265"},
		"one leading zero gone": {id: "2e299711ce47710289dc6640727404f", expected: "02e299711ce47710289dc6640727404f"},
		"several leading zeros": {id: "b70b02766cb83333631f6654cc5", expected: "00000b70b02766cb83333631f6654cc5"},
	} {
		t.Run(name, func(t *testing.T) {
			trace := tempopb.TraceSearchMetadata{
				TraceID:       tc.id,
				RootTraceName: "reviews.bookinfo.svc.cluster.local:9080/*",
				SpanSet: &tempopb.SpanSet{
					Spans: []*tempopb.Span{{
						SpanID:            "2e6ca056f5fc6fdc",
						Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
						StartTimeUnixNano: 1693389472310270000,
						DurationNanos:     646000,
					}},
				},
			}

			overGRPC, err := ConvertTraceMetadata(trace, "reviews.bookinfo")
			require.NoError(t, err)
			require.Len(t, overGRPC.Spans, 1)
			assert.Equal(t, jaegerModels.TraceID(tc.expected), overGRPC.TraceID)
			assert.Equal(t, overGRPC.TraceID, overGRPC.Spans[0].TraceID)

			overHTTP := ConvertSpanSet(otel.Span{
				SpanID:            "2e6ca056f5fc6fdc",
				StartTimeUnixNano: "1693389472310270000",
				DurationNanos:     "646000",
				Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
			}, "reviews.bookinfo", tc.id, "root")
			require.Len(t, overHTTP, 1)
			assert.Equal(t, overHTTP[0].TraceID, overGRPC.Spans[0].TraceID)
		})
	}
}

// TestConvertTraceMetadataWithoutSpanSet covers the gRPC half of the same answer. Its span set is
// a pointer, so a TraceSearchMetadata that carries none is a nil dereference rather than an empty
// range - a SIGSEGV on the path every Tempo traces list takes, reached from processStream.
func TestConvertTraceMetadataWithoutSpanSet(t *testing.T) {
	converted, err := ConvertTraceMetadata(tempopb.TraceSearchMetadata{
		TraceID:       "cafe9bc0903e18f6b914752f8ee577a5",
		RootTraceName: "reviews.bookinfo.svc.cluster.local:9080/*",
	}, "reviews.bookinfo")
	require.NoError(t, err)
	require.NotNil(t, converted)
	assert.Equal(t, jaegerModels.TraceID("cafe9bc0903e18f6b914752f8ee577a5"), converted.TraceID)
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
	converted, err := ConvertTraceMetadata(tempopb.TraceSearchMetadata{
		TraceID: "cafe9bc0903e18f6b914752f8ee577a5",
		SpanSet: &tempopb.SpanSet{},
	}, "reviews.bookinfo")
	require.NoError(t, err)
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
		wantType  jaegerModels.ValueType
	}{
		"a string": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "HTTP/1.1"}},
			wantValue: "HTTP/1.1", wantType: jaegerModels.StringType,
		},
		"an int": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 503}},
			wantValue: int64(503), wantType: jaegerModels.Int64Type,
		},
		"a bool": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}},
			wantValue: true, wantType: jaegerModels.BoolType,
		},
		"a double": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}},
			wantValue: 1.5, wantType: jaegerModels.Float64Type,
		},
		"a double that is not a number": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.NaN()}},
			wantValue: "NaN", wantType: jaegerModels.StringType,
		},
		"bytes": {
			value:     &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{10, 0, 0, 1}}},
			wantValue: []byte{10, 0, 0, 1}, wantType: jaegerModels.BinaryType,
		},
		"an array": {
			value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
				Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_StringValue{StringValue: "*/*"}}},
			}}},
			wantValue: `["*/*"]`, wantType: jaegerModels.StringType,
		},
		"a map": {
			value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{
				Values: []*commonpb.KeyValue{{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}}},
			}}},
			wantValue: `{"k":7}`, wantType: jaegerModels.StringType,
		},
		"no variant set":  {value: &commonpb.AnyValue{}, wantValue: "", wantType: jaegerModels.StringType},
		"no value at all": {wantValue: "", wantType: jaegerModels.StringType},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tags := convertAttributes([]*commonpb.KeyValue{{Key: "k", Value: tc.value}}, "2e6ca056f5fc6fdc")
			require.Len(t, tags, 1)
			assert.Equal(t, jaegerModels.KeyValue{Key: "k", Value: tc.wantValue, Type: tc.wantType}, tags[0])
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
	assert.Equal(t, []jaegerModels.KeyValue{{Key: "error", Value: true, Type: jaegerModels.BoolType}}, failed)

	for _, value := range []string{"ok", "unset"} {
		tags := convertAttributes([]*commonpb.KeyValue{{Key: "status", Value: text(value)}}, "2e6ca056f5fc6fdc")
		assert.Equal(t, []jaegerModels.KeyValue{{Key: "status", Value: value, Type: jaegerModels.StringType}}, tags)
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
	assert.Equal(t, jaegerModels.KeyValue{Key: "probe.strindex", Value: "", Type: jaegerModels.StringType}, tags[0])
	assert.Equal(t, jaegerModels.KeyValue{Key: "http.method", Value: "GET", Type: jaegerModels.StringType}, tags[1])

	text := logged.String()
	assert.Equal(t, 1, strings.Count(text, "Could not read the value of attribute(s)"), text)
	assert.Contains(t, text, "probe.strindex, probe.other")
	assert.Contains(t, text, "span 2e6ca056f5fc6fdc")
	assert.NotContains(t, text, "probe.unset")
	assert.NotContains(t, text, "http.method")
}

// TestConvertModelAttributes covers the same attributes on Tempo's gRPC search path, which is the
// path Kiali's Tempo provider takes by default. They were read through GetStringValue alone with
// the tag type hard-coded to string, so every number, boolean, array and kvlist reached the span
// detail table with a key and no value, where the HTTP path beside it reported them correctly.
func TestConvertModelAttributes(t *testing.T) {
	cases := map[string]struct {
		value     *v1.AnyValue
		wantValue any
		wantType  jaegerModels.ValueType
	}{
		"a string": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "HTTP/1.1"}},
			wantValue: "HTTP/1.1", wantType: jaegerModels.StringType,
		},
		"an int": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_IntValue{IntValue: 503}},
			wantValue: int64(503), wantType: jaegerModels.Int64Type,
		},
		"a bool": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_BoolValue{BoolValue: true}},
			wantValue: true, wantType: jaegerModels.BoolType,
		},
		"a double": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_DoubleValue{DoubleValue: 1.5}},
			wantValue: 1.5, wantType: jaegerModels.Float64Type,
		},
		"a double that is not a number": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_DoubleValue{DoubleValue: math.NaN()}},
			wantValue: "NaN", wantType: jaegerModels.StringType,
		},
		"bytes": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_BytesValue{BytesValue: []byte{10, 0, 0, 1}}},
			wantValue: []byte{10, 0, 0, 1}, wantType: jaegerModels.BinaryType,
		},
		"an array": {
			value: &v1.AnyValue{Value: &v1.AnyValue_ArrayValue{ArrayValue: &v1.ArrayValue{
				Values: []*v1.AnyValue{{Value: &v1.AnyValue_StringValue{StringValue: "*/*"}}},
			}}},
			wantValue: `["*/*"]`, wantType: jaegerModels.StringType,
		},
		"an array holding a double that is not a number": {
			value: &v1.AnyValue{Value: &v1.AnyValue_ArrayValue{ArrayValue: &v1.ArrayValue{
				Values: []*v1.AnyValue{
					{Value: &v1.AnyValue_DoubleValue{DoubleValue: 1.5}},
					{Value: &v1.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}},
				},
			}}},
			wantValue: `[1.5,"+Inf"]`, wantType: jaegerModels.StringType,
		},
		"a map": {
			value: &v1.AnyValue{Value: &v1.AnyValue_KvlistValue{KvlistValue: &v1.KeyValueList{
				Values: []*v1.KeyValue{{Key: "k", Value: &v1.AnyValue{Value: &v1.AnyValue_IntValue{IntValue: 7}}}},
			}}},
			wantValue: `{"k":7}`, wantType: jaegerModels.StringType,
		},
		"no variant set":  {value: &v1.AnyValue{}, wantValue: "", wantType: jaegerModels.StringType},
		"no value at all": {wantValue: "", wantType: jaegerModels.StringType},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tags := convertModelAttributes([]*v1.KeyValue{{Key: "k", Value: tc.value}})
			require.Len(t, tags, 1)
			assert.Equal(t, jaegerModels.KeyValue{Key: "k", Value: tc.wantValue, Type: tc.wantType}, tags[0])
		})
	}
}

// TestConvertModelAttributesStatus checks that the TraceQL status intrinsic is read the same way
// on both of Tempo's search transports. An error becomes the boolean tag the frontend reads; any
// other value stays a tag of its own, which the gRPC path used to drop - so a span whose status
// was "unset" arrived over gRPC with one tag fewer than over HTTP.
func TestConvertModelAttributesStatus(t *testing.T) {
	text := func(value string) *v1.AnyValue {
		return &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: value}}
	}

	failed := convertModelAttributes([]*v1.KeyValue{{Key: "status", Value: text("error")}})
	assert.Equal(t, []jaegerModels.KeyValue{{Key: "error", Value: true, Type: jaegerModels.BoolType}}, failed)

	for _, value := range []string{"ok", "unset"} {
		tags := convertModelAttributes([]*v1.KeyValue{{Key: "status", Value: text(value)}})
		assert.Equal(t, []jaegerModels.KeyValue{{Key: "status", Value: value, Type: jaegerModels.StringType}}, tags)
	}
}

// TestConvertSpanSetNoStartTime covers a span that Tempo's search API answers with and that carries
// no start time. Reported with a start time of zero it is dated to the Unix epoch; ConvertSpans
// drops such a span on the trace detail path, and this is the same rule on the path that feeds the
// traces list and the Metrics-tab span overlay.
//
// Kiali's frontend drops a span with no start time in both places that read them, so the backend
// agrees with the frontend.
func TestConvertSpanSetNoStartTime(t *testing.T) {
	for name, startTime := range map[string]string{"absent": "", "written as zero": "0", "not a number": "now"} {
		t.Run(name, func(t *testing.T) {
			span := otel.Span{SpanID: "2e6ca056f5fc6fdc", StartTimeUnixNano: startTime, DurationNanos: "646000"}
			assert.Empty(t, ConvertSpanSet(span, "reviews.bookinfo", getId(), "root"))
		})
	}
}

func getId() string {
	id := "727a0d200236314473666c051e6f65f4"
	return id
}
