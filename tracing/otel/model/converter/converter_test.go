package converter

import (
	"bytes"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	zlog "github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
)

func TestConvertId(t *testing.T) {
	assert := assert.New(t)

	id := getId()
	jaegerId := ConvertId(id)
	assert.Equal(jaegerModels.TraceID(id), jaegerId)
}

func TestConvertSpanId(t *testing.T) {
	assert := assert.New(t)

	id, err := hex.DecodeString("49cd77d1f9dcd936")
	assert.Nil(err)
	assert.Equal(jaegerModels.SpanID("49cd77d1f9dcd936"), convertSpanId(id))
	assert.Equal(jaegerModels.SpanID(""), convertSpanId(nil))
}

func TestConvertSpans(t *testing.T) {
	assert := assert.New(t)

	spans := getSpans()
	id := getId()
	serviceName := "kiali-traffic-generator.bookinfo"

	jaegerSpans := ConvertSpans(spans, serviceName, id)
	assert.Equal(jaegerModels.SpanID("49cd77d1f9dcd936"), jaegerSpans[0].SpanID)
	assert.Equal(serviceName, jaegerSpans[0].Process.ServiceName)
	assert.Equal("reviews.bookinfo.svc.cluster.local:9080/*", jaegerSpans[0].OperationName)
	assert.Equal(uint64(646), jaegerSpans[0].Duration)
	assert.Equal(jaegerModels.TraceID(id), jaegerSpans[0].References[0].TraceID)
	assert.Equal(jaegerModels.SpanID("1234567890abcdef"), jaegerSpans[0].References[0].SpanID)
}

// TestConvertAttributes covers the tag a span attribute becomes, for every variant an OTLP
// attribute value can be written as. The frontend reads a status code as a number and an error
// flag for truth, so the tag type matters as much as the value does.
func TestConvertAttributes(t *testing.T) {
	cases := map[string]struct {
		value     *commonv1.AnyValue
		wantValue any
		wantType  jaegerModels.ValueType
	}{
		"a string": {
			value:     &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "HTTP/1.1"}},
			wantValue: "HTTP/1.1", wantType: jaegerModels.StringType,
		},
		"a bool": {
			value:     &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: true}},
			wantValue: true, wantType: jaegerModels.BoolType,
		},
		"an int": {
			value:     &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 503}},
			wantValue: int64(503), wantType: jaegerModels.Int64Type,
		},
		"a double": {
			value:     &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: 1.5}},
			wantValue: 1.5, wantType: jaegerModels.Float64Type,
		},
		"a double that is not a finite number": {
			value:     &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}},
			wantValue: "+Inf", wantType: jaegerModels.StringType,
		},
		"bytes": {
			value:     &commonv1.AnyValue{Value: &commonv1.AnyValue_BytesValue{BytesValue: []byte("hi")}},
			wantValue: []byte("hi"), wantType: jaegerModels.BinaryType,
		},
		"an array, which Jaeger renders as JSON too": {
			value: &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{ArrayValue: &commonv1.ArrayValue{
				Values: []*commonv1.AnyValue{
					{Value: &commonv1.AnyValue_StringValue{StringValue: "a"}},
					{Value: &commonv1.AnyValue_IntValue{IntValue: 2}},
				},
			}}},
			wantValue: `["a",2]`, wantType: jaegerModels.StringType,
		},
		"a map": {
			value: &commonv1.AnyValue{Value: &commonv1.AnyValue_KvlistValue{KvlistValue: &commonv1.KeyValueList{
				Values: []*commonv1.KeyValue{
					{Key: "k", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "v"}}},
				},
			}}},
			wantValue: `{"k":"v"}`, wantType: jaegerModels.StringType,
		},
		"no variant set": {
			value:     &commonv1.AnyValue{},
			wantValue: "", wantType: jaegerModels.StringType,
		},
		"no value at all": {
			value:     nil,
			wantValue: "", wantType: jaegerModels.StringType,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			attributes := []*commonv1.KeyValue{{Key: "k", Value: tc.value}}
			tags := convertAttributes(attributes)

			assert.Len(t, tags, 1)
			assert.Equal(t, "k", tags[0].Key)
			assert.Equal(t, tc.wantValue, tags[0].Value)
			assert.Equal(t, tc.wantType, tags[0].Type)
		})
	}
}

// TestConvertAttributesError covers how a span matched by Tempo's search API says it failed: the
// "status" attribute, which is the TraceQL status intrinsic prepareTraceQL selects. Measured
// against Tempo 3.1.0, its three values are "error", "ok" and "unset".
func TestConvertAttributesError(t *testing.T) {
	errorTag := jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType}

	attribute := []*commonv1.KeyValue{
		{Key: "status", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "error"}}},
	}
	assert.Equal(t, []jaegerModels.KeyValue{errorTag}, convertAttributes(attribute))

	unset := []*commonv1.KeyValue{
		{Key: "status", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "unset"}}},
	}
	assert.Equal(t, []jaegerModels.KeyValue{{Key: "status", Value: "unset", Type: jaegerModels.StringType}},
		convertAttributes(unset))
}

// TestConvertStatus covers the span-level status, which arrives on the trace detail path, where
// the response really is OTLP. The mapping to non-OTLP formats says a status of UNSET MUST NOT
// be reported at all.
func TestConvertStatus(t *testing.T) {
	errorTag := jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType}

	assert.Equal(t, []jaegerModels.KeyValue{errorTag},
		convertStatus(&tracev1.Status{Code: tracev1.Status_STATUS_CODE_ERROR}))
	assert.Nil(t, convertStatus(&tracev1.Status{Code: tracev1.Status_STATUS_CODE_OK}))
	assert.Nil(t, convertStatus(&tracev1.Status{Code: tracev1.Status_STATUS_CODE_UNSET}))
	assert.Nil(t, convertStatus(nil))
}

// TestConvertSpansDuration checks the duration arithmetic. The subtraction is between two unsigned
// nanosecond timestamps, so an end that is not after the start wraps round to hundreds of years:
// without the guard, the absent end below reports 16655768095921845us, which is 528 years, and the
// end one microsecond early reports 18446744073709550us. No span of the captured responses in
// ../../../tracingtest has an end before its start - both shapes are ones the proto permits rather
// than ones a backend here was seen to send.
func TestConvertSpansDuration(t *testing.T) {
	const start = uint64(1790975977787706000)

	cases := map[string]struct {
		start            uint64
		end              uint64
		expectedDuration uint64
		expectedDropped  bool
	}{
		"an ordinary span": {
			start:            start,
			end:              1790975977789242000,
			expectedDuration: 1536,
		},
		"an end equal to the start": {
			start:            start,
			end:              start,
			expectedDuration: 0,
		},
		"an end before the start": {
			start:            start,
			end:              1790975977787705000,
			expectedDuration: 0,
		},
		"an absent end": {
			start:            start,
			end:              0,
			expectedDuration: 0,
		},
		"an absent start drops the span": {
			start:           0,
			end:             1790975977789242000,
			expectedDropped: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spans := []*tracev1.Span{{
				Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
				StartTimeUnixNano: tc.start,
				EndTimeUnixNano:   tc.end,
			}}

			converted := ConvertSpans(spans, "reviews.bookinfo", getId())
			if tc.expectedDropped {
				assert.Empty(t, converted)
				return
			}

			require.Len(t, converted, 1)
			assert.Equal(t, tc.expectedDuration, converted[0].Duration)
		})
	}
}

// TestConvertAttributesUnreadVariant covers an attribute whose value uses an AnyValue variant this
// build cannot read: string_value_strindex, which the proto reserves for the Profiling signal. The
// tag arrives with an empty value and the warning names its key, which is what the proto's own
// comment asks any other receiver to do.
//
// The warning is per span, not per attribute: one line naming every key.
func TestConvertAttributesUnreadVariant(t *testing.T) {
	strindex := &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValueStrindex{StringValueStrindex: 3}}

	attributes := []*commonv1.KeyValue{
		{Key: "probe.strindex", Value: strindex},
		{Key: "http.method", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "GET"}}},
		{Key: "probe.other", Value: strindex},
		// an unset value is legal OTLP and must not be warned about
		{Key: "probe.unset", Value: &commonv1.AnyValue{}},
	}

	buf := &bytes.Buffer{}
	restore := zlog.Logger
	zlog.Logger = zlog.Logger.Output(buf)
	defer func() { zlog.Logger = restore }()

	tags := convertAttributes(attributes)

	require.Len(t, tags, 4)
	assert.Equal(t, "", tags[0].Value)
	assert.Equal(t, "GET", tags[1].Value)

	logged := buf.String()
	assert.Equal(t, 1, strings.Count(logged, "Could not read the value of span attribute"), logged)
	assert.Contains(t, logged, "probe.strindex, probe.other")
	assert.NotContains(t, logged, "probe.unset")
	assert.NotContains(t, logged, "http.method")
}

func getId() string {
	id := "727a0d200236314473666c051e6f65f4"
	return id
}

func getSpans() []*tracev1.Span {
	traceID, _ := hex.DecodeString(getId())
	spanID, _ := hex.DecodeString("49cd77d1f9dcd936")
	parentSpanID, _ := hex.DecodeString("1234567890abcdef")

	return []*tracev1.Span{{
		TraceId:           traceID,
		SpanId:            spanID,
		ParentSpanId:      parentSpanID,
		Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
		Kind:              tracev1.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: 1693389472310270000,
		EndTimeUnixNano:   1693389472310916000,
		Attributes:        getAttributes(),
		Events:            []*tracev1.Span_Event{},
		Status:            &tracev1.Status{},
	}}
}

func getAttributes() []*commonv1.KeyValue {
	return []*commonv1.KeyValue{
		{Key: "guid:x-request-id", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "48c7189e-1e39-9984-9556-20a8f2e8be45"}}},
		{Key: "ttp.protocol\"", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "HTTP/1.1"}}},
	}
}
