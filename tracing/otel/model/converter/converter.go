package converter

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"

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

// ConvertSpans reports OTLP spans as the Jaeger-shaped spans Kiali's own API returns, which is
// the model the frontend reads whichever backend the trace came from.
//
// The tag names for the OpenTelemetry fields that have no Jaeger equivalent come from the
// OpenTelemetry mapping to non-OTLP formats:
// https://opentelemetry.io/docs/specs/otel/common/mapping-to-non-otlp/
//
// A span with no start time is dropped rather than reported, so the caller gets fewer spans than
// the backend sent. The id of the trace is taken from the argument and not from the span.
func ConvertSpans(spans []*tracev1.Span, scope *commonv1.InstrumentationScope, serviceName string, traceID string) []jaegerModels.Span {
	var toRet []jaegerModels.Span
	scopeTags := convertScope(scope)
	for _, span := range spans {
		// a span with no start time is not placed anywhere on a timeline, and the OTLP proto
		// requires one, so there is nothing to report it as
		if span.GetStartTimeUnixNano() == 0 {
			log.Errorf("Span %s of trace %s on service [%s] has no start time. Skipping span",
				convertSpanId(span.GetSpanId()), traceID, serviceName)
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
			Tags:          append(append(convertAttributes(span.GetAttributes()), convertStatus(span.GetStatus())...), scopeTags...),
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
		log.Errorf("Could not read the start time %q of span %s in trace %s: %s",
			span.StartTimeUnixNano, span.SpanID, traceId, err)
	}
	duration, err := strconv.ParseUint(span.DurationNanos, 10, 64)
	if err != nil {
		log.Errorf("Could not read the duration %q of span %s in trace %s: %s",
			span.DurationNanos, span.SpanID, traceId, err)
	}

	jaegerTraceId := ConvertId(traceId)
	operationName := rootName
	if span.Name != "" {
		operationName = span.Name
	}

	jaegerSpan := jaegerModels.Span{
		TraceID: jaegerTraceId,
		// Tempo's search API reports the span id as hex text already
		SpanID: jaegerModels.SpanID(span.SpanID),
		// nano to micro, as getDuration does for the other path: Jaeger reports a duration in
		// microseconds, and this comment said milliseconds for two years.
		Duration:  duration / 1000,
		StartTime: startTime / 1000,
		// No more mapped data
		Flags: 0,
		// OperationName: span.Name,
		References:    []jaegerModels.Reference{},
		Tags:          convertAttributes(span.Attributes),
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
	// The subtraction is unsigned, and the OTLP proto only says that the end time is expected to be at
	// or after the start time. An end before the start, an absent end among them, wraps the result
	// round to several hundred years. A zero duration keeps such a span rather than dropping it: it
	// still carries its name, its service and its tags.
	if end < start {
		log.Warningf("Span %s end time %d is before its start time %d, reporting a zero duration",
			convertSpanId(span.GetSpanId()), end, start)
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

// convertScope reports an OTLP instrumentation scope as span tags. The mapping to non-OTLP formats
// requires the scope's fields to be reported as key-value pairs and names otel.scope.name and
// otel.scope.version for them, both at requirement level Recommended.
//
// It also says the deprecated otel.library.name and otel.library.version "MUST also be reported
// with exact same values for backward compatibility reasons", and those two rows are marked
// Recommended as well. Only the current pair is emitted, so that MUST is deliberately declined: the
// aliases double every scope tag on every span to serve a reader that predates them, and nothing
// Kiali ships reads either name.
// https://opentelemetry.io/docs/specs/otel/common/mapping-to-non-otlp/#instrumentationscope
//
// Skipping an empty name or version is this function's own choice, not a rule from that document -
// it prescribes nothing about an empty field. A tag whose value is the empty string says nothing a
// missing tag does not.
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

// convertStatus reports an OTLP span status as span tags. The mapping to non-OTLP formats
// requires the status to be reported as key-value pairs on the span "unless the Status is UNSET.
// In the latter case it MUST NOT be reported." It names otel.status_code, whose value is "OK" or
// "ERROR", and otel.status_description for the status message.
// https://opentelemetry.io/docs/specs/otel/common/mapping-to-non-otlp/#span-status
//
// Kiali's error=true is not one of those two names and is not a substitute for them: it says
// that a span failed and nothing about why.
//
// It is its own function rather than a parameter of convertAttributes because only one of that
// function's two callers has a status to pass: Tempo's search API answers with the TraceQL
// status intrinsic as an attribute, not with the span-level status the OTLP proto declares.
func convertStatus(status *tracev1.Status) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue

	// Jaeger's own OTLP translation adds this boolean for an errored span, and Kiali's frontend
	// reads it as the one signal that a span failed, so the Tempo path has to report it too.
	if status.GetCode() == tracev1.Status_STATUS_CODE_ERROR {
		tags = append(tags, jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType})
	}

	// the mapping prescribes the name of the status code, not its number, and names only OK and
	// ERROR; UNSET is the case it says not to report
	statusCode := ""
	switch status.GetCode() {
	case tracev1.Status_STATUS_CODE_OK:
		statusCode = "OK"
	case tracev1.Status_STATUS_CODE_ERROR:
		statusCode = "ERROR"
	}
	// an UNSET status is not reported at all, so its message is not reported either: the rule is
	// about the status, not about one of its fields
	if statusCode == "" {
		return tags
	}
	tags = append(tags, jaegerModels.KeyValue{Key: "otel.status_code", Value: statusCode, Type: jaegerModels.StringType})
	if message := status.GetMessage(); message != "" {
		tags = append(tags, jaegerModels.KeyValue{Key: "otel.status_description", Value: message, Type: jaegerModels.StringType})
	}

	return tags
}

func convertAttributes(attributes []*commonv1.KeyValue) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue
	var unread []string
	for _, atb := range attributes {
		if atb.GetKey() == "status" && atb.GetValue().GetStringValue() == "error" {
			tag := jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType}
			tags = append(tags, tag)
		} else {
			plain := plainValue(atb.GetValue())
			if plain == nil && atb.GetValue().GetValue() != nil {
				unread = append(unread, atb.GetKey())
			}
			value, valueType := attributeValue(plain)
			tag := jaegerModels.KeyValue{Key: atb.GetKey(), Value: value, Type: valueType}
			tags = append(tags, tag)
		}
	}
	warnUnreadVariants(unread)
	return tags
}

// warnUnreadVariants reports the attributes whose value uses an AnyValue variant this build cannot
// read, which arrive as a tag with an empty value. The only such variant the proto Kiali builds
// against declares is string_value_strindex, which the proto reserves for the Profiling signal and
// tells any other receiver to log about.
//
// A value with no variant set at all is not reported: an unset value is legal OTLP and says what a
// missing tag says.
//
// One line per span, naming every key, because a trace detail carries a few hundred attribute
// values and a search answer a few thousand.
func warnUnreadVariants(keys []string) {
	if len(keys) == 0 {
		return
	}
	log.Warningf("Could not read the value of span attribute(s) %s: written as an OTLP value variant this build does not know. Reporting them as empty",
		strings.Join(keys, ", "))
}

// attributeValue maps an OTLP attribute value, as plainValue returns it, onto a Jaeger tag value
// and the tag type that describes it.
//
// The type is carried rather than stringified because the attribute arrives with it: OTLP states
// which variant was written, and Kiali's tag model has a field for it. Stringifying an int64 here
// discards something no later reader can recover.
func attributeValue(value any) (any, jaegerModels.ValueType) {
	switch plain := value.(type) {
	case string:
		return plain, jaegerModels.StringType
	case bool:
		return plain, jaegerModels.BoolType
	case int64:
		return plain, jaegerModels.Int64Type
	case float64:
		// Finite by construction: plainValue hands a non-finite double over as text, because one nested
		// in an array or a map fails json.Marshal below.
		return plain, jaegerModels.Float64Type
	case []byte:
		return plain, jaegerModels.BinaryType
	case nil:
		// no variant set, or one this build cannot read - the proto's profiling variant, or a variant a
		// later revision adds. Either reads as an absent value; warnUnreadVariants says which keys the
		// second case applies to.
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

// plainValue returns an OTLP attribute value as a plain Go value, nested values included, and nil
// when no variant of it is set.
//
// A double that is not a finite number comes back as its text rather than as a float64. JSON has no
// literal for NaN or an infinity, so json.Marshal refuses the whole value such a double sits in: as
// a float64, a single NaN inside an array costs that attribute every one of its elements.
func plainValue(value *commonv1.AnyValue) any {
	switch variant := value.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return variant.StringValue
	case *commonv1.AnyValue_BoolValue:
		return variant.BoolValue
	case *commonv1.AnyValue_IntValue:
		return variant.IntValue
	case *commonv1.AnyValue_DoubleValue:
		return finiteOrText(variant.DoubleValue)
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

// finiteOrText returns a double as itself, or as its text when it is NaN or an infinity, so that
// the value can be written as JSON wherever it appears, nested or not.
func finiteOrText(value float64) any {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	return value
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
