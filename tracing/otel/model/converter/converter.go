package converter

import (
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/kiali/kiali/log"
	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	otel "github.com/kiali/kiali/tracing/otel/model"
	"github.com/kiali/kiali/tracing/tempo/tempopb"
	v1 "github.com/kiali/kiali/tracing/tempo/tempopb/common/v1"
	v11 "github.com/kiali/kiali/tracing/tempo/tempopb/resource/v1"
)

// ConvertId reports a trace ID at the 32 hex characters a 128 bit ID has.
//
// Tempo strips the leading zeros of a trace ID, so roughly one in sixteen arrives a character
// short - measured over HTTP as 23 of 200 search results, at 31, 30 and 29 characters, and 4 of 40
// over gRPC. The stripping is in how its search answer spells the ID, not in the ID: the spans of
// the same trace carry all 16 bytes, which is the full width. So the padded form is the one Tempo
// itself reports once asked for the trace, and an unpadded search answer makes one trace into two
// for everything that compares the two: the scatter plot draws the selected trace twice and never
// marks it as selected.
func ConvertId(id string) jaegerModels.TraceID {
	const width = 32
	if len(id) < width {
		id = strings.Repeat("0", width-len(id)) + id
	}
	return jaegerModels.TraceID(id)
}

// convertSpanId
func convertSpanId(id string) jaegerModels.SpanID {
	return jaegerModels.SpanID(id)
}

// SearchTrace assembles the Jaeger-shaped trace a Tempo search result becomes. Both of Tempo's
// search transports end here, so the fields the frontend reads without checking are set in one
// place rather than guarded at each call site.
//
// Spans, Processes and Warnings are set whether or not the result carried any spans. A nil slice
// or map serialises as JSON null, and the frontend's transformTraceData reads all three without
// checking, so a trace with no spans would take the Traces tab down with a TypeError instead of
// rendering as an empty trace.
func SearchTrace(traceID jaegerModels.TraceID, spans []jaegerModels.Span, matched int) jaegerModels.Trace {
	if spans == nil {
		spans = []jaegerModels.Span{}
	}
	return jaegerModels.Trace{
		TraceID:   traceID,
		Spans:     spans,
		Matched:   matched,
		Processes: map[jaegerModels.ProcessID]jaegerModels.Process{},
		Warnings:  []string{},
	}
}

// ConvertTraceMetadata used by the GRPC Client
func ConvertTraceMetadata(trace tempopb.TraceSearchMetadata, serviceName string) (*jaegerModels.Trace, error) {
	traceID := ConvertId(trace.TraceID)

	var spans []jaegerModels.Span
	// A matched trace carries the spans that matched, and a trace reporting only its statistics
	// carries no span set at all. The field is a pointer, so reaching into it is a nil
	// dereference rather than an empty range.
	for _, span := range trace.GetSpanSet().GetSpans() {
		spans = append(spans, convertOtelSpan(span, serviceName, traceID, trace.RootTraceName))
	}

	jaegerTrace := SearchTrace(traceID, spans, len(spans))
	return &jaegerTrace, nil
}

// convertOtelSpan used for GRPC format Spans
func convertOtelSpan(span *tempopb.Span, serviceName string, traceID jaegerModels.TraceID, rootTrace string) jaegerModels.Span {
	// Tempo subtracts this duration itself and the subtraction is unsigned, so a span whose end
	// precedes its start arrives already wrapped round. The rule is the same as on the HTTP search
	// path: at or above 2^63 nanoseconds the value is a wrap rather than a duration.
	duration := span.DurationNanos
	if duration >= 1<<63 {
		log.Warningf("Span [%s] of trace [%s] reports a duration of [%d]ns, which is a wrapped negative; reporting a zero duration",
			span.SpanID, traceID, duration)
		duration = 0
	}

	modelSpan := jaegerModels.Span{
		SpanID:    jaegerModels.SpanID(span.SpanID),
		TraceID:   traceID,
		Duration:  duration / 1000,
		StartTime: span.StartTimeUnixNano / 1000,
		// No more mapped data
		Flags:         0,
		References:    []jaegerModels.Reference{},
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
		log.Errorf("Could not read the start time %q of span [%s] in trace [%s], skipping span: %s",
			span.StartTimeUnixNano, span.SpanID, traceId, err)
		return nil
	}
	// A span with no start time is not placed anywhere on a timeline, and reporting it with a
	// start time of zero places it at the Unix epoch instead - in the trace list, in the
	// heatmap and in the Metrics-tab span overlay, all of which read this path.
	if startTime == 0 {
		log.Errorf("Span [%s] of trace [%s] on service [%s] has no start time. Skipping span",
			span.SpanID, traceId, serviceName)
		return nil
	}
	duration, err := strconv.ParseUint(span.DurationNanos, 10, 64)
	if err != nil {
		log.Errorf("Could not read the duration %q of span [%s] in trace [%s]: %s",
			span.DurationNanos, span.SpanID, traceId, err)
	}
	// Tempo computes this duration itself, as an unsigned subtraction, so a span whose end precedes
	// its start arrives already wrapped round rather than as two timestamps this function can compare:
	// 18446744073705551616ns, for instance, is 584 years. Any nanosecond duration at or above 2^63 is
	// such a wrap and not a duration, because the longest honest one is bounded by the time since the
	// epoch, which is under 2^61.
	if duration >= 1<<63 {
		log.Warningf("Span [%s] of trace [%s] reports a duration of [%d]ns, which is a wrapped negative; reporting a zero duration",
			span.SpanID, traceId, duration)
		duration = 0
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
		Tags:          convertAttributes(span.Attributes, jaegerSpanId),
		Logs:          []jaegerModels.Log{},
		OperationName: operationName,
		ProcessID:     "",
		Process:       &jaegerModels.Process{Tags: []jaegerModels.KeyValue{}, ServiceName: serviceName},
		Warnings:      []string{},
	}

	toRet = append(toRet, jaegerSpan)

	return toRet
}

// convertAttributes reports the attributes of a span matched by Tempo's search API as span tags.
//
// There is no status to report alongside them: Tempo writes no span-level status on this path,
// and the status it does report arrives as the "status" attribute below. The span status is a
// real thing on the trace detail path, where the response is OTLP and statusTags reads it.
func convertAttributes(attributes []*commonpb.KeyValue, spanID jaegerModels.SpanID) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue
	var unread []string
	for _, atb := range attributes {
		if atb.GetKey() == "status" && atb.GetValue().GetStringValue() == "error" {
			tags = append(tags, jaegerModels.KeyValue{Key: "error", Value: true, Type: jaegerModels.BoolType})
			continue
		}
		tag, cannotRead := keyValueFromAttribute(atb)
		if cannotRead {
			unread = append(unread, atb.GetKey())
		}
		tags = append(tags, tag)
	}
	warnUnreadVariants("span "+string(spanID), unread)
	return tags
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
