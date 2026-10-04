package converter

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	otelModels "github.com/kiali/kiali/tracing/otel/model/json"
	"github.com/kiali/kiali/tracing/tempo/tempopb"
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

	id := getId()
	jaegerId := convertSpanId(id)
	assert.Equal(jaegerModels.SpanID(id), jaegerId)
}

func TestConvertSpans(t *testing.T) {
	assert := assert.New(t)

	spans := getSpans()
	id := getId()
	serviceName := "kiali-traffic-generator.bookinfo"

	jaegerSpans := ConvertSpans(spans, serviceName, id)
	assert.Equal(jaegerModels.SpanID(id), jaegerSpans[0].SpanID)
	assert.Equal(serviceName, jaegerSpans[0].Process.ServiceName)
	assert.Equal("reviews.bookinfo.svc.cluster.local:9080/*", jaegerSpans[0].OperationName)
}

func getId() string {
	id := "727a0d200236314473666c051e6f65f4"
	return id
}

func getSpans() []otelModels.Span {
	var spans []otelModels.Span

	attbs := getAttributes()

	span := otelModels.Span{
		TraceID:           getId(),
		SpanID:            getId(),
		Name:              "reviews.bookinfo.svc.cluster.local:9080/*",
		Kind:              "SPAN_KIND_SERVER",
		StartTimeUnixNano: "1693389472310270000",
		EndTimeUnixNano:   "1693389472310916000",
		Attributes:        attbs,
		Events:            []otelModels.Event{},
		Status:            otelModels.Status{},
	}

	spans = append(spans, span)

	return spans
}

func getAttributes() []otelModels.Attribute {
	var attbs []otelModels.Attribute
	atb1 := otelModels.Attribute{Key: "guid:x-request-id", Value: otelModels.ValueString{StringValue: "48c7189e-1e39-9984-9556-20a8f2e8be45"}}
	atb2 := otelModels.Attribute{Key: "ttp.protocol\"", Value: otelModels.ValueString{StringValue: "HTTP/1.1"}}

	attbs = append(attbs, atb1)
	attbs = append(attbs, atb2)

	return attbs
}

// TestConvertModelAttributes covers the same attributes on the Tempo gRPC path, which reads them
// from Tempo's own generated OTLP types. Each one has to arrive with its value and the type that
// describes it, the same as on the HTTP path.
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

// TestConvertModelAttributesStatus checks that the status attribute Tempo selects is read the same
// way on both paths. An error becomes the error tag; anything else stays a tag of its own.
func TestConvertModelAttributesStatus(t *testing.T) {
	failed := []*v1.KeyValue{{Key: "status", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "error"}}}}
	assert.Equal(t, []jaegerModels.KeyValue{{Key: "error", Value: true, Type: jaegerModels.BoolType}},
		convertModelAttributes(failed))

	unset := []*v1.KeyValue{{Key: "status", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "unset"}}}}
	assert.Equal(t, []jaegerModels.KeyValue{{Key: "status", Value: "unset", Type: jaegerModels.StringType}},
		convertModelAttributes(unset))
}

// TestConvertModelAttributesNestedNonFinite covers a double that JSON has no literal for, nested
// inside an array. json.Marshal refuses the whole value such a double sits in, so every element of
// that attribute is lost unless the double is carried as text.
func TestConvertModelAttributesNestedNonFinite(t *testing.T) {
	attributes := []*v1.KeyValue{{Key: "sampler.params", Value: &v1.AnyValue{Value: &v1.AnyValue_ArrayValue{
		ArrayValue: &v1.ArrayValue{Values: []*v1.AnyValue{
			{Value: &v1.AnyValue_DoubleValue{DoubleValue: 0.25}},
			{Value: &v1.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}},
		}},
	}}}}

	tags := convertModelAttributes(attributes)
	require.Len(t, tags, 1)
	assert.Equal(t, jaegerModels.StringType, tags[0].Type)
	assert.Equal(t, `[0.25,"+Inf"]`, tags[0].Value)
}

// TestConvertTraceMetadataNoSpanSet covers a trace in the search stream whose spanSet is unset.
// The field is a pointer, and Tempo's own proto marks it deprecated in favour of spanSets, so a
// Tempo that stops writing it hands Kiali a nil here - and every trace of the stream goes
// through this function.
func TestConvertTraceMetadataNoSpanSet(t *testing.T) {
	trace, err := ConvertTraceMetadata(tempopb.TraceSearchMetadata{TraceID: getId()}, "reviews.bookinfo")
	require.NoError(t, err)
	require.NotNil(t, trace)
	assert.Empty(t, trace.Spans)
	assert.Equal(t, 0, trace.Matched)
}
