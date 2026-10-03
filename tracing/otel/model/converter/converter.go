package converter

import (
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

		startTime, err := strconv.ParseUint(string(span.StartTimeUnixNano), 10, 64)
		if err != nil {
			log.Errorf("Error converting start time. Skipping trace")
			continue
		}

		duration, err := getDuration(string(span.EndTimeUnixNano), string(span.StartTimeUnixNano))
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
		case otelModels.SpanKindClient:
			atb_val = "client"
		case otelModels.SpanKindServer:
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
	for _, span := range trace.SpanSet.Spans {
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
		if atb.Key == "status" && atb.Value.String() == "error" {
			tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: "bool"}
			tags = append(tags, tag)
		} else {
			value, valueType := attributeValue(atb.Value)
			tag := jaegerModels.KeyValue{Key: atb.Key, Value: value, Type: valueType}
			tags = append(tags, tag)
		}
	}
	// When Span Status is set to ERROR, an error span tag MUST be added with the Boolean value of true
	if status.Code == otelModels.StatusCodeError {
		tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: "bool"}
		tags = append(tags, tag)
	}
	return tags
}

// attributeValue maps an OTLP attribute value onto a Jaeger tag value and the tag type that
// describes it. Jaeger's own API reports typed tags for the same attributes, and the UI reads
// some of them as numbers and booleans rather than as text.
func attributeValue(value otelModels.AnyValue) (any, jaegerModels.ValueType) {
	switch {
	case value.BoolValue != nil:
		return *value.BoolValue, jaegerModels.BoolType
	case value.IntValue != nil:
		return *value.IntValue, jaegerModels.Int64Type
	case value.DoubleValue != nil:
		// JSON has no NaN and no infinity, so a double that is not a finite number is reported
		// as its text. A number tag holding one could not be encoded into Kiali's own response.
		if math.IsNaN(*value.DoubleValue) || math.IsInf(*value.DoubleValue, 0) {
			return value.String(), jaegerModels.StringType
		}
		return *value.DoubleValue, jaegerModels.Float64Type
	case value.BytesValue != nil:
		return *value.BytesValue, jaegerModels.BinaryType
	}
	// a string, an array or a kvlist, all of which Jaeger reports as a string tag too
	return value.String(), jaegerModels.StringType
}

func convertModelAttributes(attributes []*v1.KeyValue) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue
	for _, atb := range attributes {
		if atb.Key == "status" {
			if atb.Value.GetStringValue() == "error" {
				tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: "bool"}
				tags = append(tags, tag)
			}
		} else {
			tag := jaegerModels.KeyValue{Key: atb.Key, Value: atb.Value.GetStringValue(), Type: "string"}
			tags = append(tags, tag)
		}
	}
	return tags
}

func ConvertResource(resourceSpans *v11.Resource) jaegerModels.Span {
	span := jaegerModels.Span{}
	return span
}
