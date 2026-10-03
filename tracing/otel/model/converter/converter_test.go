package converter

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	otelModels "github.com/kiali/kiali/tracing/otel/model/json"
	"github.com/kiali/kiali/util"
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

// TestConvertAttributes checks that an OTLP attribute keeps its type when it becomes a Jaeger
// tag. Jaeger reports typed tags for the same attributes, and the UI reads some of them as
// numbers and booleans rather than as text.
func TestConvertAttributes(t *testing.T) {
	cases := map[string]struct {
		attribute     otelModels.Attribute
		expectedValue any
		expectedType  jaegerModels.ValueType
	}{
		"a string": {
			attribute:     otelModels.Attribute{Key: "http.method", Value: otelModels.AnyValue{StringValue: util.AsPtr("GET")}},
			expectedValue: "GET",
			expectedType:  jaegerModels.StringType,
		},
		"an integer": {
			attribute:     otelModels.Attribute{Key: "http.response.status_code", Value: otelModels.AnyValue{IntValue: util.AsPtr(int64(503))}},
			expectedValue: int64(503),
			expectedType:  jaegerModels.Int64Type,
		},
		"a boolean": {
			attribute:     otelModels.Attribute{Key: "error", Value: otelModels.AnyValue{BoolValue: util.AsPtr(true)}},
			expectedValue: true,
			expectedType:  jaegerModels.BoolType,
		},
		"a double": {
			attribute:     otelModels.Attribute{Key: "sampler.param", Value: otelModels.AnyValue{DoubleValue: util.AsPtr(0.25)}},
			expectedValue: 0.25,
			expectedType:  jaegerModels.Float64Type,
		},
		"a double that is not a finite number is reported as text": {
			attribute:     otelModels.Attribute{Key: "queue.ratio", Value: otelModels.AnyValue{DoubleValue: util.AsPtr(math.NaN())}},
			expectedValue: "NaN",
			expectedType:  jaegerModels.StringType,
		},
		"bytes": {
			attribute:     otelModels.Attribute{Key: "raw", Value: otelModels.AnyValue{BytesValue: util.AsPtr("YWJj")}},
			expectedValue: "YWJj",
			expectedType:  jaegerModels.BinaryType,
		},
		"an array is rendered as JSON": {
			attribute: otelModels.Attribute{Key: "http.request.header.accept", Value: otelModels.AnyValue{ArrayValue: &otelModels.ArrayValue{
				Values: []otelModels.AnyValue{{StringValue: util.AsPtr("application/json")}},
			}}},
			expectedValue: `["application/json"]`,
			expectedType:  jaegerModels.StringType,
		},
		"a kvlist is rendered as JSON": {
			attribute: otelModels.Attribute{Key: "peer", Value: otelModels.AnyValue{KvlistValue: &otelModels.KeyValueList{
				Values: []otelModels.Attribute{{Key: "port", Value: otelModels.AnyValue{IntValue: util.AsPtr(int64(9080))}}},
			}}},
			expectedValue: `{"port":9080}`,
			expectedType:  jaegerModels.StringType,
		},
		"a value with no variant set": {
			attribute:     otelModels.Attribute{Key: "empty", Value: otelModels.AnyValue{}},
			expectedValue: "",
			expectedType:  jaegerModels.StringType,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tags := convertAttributes([]otelModels.Attribute{tc.attribute}, otelModels.Status{})
			assert.Len(t, tags, 1)
			assert.Equal(t, tc.attribute.Key, tags[0].Key)
			assert.Equal(t, tc.expectedValue, tags[0].Value)
			assert.Equal(t, tc.expectedType, tags[0].Type)
		})
	}
}

// TestConvertAttributesError checks the two ways a span is reported as being in error.
func TestConvertAttributesError(t *testing.T) {
	cases := map[string]struct {
		attributes []otelModels.Attribute
		status     otelModels.Status
	}{
		"a status attribute": {
			attributes: []otelModels.Attribute{{Key: "status", Value: otelModels.AnyValue{StringValue: util.AsPtr("error")}}},
		},
		"the span status code": {
			status: otelModels.Status{Code: otelModels.StatusCodeError},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tags := convertAttributes(tc.attributes, tc.status)
			assert.Len(t, tags, 1)
			assert.Equal(t, jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType}, tags[0])
		})
	}
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
		Kind:              otelModels.SpanKindServer,
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
	atb1 := otelModels.Attribute{Key: "guid:x-request-id", Value: otelModels.AnyValue{StringValue: util.AsPtr("48c7189e-1e39-9984-9556-20a8f2e8be45")}}
	atb2 := otelModels.Attribute{Key: "ttp.protocol\"", Value: otelModels.AnyValue{StringValue: util.AsPtr("HTTP/1.1")}}

	attbs = append(attbs, atb1)
	attbs = append(attbs, atb2)

	return attbs
}
