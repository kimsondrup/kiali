package model

import (
	"time"

	"github.com/kiali/kiali/tracing/otel/model/otlpjson"
)

// Trace is a list of spans
type TraceMetadata struct {
	TraceID           string        `json:"traceID"`
	RootServiceName   string        `json:"RootServiceName"`
	StartTimeUnixNano string        `json:"startTimeUnixNano"`
	Duration          time.Duration `json:"durationMs"`
}

type TracingResponse struct {
	Traces []TraceMetadata `json:"traces"`
}

type TagsResponse struct {
	TagNames []string `json:"tagNames"`
}

// Span is a span of a trace matched by Tempo's search API. The shape is Tempo's own, not OTLP,
// but the attributes of the matched span are OTLP and are carried as such.
//
// There is no status field: Tempo does not write a span-level status here. Asked for one with
// select(status), it answers with an attribute keyed "status" whose value is the TraceQL status
// intrinsic as text - "unset", "ok" or "error".
type Span struct {
	SpanID            string              `json:"spanID"`
	StartTimeUnixNano string              `json:"startTimeUnixNano"`
	DurationNanos     string              `json:"durationNanos"`
	Attributes        otlpjson.Attributes `json:"attributes"`
	Name              string              `json:"name"`
}

type SpanSet struct {
	Spans   []Span `json:"spans"`
	Matched int    `json:"matched"` // Tempo returns the number of total spans matched in this field
}

type Trace struct {
	TraceID           string  `json:"traceID"`
	RootServiceName   string  `json:"rootServiceName"`
	RootTraceName     string  `json:"rootTraceName,omitempty"`
	StartTimeUnixNano string  `json:"startTimeUnixNano"`
	Duration          int     `json:"durationMs"`
	SpanSet           SpanSet `json:"spanSet"`
}

type Traces struct {
	Traces  []Trace  `json:"traces"`
	Metrics struct{} `json:"metrics"`
}

type TracesError struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}
