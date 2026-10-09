package converter

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kiali/kiali/log"
	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
)

const (
	// serviceNameAttribute is the resource attribute naming the process a span belongs to.
	serviceNameAttribute = "service.name"

	// unknownService is the service name the OpenTelemetry specification mandates when an SDK
	// cannot determine one, so a resource without the attribute is still named in the UI.
	unknownService = "unknown_service"

	// errorAttribute is the error flag a proxy sets on a failed span, alongside an OTLP status of
	// error rather than instead of it.
	errorAttribute = "error"

	// statusAttribute carries an error on a span whose OTLP status does not.
	statusAttribute = "status"
)

// TracesFromOTLP converts an OTLP payload into Kiali's traces.
//
// It is the one place the OTLP wire model is read, for both providers and for both of their
// transports, so every representational decision below is made exactly once.
func TracesFromOTLP(traces *tracepb.TracesData) []jaegerModels.Trace {
	var order []jaegerModels.TraceID
	byID := map[jaegerModels.TraceID]*traceBuilder{}

	for _, resourceSpans := range traces.GetResourceSpans() {
		process := processFromResource(resourceSpans.GetResource())
		// Every scope, not the first one: a resource span carries one scope per instrumentation
		// library, and a proxy reports two of them for well over a third of the resources it sends.
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			for _, span := range scopeSpans.GetSpans() {
				traceID := jaegerModels.TraceID(hex.EncodeToString(span.GetTraceId()))
				converted, ok := spanFromOTLP(span, traceID, scopeSpans.GetScope())
				if !ok {
					continue
				}
				builder, found := byID[traceID]
				if !found {
					builder = newTraceBuilder(traceID)
					byID[traceID] = builder
					order = append(order, traceID)
				}
				builder.add(converted, process)
			}
		}
	}

	converted := make([]jaegerModels.Trace, 0, len(order))
	for _, traceID := range order {
		converted = append(converted, byID[traceID].trace())
	}
	return converted
}

// TraceFromOTLP converts an OTLP payload holding a single trace, and answers nil for a payload
// holding no span at all. Whether that is a trace the backend does not have or a backend that
// answered badly is told by the transport's own status, not here.
func TraceFromOTLP(traces *tracepb.TracesData) *jaegerModels.Trace {
	converted := TracesFromOTLP(traces)
	if len(converted) == 0 {
		return nil
	}
	if len(converted) > 1 {
		log.Warningf("A trace detail response carries [%d] traces; reporting trace [%s] only", len(converted), converted[0].TraceID)
	}
	return &converted[0]
}

// traceBuilder collects the spans of one trace together with the process table they resolve in.
type traceBuilder struct {
	traceID   jaegerModels.TraceID
	spans     []jaegerModels.Span
	processes map[jaegerModels.ProcessID]jaegerModels.Process
	keys      map[string]jaegerModels.ProcessID
}

func newTraceBuilder(traceID jaegerModels.TraceID) *traceBuilder {
	return &traceBuilder{
		traceID:   traceID,
		processes: map[jaegerModels.ProcessID]jaegerModels.Process{},
		keys:      map[string]jaegerModels.ProcessID{},
	}
}

// add places a span in the trace, carrying its process both as a table entry it refers to and as
// an embedded value. Both are needed: a span is selected either by the service name of its
// embedded process or by looking its process ID up in the table, and which of the two happens is
// not this layer's business.
func (b *traceBuilder) add(span jaegerModels.Span, process jaegerModels.Process) {
	span.ProcessID = b.processID(process)
	span.Process = &process
	b.spans = append(b.spans, span)
}

// processID returns the key of the process in this trace's table, minting one the first time the
// process is seen. The p1, p2, ... form is the one Jaeger mints for the same table.
func (b *traceBuilder) processID(process jaegerModels.Process) jaegerModels.ProcessID {
	key := processKey(process)
	if id, found := b.keys[key]; found {
		return id
	}
	id := jaegerModels.ProcessID("p" + strconv.Itoa(len(b.processes)+1))
	b.keys[key] = id
	b.processes[id] = process
	return id
}

func (b *traceBuilder) trace() jaegerModels.Trace {
	return jaegerModels.Trace{
		TraceID:   b.traceID,
		Spans:     b.spans,
		Processes: b.processes,
		Warnings:  []string{},
		Matched:   len(b.spans),
	}
}

// processKey identifies a process by its content, so that the resource two spans share becomes one
// entry of the trace's table. The order of the attributes is not part of that identity: a backend
// is free to send them differently from one resource span to the next.
func processKey(process jaegerModels.Process) string {
	tags := make([]string, 0, len(process.Tags))
	for _, tag := range process.Tags {
		tags = append(tags, fmt.Sprintf("%s=%s=%v", tag.Key, tag.Type, tag.Value))
	}
	sort.Strings(tags)
	return process.ServiceName + "\x00" + strings.Join(tags, "\x00")
}

func processFromResource(resource *resourcepb.Resource) jaegerModels.Process {
	process := jaegerModels.Process{Tags: []jaegerModels.KeyValue{}}
	var unread []string
	for _, attribute := range resource.GetAttributes() {
		if attribute.GetKey() == serviceNameAttribute {
			process.ServiceName = attribute.GetValue().GetStringValue()
			continue
		}
		tag, cannotRead := keyValueFromAttribute(attribute)
		if cannotRead {
			unread = append(unread, attribute.GetKey())
		}
		process.Tags = append(process.Tags, tag)
	}
	if process.ServiceName == "" {
		process.ServiceName = unknownService
	}
	warnUnreadVariants("resource "+process.ServiceName, unread)
	return process
}

// spanFromOTLP converts one span, and reports false for a span that cannot be placed on a
// timeline.
func spanFromOTLP(span *tracepb.Span, traceID jaegerModels.TraceID, scope *commonpb.InstrumentationScope) (jaegerModels.Span, bool) {
	spanID := jaegerModels.SpanID(hex.EncodeToString(span.GetSpanId()))

	// A span with no start time is not placed anywhere on a timeline, and reporting it with a
	// start time of zero places it at the Unix epoch instead.
	if span.GetStartTimeUnixNano() == 0 {
		log.Errorf("Span [%s] of trace [%s] has no start time. Skipping span", spanID, traceID)
		return jaegerModels.Span{}, false
	}

	return jaegerModels.Span{
		TraceID:       traceID,
		SpanID:        spanID,
		OperationName: span.GetName(),
		StartTime:     span.GetStartTimeUnixNano() / 1000,
		Duration:      durationMicros(span, spanID),
		References:    referencesFromParent(traceID, jaegerModels.SpanID(hex.EncodeToString(span.GetParentSpanId()))),
		Tags:          tagsFromSpan(span, spanID, scope),
		Logs:          []jaegerModels.Log{},
		Warnings:      []string{},
		Flags:         0,
	}, true
}

func referencesFromParent(traceID jaegerModels.TraceID, parentSpanID jaegerModels.SpanID) []jaegerModels.Reference {
	if parentSpanID == "" {
		return []jaegerModels.Reference{}
	}
	return []jaegerModels.Reference{{
		RefType: jaegerModels.ChildOf,
		TraceID: traceID,
		SpanID:  parentSpanID,
	}}
}

// tagsFromSpan reports everything of a span that Jaeger's model carries as a tag. The names for
// the OpenTelemetry fields with no Jaeger equivalent come from the OpenTelemetry mapping to
// non-OTLP formats: https://opentelemetry.io/docs/specs/otel/common/mapping-to-non-otlp/
func tagsFromSpan(span *tracepb.Span, spanID jaegerModels.SpanID, scope *commonpb.InstrumentationScope) []jaegerModels.KeyValue {
	tags := make([]jaegerModels.KeyValue, 0, len(span.GetAttributes())+2)
	isError := span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR
	var unread []string

	for _, attribute := range span.GetAttributes() {
		switch attribute.GetKey() {
		case errorAttribute:
			flag, isFlag := errorFlag(attribute.GetValue())
			isError = isError || flag
			// A bare flag says nothing the single error tag below does not, so it is folded into
			// it rather than passed through: a span would otherwise carry two tags keyed "error"
			// with two different types. Envoy sends the attribute and an error status together on
			// about one span in ten. Any other value says more than the flag does, so it is kept.
			if isFlag {
				continue
			}
		case statusAttribute:
			if attribute.GetValue().GetStringValue() == "error" {
				isError = true
				continue
			}
		}
		tag, cannotRead := keyValueFromAttribute(attribute)
		if cannotRead {
			unread = append(unread, attribute.GetKey())
		}
		tags = append(tags, tag)
	}
	warnUnreadVariants("span "+string(spanID), unread)

	// Kiali reads span.kind to tell an inbound span from an outbound one. An unspecified kind
	// carries no tag, which is also how Jaeger reports it.
	if kind := spanKind(span.GetKind()); kind != "" {
		tags = append(tags, jaegerModels.KeyValue{Key: "span.kind", Type: jaegerModels.StringType, Value: kind})
	}

	// A span whose status is an error MUST carry an error tag with the Boolean value true.
	// https://opentelemetry.io/docs/specs/otel/trace/sdk_exporters/jaeger
	if isError {
		tags = append(tags, jaegerModels.KeyValue{Key: errorAttribute, Type: jaegerModels.BoolType, Value: true})
	}
	tags = append(tags, statusTags(span.GetStatus())...)
	return append(tags, scopeTags(scope)...)
}

// statusTags reports an OTLP span status as the two tags the mapping to non-OTLP formats names
// for it, which say WHY a span failed where the error tag above says only that it did. The
// mapping requires the status to be reported "unless the Status is UNSET. In the latter case it
// MUST NOT be reported", and prescribes the name of the code rather than its number.
// https://opentelemetry.io/docs/specs/otel/common/mapping-to-non-otlp/#span-status
//
// The error tag the same mapping asks for is not emitted here: it is folded with the error and
// status attributes in tagsFromSpan, because Envoy sends all three signals on one span and a
// span must not come out carrying three tags keyed "error".
func statusTags(status *tracepb.Status) []jaegerModels.KeyValue {
	code := ""
	switch status.GetCode() {
	case tracepb.Status_STATUS_CODE_OK:
		code = "OK"
	case tracepb.Status_STATUS_CODE_ERROR:
		code = "ERROR"
	}
	// An UNSET status is not reported at all, so neither is its message: the rule is about the
	// status, not about one of its fields.
	if code == "" {
		return nil
	}
	tags := []jaegerModels.KeyValue{{Key: "otel.status_code", Type: jaegerModels.StringType, Value: code}}
	if message := status.GetMessage(); message != "" {
		tags = append(tags, jaegerModels.KeyValue{Key: "otel.status_description", Type: jaegerModels.StringType, Value: message})
	}
	return tags
}

// scopeTags reports an OTLP instrumentation scope as span tags. The mapping to non-OTLP formats
// names otel.scope.name and otel.scope.version for its fields, at requirement level Recommended.
//
// It also says the deprecated otel.library.name and otel.library.version "MUST also be reported
// with exact same values for backward compatibility reasons". Only the current pair is emitted,
// so that MUST is deliberately declined: the aliases would double every scope tag on every span
// to serve a reader that predates them, and nothing Kiali ships reads either name.
// https://opentelemetry.io/docs/specs/otel/common/mapping-to-non-otlp/#instrumentationscope
//
// Skipping an empty name or version is this function's own choice, not a rule from that document,
// which prescribes nothing about an empty field. A tag whose value is the empty string says
// nothing a missing tag does not, and Jaeger's own translator omits them too.
func scopeTags(scope *commonpb.InstrumentationScope) []jaegerModels.KeyValue {
	var tags []jaegerModels.KeyValue
	if name := scope.GetName(); name != "" {
		tags = append(tags, jaegerModels.KeyValue{Key: "otel.scope.name", Type: jaegerModels.StringType, Value: name})
	}
	if version := scope.GetVersion(); version != "" {
		tags = append(tags, jaegerModels.KeyValue{Key: "otel.scope.version", Type: jaegerModels.StringType, Value: version})
	}
	return tags
}

// errorFlag reads an error attribute. The flag arrives as the Boolean an SDK sets or as the string
// a proxy sets, and an instrumentation may instead fill the attribute with the error itself. The
// second return reports whether the value is a bare flag and so carries nothing worth keeping
// beyond the error tag; a value that is not a flag, such as a message or a count, is reported as
// such so the caller can keep it.
func errorFlag(value *commonpb.AnyValue) (flag bool, isFlag bool) {
	switch value := value.GetValue().(type) {
	case *commonpb.AnyValue_BoolValue:
		return value.BoolValue, true
	case *commonpb.AnyValue_StringValue:
		switch {
		case value.StringValue == "" || strings.EqualFold(value.StringValue, "false"):
			return false, true
		case strings.EqualFold(value.StringValue, "true"):
			return true, true
		}
		// The error itself. It says the span failed and it is the only account of why.
		return true, false
	}
	return false, false
}

// spanKind is the direction tag Kiali reads to tell an inbound span from an outbound one.
//
// internal is reported although it says nothing about a direction, because Jaeger's own OTLP
// translator passes it through: omitting it would make a Tempo-sourced span and a Jaeger-sourced
// span of the same trace carry different tags. The frontend reads an unrecognised kind exactly as
// it reads a missing one.
func spanKind(kind tracepb.Span_SpanKind) string {
	switch kind {
	case tracepb.Span_SPAN_KIND_CLIENT:
		return "client"
	case tracepb.Span_SPAN_KIND_SERVER:
		return "server"
	case tracepb.Span_SPAN_KIND_PRODUCER:
		return "producer"
	case tracepb.Span_SPAN_KIND_CONSUMER:
		return "consumer"
	case tracepb.Span_SPAN_KIND_INTERNAL:
		return "internal"
	}
	return ""
}

// durationMicros is the duration Jaeger reports for a span, in microseconds.
//
// The subtraction is unsigned, and the OTLP proto only says that the end time is expected to be at
// or after the start time. An end before the start wraps the result round to several hundred years.
// A zero duration says the span's end cannot be believed and keeps the span, which is better than
// dropping it: it still carries its name, its service and its tags.
func durationMicros(span *tracepb.Span, spanID jaegerModels.SpanID) uint64 {
	start, end := span.GetStartTimeUnixNano(), span.GetEndTimeUnixNano()
	if end < start {
		log.Warningf("Span [%s] end time [%d] is before its start time [%d], reporting a zero duration", spanID, end, start)
		return 0
	}
	// nano to micro
	return (end - start) / 1000
}
