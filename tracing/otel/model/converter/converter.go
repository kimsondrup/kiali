package converter

import (
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/kiali/kiali/log"
	jsonModel "github.com/kiali/kiali/models/tracing/json"
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
func ConvertId(id string) jsonModel.TraceID {
	const width = 32
	if len(id) < width {
		id = strings.Repeat("0", width-len(id)) + id
	}
	return jsonModel.TraceID(id)
}

func convertSpanId(id string) jsonModel.SpanID {
	return jsonModel.SpanID(id)
}

// SpanSetOf reports the span set of a trace matched by Tempo's search, which Tempo sends twice.
//
// The singular spanSet is the one upstream means to drop: pkg/traceql's engine populates it for
// Grafana's sake and says so, and its combiner assigns it from spanSets[0] under a comment calling
// it deprecated. So the plural is the same set by construction, and it has existed since Tempo
// 2.2.0, which is the oldest Tempo Kiali can query at all. Reading it leaves the singular as the
// fallback it already is upstream.
//
// The first set is the whole answer for one trace unless the query produced several, which is as
// true of the singular - upstream picks spanSets[0] for it.
func SpanSetOf(trace *querypb.TraceSearchMetadata) *querypb.SpanSet {
	if spanSets := trace.GetSpanSets(); len(spanSets) > 0 {
		return spanSets[0]
	}
	return trace.GetSpanSet()
}

// ConvertTraceMetadata reports a trace matched by Tempo's search API as a Jaeger-shaped trace.
// Both of Tempo's search transports reach it, because the search answer is the same document
// whether it arrived as protobuf over the streaming RPC or as JSON over /api/search.
//
// The metadata is taken by pointer because a generated message carries a mutex, so copying one is
// what go vet's copylocks reports.
func ConvertTraceMetadata(trace *querypb.TraceSearchMetadata, serviceName string) *jsonModel.Trace {
	traceID := ConvertId(trace.GetTraceID())
	builder := newTraceBuilder(traceID)

	// Tempo's search answer carries no resource, so the process is the one service name the
	// answer names for the whole trace: the service the query asked for over gRPC, and the one
	// the matched spans report over HTTP. The table is minted here all the same, because the
	// business layer selects a service's spans by looking a span's process up in it.
	process := jsonModel.Process{ServiceName: serviceName, Tags: []jsonModel.KeyValue{}}

	// A matched trace carries the spans that matched, and a trace reporting only its statistics
	// carries no span set at all. The set is a pointer, so reaching into it is a nil dereference
	// rather than an empty range.
	spanSet := SpanSetOf(trace)
	for _, span := range spanSet.GetSpans() {
		converted, ok := convertOtelSpan(span, serviceName, traceID, trace.GetRootTraceName())
		if !ok {
			continue
		}
		builder.add(converted, process)
	}

	converted := builder.trace()
	// Matched is the number of spans of this trace the query matched, which the scatter plot
	// draws a trace's bubble from. It is not len(spans): SpansPerSpanSet bounds how many of them
	// the answer carries, so a trace that matched six spans arrives with ten at most.
	converted.Matched = int(spanSet.GetMatched())
	return &converted
}

// convertOtelSpan reports one span matched by Tempo's search API as a Jaeger span. The second
// return is false for a span the caller must drop.
func convertOtelSpan(span *querypb.Span, serviceName string, traceID jsonModel.TraceID, rootTrace string) (jsonModel.Span, bool) {
	// A span with no start time is not placed anywhere on a timeline, and reporting it with a
	// start time of zero places it at the Unix epoch instead - in the trace list, in the heatmap
	// and in the Metrics-tab span overlay, all of which read this path.
	if span.GetStartTimeUnixNano() == 0 {
		log.Errorf("Span [%s] of trace [%s] on service [%s] has no start time. Skipping span",
			span.GetSpanID(), traceID, serviceName)
		return jsonModel.Span{}, false
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
	modelSpan := jsonModel.Span{
		Duration:      duration / 1000, // Tempo reports nanoseconds; the span model is in microseconds
		Flags:         0,
		Logs:          []jsonModel.Log{},
		OperationName: operationName,
		References:    []jsonModel.Reference{},
		SpanID:        spanID,
		StartTime:     span.GetStartTimeUnixNano() / 1000,
		Tags:          convertAttributes(span.GetAttributes(), spanID),
		TraceID:       traceID,
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
func convertAttributes(attributes []*commonpb.KeyValue, spanID jsonModel.SpanID) []jsonModel.KeyValue {
	var tags []jsonModel.KeyValue
	var unread []string
	for _, atb := range attributes {
		if atb.GetKey() == "status" && atb.GetValue().GetStringValue() == "error" {
			tags = append(tags, jsonModel.KeyValue{Key: "error", Type: jsonModel.BoolType, Value: true})
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
