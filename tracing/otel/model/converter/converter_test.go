package converter

import (
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	v1 "github.com/kiali/kiali/tracing/tempo/tempopb/common/v1"
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

	jaegerSpans := ConvertSpans(spans, nil, serviceName, id)
	assert.Equal(jaegerModels.SpanID("49cd77d1f9dcd936"), jaegerSpans[0].SpanID)
	assert.Equal(serviceName, jaegerSpans[0].Process.ServiceName)
	assert.Equal("reviews.bookinfo.svc.cluster.local:9080/*", jaegerSpans[0].OperationName)
	assert.Equal(uint64(646), jaegerSpans[0].Duration)
	assert.Equal(jaegerModels.TraceID(id), jaegerSpans[0].References[0].TraceID)
	assert.Equal(jaegerModels.SpanID("1234567890abcdef"), jaegerSpans[0].References[0].SpanID)
}

// TestConvertSpansDuration checks the duration arithmetic. The subtraction is between two
// unsigned nanosecond timestamps, so an end that is not after the start used to wrap round to
// hundreds of years: without the guard, the absent end below reports 16655768095921845us, or
// 528 years, and the end one microsecond early reports 18446744073709550us. Neither shape has
// been seen in a Tempo response; both are values the proto permits.
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

			converted := ConvertSpans(spans, nil, "reviews.bookinfo", getId())
			if tc.expectedDropped {
				assert.Empty(t, converted)
				return
			}

			assert.Len(t, converted, 1)
			assert.Equal(t, tc.expectedDuration, converted[0].Duration)
		})
	}
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
			tags := convertAttributes(attributes, tracev1.Status_STATUS_CODE_UNSET)

			assert.Len(t, tags, 1)
			assert.Equal(t, "k", tags[0].Key)
			assert.Equal(t, tc.wantValue, tags[0].Value)
			assert.Equal(t, tc.wantType, tags[0].Type)
		})
	}
}

// TestConvertAttributesError covers the two ways a span says it failed: the status attribute
// Tempo's search API selects, and the span status the OTLP proto carries.
func TestConvertAttributesError(t *testing.T) {
	errorTag := jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType}

	attribute := []*commonv1.KeyValue{
		{Key: "status", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "error"}}},
	}
	assert.Equal(t, []jaegerModels.KeyValue{errorTag}, convertAttributes(attribute, tracev1.Status_STATUS_CODE_UNSET))

	unset := []*commonv1.KeyValue{
		{Key: "status", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "unset"}}},
	}
	assert.Equal(t, []jaegerModels.KeyValue{{Key: "status", Value: "unset", Type: jaegerModels.StringType}},
		convertAttributes(unset, tracev1.Status_STATUS_CODE_UNSET))

	assert.Equal(t, []jaegerModels.KeyValue{errorTag}, convertAttributes(nil, tracev1.Status_STATUS_CODE_ERROR))
	assert.Nil(t, convertAttributes(nil, tracev1.Status_STATUS_CODE_OK))
}

// TestConvertModelAttributes covers the same attributes on the Tempo gRPC path, which reads
// them from Tempo's own generated OTLP types. They used to be read through GetStringValue
// alone, with the tag type hard-coded to string, so a search run with use_grpc set reported
// every typed attribute as empty where the HTTP path reported it correctly.
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
		"a bool": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_BoolValue{BoolValue: true}},
			wantValue: true, wantType: jaegerModels.BoolType,
		},
		"an int": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_IntValue{IntValue: 503}},
			wantValue: int64(503), wantType: jaegerModels.Int64Type,
		},
		"a double": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_DoubleValue{DoubleValue: 1.5}},
			wantValue: 1.5, wantType: jaegerModels.Float64Type,
		},
		"bytes": {
			value:     &v1.AnyValue{Value: &v1.AnyValue_BytesValue{BytesValue: []byte("hi")}},
			wantValue: []byte("hi"), wantType: jaegerModels.BinaryType,
		},
		"an array": {
			value: &v1.AnyValue{Value: &v1.AnyValue_ArrayValue{ArrayValue: &v1.ArrayValue{
				Values: []*v1.AnyValue{
					{Value: &v1.AnyValue_StringValue{StringValue: "a"}},
					{Value: &v1.AnyValue_IntValue{IntValue: 2}},
				},
			}}},
			wantValue: `["a",2]`, wantType: jaegerModels.StringType,
		},
		"a map": {
			value: &v1.AnyValue{Value: &v1.AnyValue_KvlistValue{KvlistValue: &v1.KeyValueList{
				Values: []*v1.KeyValue{
					{Key: "k", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "v"}}},
				},
			}}},
			wantValue: `{"k":"v"}`, wantType: jaegerModels.StringType,
		},
		"no variant set": {
			value:     &v1.AnyValue{},
			wantValue: "", wantType: jaegerModels.StringType,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tags := convertModelAttributes([]*v1.KeyValue{{Key: "k", Value: tc.value}})

			assert.Len(t, tags, 1)
			assert.Equal(t, "k", tags[0].Key)
			assert.Equal(t, tc.wantValue, tags[0].Value)
			assert.Equal(t, tc.wantType, tags[0].Type)
		})
	}
}

// TestConvertModelAttributesStatus checks that the status attribute Tempo selects is read the
// same way on both paths. An error becomes the error tag; anything else stays a tag of its own,
// which the gRPC path used to drop.
func TestConvertModelAttributesStatus(t *testing.T) {
	failed := []*v1.KeyValue{{Key: "status", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "error"}}}}
	assert.Equal(t, []jaegerModels.KeyValue{{Key: "error", Value: true, Type: jaegerModels.BoolType}},
		convertModelAttributes(failed))

	unset := []*v1.KeyValue{{Key: "status", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "unset"}}}}
	assert.Equal(t, []jaegerModels.KeyValue{{Key: "status", Value: "unset", Type: jaegerModels.StringType}},
		convertModelAttributes(unset))
}

// TestConvertScope checks the two tags the OpenTelemetry mapping to non-OTLP formats asks for,
// which Jaeger's own translation of the same span reports as well.
func TestConvertScope(t *testing.T) {
	cases := map[string]struct {
		scope *commonv1.InstrumentationScope
		want  []jaegerModels.KeyValue
	}{
		"a name and a version, as Envoy exports them": {
			scope: &commonv1.InstrumentationScope{Name: "envoy", Version: "1.39.2-dev"},
			want: []jaegerModels.KeyValue{
				{Key: "otel.scope.name", Value: "envoy", Type: jaegerModels.StringType},
				{Key: "otel.scope.version", Value: "1.39.2-dev", Type: jaegerModels.StringType},
			},
		},
		"a name alone": {
			scope: &commonv1.InstrumentationScope{Name: "agentgateway"},
			want: []jaegerModels.KeyValue{
				{Key: "otel.scope.name", Value: "agentgateway", Type: jaegerModels.StringType},
			},
		},
		// an empty name means the scope is unknown, which is nothing to report
		"an empty scope, which is what Istio's own spans carry": {scope: &commonv1.InstrumentationScope{}},
		"no scope at all": {scope: nil},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, convertScope(tc.scope))

			converted := ConvertSpans(getSpans(), tc.scope, "reviews.bookinfo", getId())
			var scopeTags []jaegerModels.KeyValue
			for _, tag := range converted[0].Tags {
				if strings.HasPrefix(tag.Key, "otel.scope.") {
					scopeTags = append(scopeTags, tag)
				}
			}
			assert.Equal(t, tc.want, scopeTags)
		})
	}
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
