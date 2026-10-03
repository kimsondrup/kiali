package converter

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kiali/kiali/log"
	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	otel "github.com/kiali/kiali/tracing/otel/model"
	"github.com/kiali/kiali/tracing/tempo/tempopb"
	v1 "github.com/kiali/kiali/tracing/tempo/tempopb/common/v1"
	v11 "github.com/kiali/kiali/tracing/tempo/tempopb/resource/v1"
)

// convertID
func ConvertId(id string) jaegerModels.TraceID {
	return jaegerModels.TraceID(id)
}

// convertSpanId renders a span id as the hex text every Kiali response reports an id as, and
// which Tempo's search API already answers with. The proto declares the id as the 8 bytes it
// is, so the text is made here rather than copied from whatever the backend wrote.
func convertSpanId(id []byte) jaegerModels.SpanID {
	return jaegerModels.SpanID(hex.EncodeToString(id))
}

// ConvertSpans
// https://opentelemetry.io/docs/specs/otel/trace/sdk_exporters/jaeger
func ConvertSpans(spans []*tracev1.Span, scope *commonv1.InstrumentationScope, serviceName string, traceID string) []jaegerModels.Span {
	var toRet []jaegerModels.Span
	scopeTags := convertScope(scope)
	for _, span := range spans {
		// a span with no start time is not placed anywhere on a timeline, and the OTLP proto
		// requires one, so there is nothing to report it as
		if span.GetStartTimeUnixNano() == 0 {
			log.Errorf("Span has no start time. Skipping span")
			continue
		}

		// the span carries its own trace id, and it is readable now, but the response answers a
		// request for one trace and is keyed throughout by the id that was asked for
		jaegerTraceId := ConvertId(traceID)
		jaegerSpanId := convertSpanId(span.GetSpanId())
		parentSpanId := convertSpanId(span.GetParentSpanId())

		jaegerSpan := jaegerModels.Span{
			TraceID:   jaegerTraceId,
			SpanID:    jaegerSpanId,
			Duration:  getDuration(span),
			StartTime: span.GetStartTimeUnixNano() / 1000,
			// No more mapped data
			Flags:         0,
			OperationName: span.GetName(),
			References:    convertReferences(jaegerTraceId, parentSpanId),
			Tags:          append(convertAttributes(span.GetAttributes(), span.GetStatus().GetCode()), scopeTags...),
			Logs:          []jaegerModels.Log{},
			ProcessID:     "",
			Process:       &jaegerModels.Process{Tags: []jaegerModels.KeyValue{}, ServiceName: serviceName},
			Warnings:      []string{},
		}

		// This is how Jaeger reports it
		// Used to determine the envoy direction
		atb_val := ""
		switch span.GetKind() {
		case tracev1.Span_SPAN_KIND_CLIENT:
			atb_val = "client"
		case tracev1.Span_SPAN_KIND_SERVER:
			atb_val = "server"
		}
		if atb_val != "" {
			atb := jaegerModels.KeyValue{Key: "span.kind", Value: atb_val, Type: jaegerModels.StringType}
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
	operationName := rootName
	if span.Name != "" {
		operationName = span.Name
	}

	jaegerSpan := jaegerModels.Span{
		TraceID: jaegerTraceId,
		// Tempo's search API reports the span id as hex text already
		SpanID:    jaegerModels.SpanID(span.SpanID),
		Duration:  duration / 1000, // Provided in ns, Jaeger uses ms
		StartTime: startTime / 1000,
		// No more mapped data
		Flags: 0,
		// OperationName: span.Name,
		References:    []jaegerModels.Reference{},
		Tags:          convertAttributes(span.Attributes, span.Status.Code),
		Logs:          []jaegerModels.Log{},
		OperationName: operationName,
		ProcessID:     "",
		Process:       &jaegerModels.Process{Tags: []jaegerModels.KeyValue{}, ServiceName: serviceName},
		Warnings:      []string{},
	}

	toRet = append(toRet, jaegerSpan)

	return toRet
}

// getDuration returns the span's duration in microseconds, which Jaeger reports it in.
func getDuration(span *tracev1.Span) uint64 {
	start, end := span.GetStartTimeUnixNano(), span.GetEndTimeUnixNano()
	// The subtraction is unsigned, and the OTLP proto only says that the end time is expected to
	// be at or after the start time. An end before the start, an absent end among it, wraps the
	// result round to several hundred years. Jaeger sanitizes the same case by moving the end up
	// to the start and warning on the span; a zero duration says the same thing and keeps the
	// span, which is better than dropping it - a span with a bad end time still carries its
	// name, its service and its tags.
	if end < start {
		log.Warningf("Span end time %d is before its start time %d, reporting a zero duration", end, start)
		return 0
	}
	// nano to micro
	return (end - start) / 1000
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

// convertScope reports an OTLP instrumentation scope as the span tags the OpenTelemetry mapping
// to non-OTLP formats defines for it. Jaeger's own translator reports the same two tags and
// leaves out a name or a version that is empty, which the mapping says means unknown.
// https://opentelemetry.io/docs/specs/otel/common/mapping-to-non-otlp/#instrumentationscope
func convertScope(scope *commonv1.InstrumentationScope) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue
	if scope.GetName() != "" {
		tags = append(tags, jaegerModels.KeyValue{Key: "otel.scope.name", Value: scope.GetName(), Type: jaegerModels.StringType})
	}
	if scope.GetVersion() != "" {
		tags = append(tags, jaegerModels.KeyValue{Key: "otel.scope.version", Value: scope.GetVersion(), Type: jaegerModels.StringType})
	}
	return tags
}

func convertAttributes(attributes []*commonv1.KeyValue, status tracev1.Status_StatusCode) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue
	for _, atb := range attributes {
		if atb.GetKey() == "status" && atb.GetValue().GetStringValue() == "error" {
			tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType}
			tags = append(tags, tag)
		} else {
			value, valueType := attributeValue(atb.GetValue())
			tag := jaegerModels.KeyValue{Key: atb.GetKey(), Value: value, Type: valueType}
			tags = append(tags, tag)
		}
	}
	// When Span Status is set to ERROR, an error span tag MUST be added with the Boolean value of true
	if status == tracev1.Status_STATUS_CODE_ERROR {
		tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType}
		tags = append(tags, tag)
	}
	return tags
}

// attributeValue maps an OTLP attribute value onto a Jaeger tag value and the tag type that
// describes it. Jaeger's own API reports typed tags for the same attributes, and the frontend
// reads some of them as numbers and booleans rather than as text.
func attributeValue(value *commonv1.AnyValue) (any, jaegerModels.ValueType) {
	switch plain := plainValue(value).(type) {
	case string:
		return plain, jaegerModels.StringType
	case bool:
		return plain, jaegerModels.BoolType
	case int64:
		return plain, jaegerModels.Int64Type
	case float64:
		// JSON has no NaN and no infinity, so a double that is not a finite number is reported as
		// its text: a number tag holding one could not be encoded into Kiali's own response.
		if math.IsNaN(plain) || math.IsInf(plain, 0) {
			return strconv.FormatFloat(plain, 'g', -1, 64), jaegerModels.StringType
		}
		return plain, jaegerModels.Float64Type
	case []byte:
		return plain, jaegerModels.BinaryType
	case nil:
		// no variant set, or the one the proto reserves for profiling, which a receiver of any
		// other signal is told to read as though the value were absent
		return "", jaegerModels.StringType
	default:
		// an array or a map, which Jaeger's own OTLP translation renders as JSON as well
		text, err := json.Marshal(plain)
		if err != nil {
			log.Errorf("Error rendering attribute value: %s", err.Error())
			return "", jaegerModels.StringType
		}
		return string(text), jaegerModels.StringType
	}
}

// plainValue returns an OTLP attribute value as a plain Go value, nested values included, and
// nil when no variant of it is set.
func plainValue(value *commonv1.AnyValue) any {
	switch variant := value.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return variant.StringValue
	case *commonv1.AnyValue_BoolValue:
		return variant.BoolValue
	case *commonv1.AnyValue_IntValue:
		return variant.IntValue
	case *commonv1.AnyValue_DoubleValue:
		return variant.DoubleValue
	case *commonv1.AnyValue_BytesValue:
		return variant.BytesValue
	case *commonv1.AnyValue_ArrayValue:
		items := variant.ArrayValue.GetValues()
		values := make([]any, 0, len(items))
		for _, item := range items {
			values = append(values, plainValue(item))
		}
		return values
	case *commonv1.AnyValue_KvlistValue:
		items := variant.KvlistValue.GetValues()
		values := make(map[string]any, len(items))
		for _, item := range items {
			values[item.GetKey()] = plainValue(item.GetValue())
		}
		return values
	}
	return nil
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
