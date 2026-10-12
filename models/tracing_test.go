package models

import (
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	jsonModel "github.com/kiali/kiali/models/tracing/json"
)

// TestTracingDTOWireNames pins the property names of every tracing response
// body, because they are the swagger definitions the UI reads and renaming a
// field here breaks the API without breaking a compile.
func TestTracingDTOWireNames(t *testing.T) {
	span := jsonModel.Span{
		TraceID:       "0af7651916cd43dd8448eb211c80319c",
		SpanID:        "b7ad6b7169203331",
		ParentSpanID:  "b9c7c989f97918e1",
		Flags:         1,
		OperationName: "productpage.bookinfo.svc.cluster.local:9080/*",
		References:    []jsonModel.Reference{{RefType: jsonModel.ChildOf}},
		StartTime:     1,
		Duration:      2,
		Tags:          []jsonModel.KeyValue{{Key: "error", Type: jsonModel.BoolType, Value: true}},
		Logs:          []jsonModel.Log{{Timestamp: 3}},
		ProcessID:     "p1",
		Process:       &jsonModel.Process{ServiceName: "productpage"},
		Warnings:      []string{"clock skew"},
	}
	trace := jsonModel.Trace{
		TraceID:   span.TraceID,
		Spans:     []jsonModel.Span{span},
		Processes: map[jsonModel.ProcessID]jsonModel.Process{"p1": {ServiceName: "productpage"}},
		Warnings:  []string{"clock skew"},
		Matched:   1,
	}
	errs := []StructuredError{{Code: 500, Msg: "boom", TraceID: string(span.TraceID)}}

	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"StructuredError", errs[0], []string{"code", "msg", "traceID"}},
		{"TracingServices", TracingServices{Data: []string{"productpage"}}, []string{"data"}},
		{"TracingResponse", TracingResponse{Data: []jsonModel.Trace{trace}, Errors: errs, TracingServiceName: "productpage"}, []string{"data", "errors", "tracingServiceName"}},
		{"TracingSingleTrace", TracingSingleTrace{Data: trace, Errors: errs}, []string{"data", "errors"}},
		{"TracingSpan", TracingSpan{Span: span, TraceSize: 7}, []string{"duration", "flags", "logs", "operationName", "parentSpanID", "process", "processID", "references", "spanID", "startTime", "tags", "traceID", "traceSize", "warnings"}},
		{"ParsedUrl", ParsedUrl{BaseUrl: "http://tempo:3200", Host: "tempo", Path: "/", Port: "3200", Scheme: "http", Url: &url.URL{Scheme: "http"}}, []string{"baseUrl", "host", "path", "port", "scheme", "url"}},
		{"ValidConfig", ValidConfig{AuthType: "none", NamespaceSelector: new(bool), Provider: "tempo", Url: "http://tempo:3200", UseGRPC: true, Warning: "none"}, []string{"authType", "namespaceSelector", "provider", "url", "useGRPC", "warning"}},
		{"TracingDiagnose", TracingDiagnose{Code: 200, LogLine: []LogLine{{}}, Message: "ok", ValidConfig: []ValidConfig{{}}}, []string{"code", "logLine", "message", "validConfig"}},
		{"ConfigurationValidation", ConfigurationValidation{Error: "", Message: "ok"}, []string{"error", "message"}},
		{"LogLine", LogLine{Result: "ok", Test: "reachable", Time: time.Unix(0, 0)}, []string{"result", "test", "time"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.value)
			require.NoError(t, err)

			var document map[string]any
			require.NoError(t, json.Unmarshal(raw, &document))

			got := slices.Sorted(maps.Keys(document))
			require.Equal(t, c.want, got)
		})
	}
}

// TestTracingSpanWireNames pins the trace and span property names reached
// through TracingResponse, which is the body the UI's trace list decodes.
func TestTracingSpanWireNames(t *testing.T) {
	response := TracingResponse{Data: []jsonModel.Trace{{
		TraceID:   "0af7651916cd43dd8448eb211c80319c",
		Spans:     []jsonModel.Span{{SpanID: "b7ad6b7169203331", Tags: []jsonModel.KeyValue{{Key: "error", Type: jsonModel.BoolType, Value: true}}}},
		Processes: map[jsonModel.ProcessID]jsonModel.Process{"p1": {ServiceName: "productpage", Tags: []jsonModel.KeyValue{}}},
		Warnings:  []string{},
		Matched:   1,
	}}}

	raw, err := json.Marshal(response)
	require.NoError(t, err)

	var document struct {
		Data []struct {
			TraceID   string `json:"traceID"`
			Matched   int    `json:"matched"`
			Processes map[string]struct {
				ServiceName string `json:"serviceName"`
			} `json:"processes"`
			Spans []struct {
				SpanID string `json:"spanID"`
				Tags   []struct {
					Key   string `json:"key"`
					Type  string `json:"type"`
					Value any    `json:"value"`
				} `json:"tags"`
			} `json:"spans"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &document))

	require.Len(t, document.Data, 1)
	require.Equal(t, "0af7651916cd43dd8448eb211c80319c", document.Data[0].TraceID)
	require.Equal(t, 1, document.Data[0].Matched)
	require.Equal(t, "productpage", document.Data[0].Processes["p1"].ServiceName)
	require.Len(t, document.Data[0].Spans, 1)
	require.Equal(t, "b7ad6b7169203331", document.Data[0].Spans[0].SpanID)
	require.Equal(t, "error", document.Data[0].Spans[0].Tags[0].Key)
	require.Equal(t, "bool", document.Data[0].Spans[0].Tags[0].Type)
	require.Equal(t, true, document.Data[0].Spans[0].Tags[0].Value)
}
