package converter

import (
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/kiali/kiali/log"
	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
	"github.com/kiali/kiali/tracing/tempo/querypb"
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

// ConvertTraceMetadata reports a trace matched by Tempo's search API as a Jaeger-shaped trace.
// Both of Tempo's search transports reach it, because the search answer is the same document
// whether it arrived as protobuf over the streaming RPC or as JSON over /api/search.
//
// The metadata is taken by pointer because a generated message carries a mutex, so copying one is
// what go vet's copylocks reports.
func ConvertTraceMetadata(trace *querypb.TraceSearchMetadata, serviceName string) *jaegerModels.Trace {
	traceID := ConvertId(trace.GetTraceID())

	var spans []jaegerModels.Span
	// A matched trace carries the spans that matched, and a trace reporting only its statistics
	// carries no span set at all. The field is a pointer, so reaching into it is a nil
	// dereference rather than an empty range.
	for _, span := range trace.GetSpanSet().GetSpans() {
		converted, ok := convertOtelSpan(span, serviceName, traceID, trace.GetRootTraceName())
		if !ok {
			continue
		}
		spans = append(spans, converted)
	}

	// Matched is the number of spans of this trace the query matched, which the scatter plot
	// draws a trace's bubble from. It is not len(spans): SpansPerSpanSet bounds how many of them
	// the answer carries, so a trace that matched six spans arrives with ten at most.
	jaegerTrace := SearchTrace(traceID, spans, int(trace.GetSpanSet().GetMatched()))
	return &jaegerTrace
}

// convertOtelSpan reports one span matched by Tempo's search API as a Jaeger span. The second
// return is false for a span the caller must drop.
func convertOtelSpan(span *querypb.Span, serviceName string, traceID jaegerModels.TraceID, rootTrace string) (jaegerModels.Span, bool) {
	// A span with no start time is not placed anywhere on a timeline, and reporting it with a
	// start time of zero places it at the Unix epoch instead - in the trace list, in the heatmap
	// and in the Metrics-tab span overlay, all of which read this path.
	if span.GetStartTimeUnixNano() == 0 {
		log.Errorf("Span [%s] of trace [%s] on service [%s] has no start time. Skipping span",
			span.GetSpanID(), traceID, serviceName)
		return jaegerModels.Span{}, false
	}

	// Tempo computes this duration itself, as an unsigned subtraction, so a span whose end
	// precedes its start arrives already wrapped round rather than as two timestamps this function
	// can compare: 18446744073705551616ns, for instance, is 584 years. Any nanosecond duration at
	// or above 2^63 is such a wrap and not a duration, because the longest honest one is bounded
	// by the time since the epoch, which is under 2^61.
	duration := span.GetDurationNanos()
	if duration >= 1<<63 {
		log.Warningf("Span [%s] of trace [%s] reports a duration of [%d]ns, which is a wrapped negative; reporting a zero duration",
			span.GetSpanID(), traceID, duration)
		duration = 0
	}

	// The span carries its own name and the trace's root name is the fallback for one that
	// reported none. business.getFilteredSpans matches the operation name against the service it
	// queried, so a span named after the trace's root is a span that service does not see.
	operationName := rootTrace
	if span.GetName() != "" {
		operationName = span.GetName()
	}

	spanID := convertSpanId(span.GetSpanID())
	modelSpan := jaegerModels.Span{
		SpanID:    spanID,
		TraceID:   traceID,
		Duration:  duration / 1000, // Provided in ns, Jaeger uses ms
		StartTime: span.GetStartTimeUnixNano() / 1000,
		// No more mapped data
		Flags:         0,
		References:    []jaegerModels.Reference{},
		Tags:          convertAttributes(span.GetAttributes(), spanID),
		Logs:          []jaegerModels.Log{},
		OperationName: operationName,
		ProcessID:     "",
		Process:       &jaegerModels.Process{Tags: []jaegerModels.KeyValue{}, ServiceName: serviceName},
		Warnings:      []string{},
	}

	return modelSpan, true
}

// convertAttributes reports the attributes of a span matched by Tempo's search API as span tags.
// Both of Tempo's search transports reach it: the attributes are OTLP KeyValues over gRPC as well
// as over HTTP, so what a value becomes on the Jaeger side is one decision rather than one per
// transport.
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
