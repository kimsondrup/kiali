package converter

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/kiali/kiali/log"
	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	otel "github.com/kiali/kiali/tracing/otel/model"
	otelModels "github.com/kiali/kiali/tracing/otel/model/json"
	"github.com/kiali/kiali/tracing/tempo/tempopb"
	v1 "github.com/kiali/kiali/tracing/tempo/tempopb/common/v1"
	v11 "github.com/kiali/kiali/tracing/tempo/tempopb/resource/v1"
)

// convertID
func ConvertId(id string) jaegerModels.TraceID {
	return jaegerModels.TraceID(id)
}

// convertSpanId
func convertSpanId(id string) jaegerModels.SpanID {
	return jaegerModels.SpanID(id)
}

// ConvertSpans
// https://opentelemetry.io/docs/specs/otel/trace/sdk_exporters/jaeger
func ConvertSpans(spans []otelModels.Span, serviceName string, traceID string) []jaegerModels.Span {
	var toRet []jaegerModels.Span
	for _, span := range spans {

		startTime, err := strconv.ParseUint(span.StartTimeUnixNano, 10, 64)
		if err != nil {
			log.Errorf("Error converting start time. Skipping trace")
			continue
		}

		duration, err := getDuration(span.EndTimeUnixNano, span.StartTimeUnixNano)
		if err != nil {
			log.Errorf("Error converting duration. Skipping trace")
			continue
		}
		jaegerTraceId := ConvertId(traceID) // The traceID from the SpanID doesn't look to match (ex. Q3xfr1lMsbi2OX9CxUbYug==)
		jaegerSpanId := convertSpanId(span.SpanID)
		parentSpanId := convertSpanId(span.ParentSpanId)

		jaegerSpan := jaegerModels.Span{
			TraceID:   jaegerTraceId,
			SpanID:    jaegerSpanId,
			Duration:  duration,
			StartTime: startTime / 1000,
			// No more mapped data
			Flags:         0,
			OperationName: span.Name,
			References:    convertReferences(jaegerTraceId, parentSpanId),
			Tags:          convertAttributes(span.Attributes, span.Status),
			Logs:          []jaegerModels.Log{},
			ProcessID:     "",
			Process:       &jaegerModels.Process{Tags: []jaegerModels.KeyValue{}, ServiceName: serviceName},
			Warnings:      []string{},
		}

		// This is how Jaeger reports it
		// Used to determine the envoy direction
		atb_val := ""
		switch span.Kind {
		case "SPAN_KIND_CLIENT":
			atb_val = "client"
		case "SPAN_KIND_SERVER":
			atb_val = "server"
		}
		if atb_val != "" {
			atb := jaegerModels.KeyValue{Key: "span.kind", Value: atb_val, Type: "string"}
			jaegerSpan.Tags = append(jaegerSpan.Tags, atb)
		}

		toRet = append(toRet, jaegerSpan)
	}
	return toRet
}

// ConvertTraceMetadata used by the GRPC Client
func ConvertTraceMetadata(trace tempopb.TraceSearchMetadata, serviceName string) (*jaegerModels.Trace, error) {
	jaegerTrace := jaegerModels.Trace{
		TraceID:   ConvertId(trace.TraceID),
		Processes: map[jaegerModels.ProcessID]jaegerModels.Process{},
		Warnings:  []string{},
	}
	// SpanSet is a pointer and Tempo leaves it unset for a trace it matched without reporting a
	// span of it, so it is read through its getter rather than indexed into
	for _, span := range trace.GetSpanSet().GetSpans() {
		spanSet := convertOtelSpan(span, serviceName, trace.TraceID, trace.RootTraceName)
		jaegerTrace.Spans = append(jaegerTrace.Spans, spanSet)
	}
	jaegerTrace.Matched = len(jaegerTrace.Spans)
	return &jaegerTrace, nil
}

// convertOtelSpan used for GRPC format Spans
func convertOtelSpan(span *tempopb.Span, serviceName, traceID, rootTrace string) jaegerModels.Span {
	modelSpan := jaegerModels.Span{
		SpanID:    jaegerModels.SpanID(span.SpanID),
		TraceID:   jaegerModels.TraceID(traceID),
		Duration:  span.DurationNanos / 1000,
		StartTime: span.StartTimeUnixNano / 1000,
		// No more mapped data
		Flags:         0,
		References:    []jaegerModels.Reference{}, // convertReferences(traceID, rootTrace),
		Tags:          convertModelAttributes(span.Attributes),
		Logs:          []jaegerModels.Log{},
		OperationName: rootTrace,
		ProcessID:     "",
		Process:       &jaegerModels.Process{Tags: []jaegerModels.KeyValue{}, ServiceName: serviceName},
		Warnings:      []string{},
	}

	return modelSpan
}

func ConvertSpanSet(span otel.Span, serviceName string, traceId string, rootName string) []jaegerModels.Span {
	var toRet []jaegerModels.Span

	startTime, err := strconv.ParseUint(span.StartTimeUnixNano, 10, 64)
	if err != nil {
		log.Errorf("Error converting start time.")
	}
	duration, err := strconv.ParseUint(span.DurationNanos, 10, 64)
	if err != nil {
		log.Errorf("Error converting duration.")
	}

	jaegerTraceId := ConvertId(traceId)
	jaegerSpanId := convertSpanId(span.SpanID)
	operationName := rootName
	if span.Name != "" {
		operationName = span.Name
	}

	jaegerSpan := jaegerModels.Span{
		TraceID:   jaegerTraceId,
		SpanID:    jaegerSpanId,
		Duration:  duration / 1000, // Provided in ns, Jaeger uses ms
		StartTime: startTime / 1000,
		// No more mapped data
		Flags: 0,
		// OperationName: span.Name,
		References:    []jaegerModels.Reference{},
		Tags:          convertAttributes(span.Attributes, span.Status),
		Logs:          []jaegerModels.Log{},
		OperationName: operationName,
		ProcessID:     "",
		Process:       &jaegerModels.Process{Tags: []jaegerModels.KeyValue{}, ServiceName: serviceName},
		Warnings:      []string{},
	}

	toRet = append(toRet, jaegerSpan)

	return toRet
}

func getDuration(end string, start string) (uint64, error) {
	endInt, err := strconv.ParseUint(end, 10, 64)
	if err != nil {
		log.Errorf("Error converting end date: %s", err.Error())
		return 0, err
	}
	startInt, err := strconv.ParseUint(start, 10, 64)
	if err != nil {
		log.Errorf("Error converting start date: %s", err.Error())
		return 0, err
	}
	// nano to micro
	return (endInt - startInt) / 1000, nil
}

func convertReferences(traceId jaegerModels.TraceID, parentSpanId jaegerModels.SpanID) []jaegerModels.Reference {
	var references []jaegerModels.Reference

	if parentSpanId == "" {
		return references
	}

	ref := jaegerModels.Reference{
		RefType: jaegerModels.ReferenceType("CHILD_OF"),
		TraceID: traceId,
		SpanID:  parentSpanId,
	}

	references = append(references, ref)
	return references
}

func convertAttributes(attributes []otelModels.Attribute, status otelModels.Status) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue
	for _, atb := range attributes {
		if atb.Key == "status" && atb.Value.StringValue == "error" {
			tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: "bool"}
			tags = append(tags, tag)
		} else {
			tag := jaegerModels.KeyValue{Key: atb.Key, Value: atb.Value.StringValue, Type: "string"}
			tags = append(tags, tag)
		}
	}
	// When Span Status is set to ERROR, an error span tag MUST be added with the Boolean value of true
	if status.Code == "STATUS_CODE_ERROR" {
		tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: "bool"}
		tags = append(tags, tag)
	}
	return tags
}

// convertModelAttributes reports the attributes of a span matched by Tempo's search API, as the
// gRPC stream carries them, as Jaeger-shaped span tags.
//
// An OTLP attribute value is a oneof of seven variants, and Tempo's generated types declare all
// seven. Reading it with GetStringValue alone answers the empty string for the other six, and
// pinning the tag type to "string" then says that empty string is what the attribute holds.
func convertModelAttributes(attributes []*v1.KeyValue) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue
	for _, atb := range attributes {
		// the TraceQL status intrinsic, which prepareTraceQL selects, arrives as an attribute
		// rather than as a span status, and Kiali's frontend reads a failed span as error=true
		if atb.GetKey() == "status" && atb.GetValue().GetStringValue() == "error" {
			tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType}
			tags = append(tags, tag)
		} else {
			value, valueType := attributeValue(plainModelValue(atb.GetValue()))
			tag := jaegerModels.KeyValue{Key: atb.GetKey(), Value: value, Type: valueType}
			tags = append(tags, tag)
		}
	}
	return tags
}

// attributeValue maps an OTLP attribute value, as plainModelValue returns it, onto a Jaeger tag
// value and the tag type that describes it.
//
// The type is carried rather than stringified because the attribute arrives with it: OTLP states
// which variant was written, Kiali's tag model has a field for it, and the same tag types are
// what Kiali's Jaeger provider already reports for the same attributes.
func attributeValue(value any) (any, jaegerModels.ValueType) {
	switch plain := value.(type) {
	case string:
		return plain, jaegerModels.StringType
	case bool:
		return plain, jaegerModels.BoolType
	case int64:
		return plain, jaegerModels.Int64Type
	case float64:
		// Finite by construction: plainModelValue hands a non-finite double over as text, because one
		// nested in an array or a map fails json.Marshal below.
		return plain, jaegerModels.Float64Type
	case []byte:
		return plain, jaegerModels.BinaryType
	case nil:
		// no variant set at all, which is legal OTLP and says what a missing value says
		return "", jaegerModels.StringType
	default:
		// an array or a map, which Jaeger's own OTLP translation renders as JSON as well
		text, err := json.Marshal(plain)
		if err != nil {
			log.Errorf("Could not render an attribute value of type %T: %s", plain, err)
			return "", jaegerModels.StringType
		}
		return string(text), jaegerModels.StringType
	}
}

// plainModelValue returns an OTLP attribute value as a plain Go value, nested values included, and
// nil when no variant of it is set.
//
// A double that is not a finite number comes back as its text rather than as a float64. JSON has no
// literal for NaN or an infinity, so json.Marshal refuses the whole value such a double sits in: as
// a float64, a single NaN inside an array costs that attribute every one of its elements.
func plainModelValue(value *v1.AnyValue) any {
	switch variant := value.GetValue().(type) {
	case *v1.AnyValue_StringValue:
		return variant.StringValue
	case *v1.AnyValue_BoolValue:
		return variant.BoolValue
	case *v1.AnyValue_IntValue:
		return variant.IntValue
	case *v1.AnyValue_DoubleValue:
		return finiteOrText(variant.DoubleValue)
	case *v1.AnyValue_BytesValue:
		return variant.BytesValue
	case *v1.AnyValue_ArrayValue:
		items := variant.ArrayValue.GetValues()
		values := make([]any, 0, len(items))
		for _, item := range items {
			values = append(values, plainModelValue(item))
		}
		return values
	case *v1.AnyValue_KvlistValue:
		items := variant.KvlistValue.GetValues()
		values := make(map[string]any, len(items))
		for _, item := range items {
			values[item.GetKey()] = plainModelValue(item.GetValue())
		}
		return values
	}
	return nil
}

// finiteOrText returns a double as itself, or as its text when it is NaN or an infinity, so that
// the value can be written as JSON wherever it appears, nested or not.
func finiteOrText(value float64) any {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	return value
}

func ConvertResource(resourceSpans *v11.Resource) jaegerModels.Span {
	span := jaegerModels.Span{}
	return span
}
