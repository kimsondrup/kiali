package jaeger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kiali/kiali/tracing/otel/model/converter"
)

// Each case is a single-span trace and a search for it. Where Jaeger 2.20's memory store answers
// the trace, keepTracesWithTags, which runs on that answer over HTTP, must keep it; where the
// store does not, the filter has to drop it too, so that it narrows the answer of a gateway that
// ignores the tags. The span goes through the converter first, because the filter only ever sees
// what the converter made of it.
func TestKeepTracesWithTagsMatchesTheServer(t *testing.T) {
	str := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	cases := map[string]struct {
		attributes map[string]*commonpb.AnyValue
		dropped    bool
		kind       tracepb.Span_SpanKind
		resource   map[string]*commonpb.AnyValue
		status     tracepb.Status_StatusCode
		tags       map[string]string
	}{
		"error flag as a string":       {attributes: map[string]*commonpb.AnyValue{"error": str("true")}, status: tracepb.Status_STATUS_CODE_ERROR, tags: map[string]string{"error": "true"}},
		"error in another case":        {attributes: map[string]*commonpb.AnyValue{"error": str("TRUE")}, status: tracepb.Status_STATUS_CODE_ERROR, tags: map[string]string{"error": "TRUE"}},
		"error status only":            {status: tracepb.Status_STATUS_CODE_ERROR, tags: map[string]string{"error": "true"}},
		"error false, status ok":       {attributes: map[string]*commonpb.AnyValue{"error": str("false")}, status: tracepb.Status_STATUS_CODE_OK, tags: map[string]string{"error": "false"}},
		"error flag on an ok span":     {attributes: map[string]*commonpb.AnyValue{"error": str("true")}, status: tracepb.Status_STATUS_CODE_OK, tags: map[string]string{"error": "false"}},
		"status attribute":             {attributes: map[string]*commonpb.AnyValue{"status": str("error")}, tags: map[string]string{"status": "error"}},
		"service name":                 {resource: map[string]*commonpb.AnyValue{"service.name": str("svc")}, tags: map[string]string{"service.name": "svc"}},
		"span kind in upper case":      {kind: tracepb.Span_SPAN_KIND_SERVER, tags: map[string]string{"span.kind": "SERVER"}},
		"span. key as an attribute":    {attributes: map[string]*commonpb.AnyValue{"span.foo": str("bar")}, tags: map[string]string{"span.foo": "bar"}},
		"scope. key as an attribute":   {attributes: map[string]*commonpb.AnyValue{"scope.foo": str("bar")}, tags: map[string]string{"scope.foo": "bar"}},
		"int":                          {attributes: map[string]*commonpb.AnyValue{"code": {Value: &commonpb.AnyValue_IntValue{IntValue: 500}}}, tags: map[string]string{"code": "500"}},
		"small double":                 {attributes: map[string]*commonpb.AnyValue{"ratio": {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.5}}}, tags: map[string]string{"ratio": "0.5"}},
		"large double":                 {attributes: map[string]*commonpb.AnyValue{"big": {Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1e21}}}, tags: map[string]string{"big": "1e+21"}},
		"bytes as base64":              {attributes: map[string]*commonpb.AnyValue{"blob": {Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("hi")}}}, tags: map[string]string{"blob": "aGk="}},
		"bool":                         {attributes: map[string]*commonpb.AnyValue{"sampled": {Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}}, tags: map[string]string{"sampled": "true"}},
		"error true on an ok span":     {status: tracepb.Status_STATUS_CODE_OK, tags: map[string]string{"error": "true"}, dropped: true},
		"error false on a failed span": {status: tracepb.Status_STATUS_CODE_ERROR, tags: map[string]string{"error": "false"}, dropped: true},
		"another span kind":            {kind: tracepb.Span_SPAN_KIND_CLIENT, tags: map[string]string{"span.kind": "server"}, dropped: true},
		"span. key not carried":        {tags: map[string]string{"span.foo": "bar"}, dropped: true},
		"scope. key not carried":       {tags: map[string]string{"scope.foo": "bar"}, dropped: true},
		"another service":              {resource: map[string]*commonpb.AnyValue{"service.name": str("svc")}, tags: map[string]string{"service.name": "other"}, dropped: true},
		"resource attribute":           {resource: map[string]*commonpb.AnyValue{"k8s.namespace.name": str("bookinfo")}, tags: map[string]string{"k8s.namespace.name": "bookinfo"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			span := &tracepb.Span{
				Attributes:        keyValues(tc.attributes),
				EndTimeUnixNano:   2_000_000,
				Kind:              tc.kind,
				SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
				StartTimeUnixNano: 1_000_000,
				Status:            &tracepb.Status{Code: tc.status},
				TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			}
			traces := converter.TracesFromOTLP(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
				Resource:   &resourcepb.Resource{Attributes: keyValues(tc.resource)},
				ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}},
			}}})
			want := 1
			if tc.dropped {
				want = 0
			}
			assert.Len(t, keepTracesWithTags(traces, tc.tags), want)
		})
	}
}

func keyValues(values map[string]*commonpb.AnyValue) []*commonpb.KeyValue {
	kvs := make([]*commonpb.KeyValue, 0, len(values))
	for key, value := range values {
		kvs = append(kvs, &commonpb.KeyValue{Key: key, Value: value})
	}
	return kvs
}
