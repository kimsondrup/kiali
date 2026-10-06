// Package otlp turns a trace backend's bytes into the OpenTelemetry wire model.
// It is the only place that knows how a backend frames and encodes an OTLP/JSON
// payload; everything above it works with *tracepb.TracesData.
package otlp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Root keys the supported backends wrap their OTLP payload in.
const (
	// RootKeyBatches is the name Tempo's trace detail gives the resource spans
	// array: {"batches": [...]}.
	RootKeyBatches = "batches"
)

// The widths OTLP gives a trace and a span ID.
const (
	traceIDBytes = 16
	spanIDBytes  = 8
)

// Decode turns an OTLP/JSON body into a TracesData.
//
// rootKey is the one object key the payload is wrapped in, and is empty when the
// body already is an OTLP TracesData. It is a property of the transport rather
// than of the payload, because the bytes do not say which of the two they are.
// The wrapped value is read as the resource spans array when it is an array and
// as a TracesData when it is an object, which is the difference between the two
// routes Tempo serves a trace detail on.
//
// IDs are read as the base64 OTLP/JSON gives a bytes field, which is what Tempo
// renders and what protojson reads natively.
func Decode(body []byte, rootKey string) (*tracepb.TracesData, error) {
	dec := json.NewDecoder(bytes.NewReader(body))

	var object json.RawMessage
	if err := dec.Decode(&object); err != nil {
		// An empty body is not an answer with nothing in it. The callers above
		// cannot tell the two apart once this returns a TracesData.
		if errors.Is(err, io.EOF) {
			return nil, errors.New("OTLP decode: empty body")
		}
		return nil, fmt.Errorf("OTLP decode: unreadable body: %w", err)
	}

	traces, err := decodeObject(object, rootKey)
	if err != nil {
		return nil, err
	}

	// Anything after the object means the body is not the single payload it was
	// read as, so what was decoded is a part of an answer rather than the answer.
	if dec.More() {
		return nil, errors.New("OTLP decode: unreadable body: data after the end of the payload")
	}
	return traces, nil
}

// decodeObject unwraps the envelope of one JSON object and decodes what it holds.
func decodeObject(object json.RawMessage, rootKey string) (*tracepb.TracesData, error) {
	payload := object
	if rootKey != "" {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(object, &root); err != nil {
			return nil, fmt.Errorf("OTLP decode: unreadable body: %w", err)
		}
		wrapped, found := root[rootKey]
		if !found {
			return nil, fmt.Errorf("OTLP decode: body carries no %q key", rootKey)
		}
		payload = wrapped
		if isArray(payload) {
			payload = wrapResourceSpans(payload)
		}
	}

	traces := &tracepb.TracesData{}
	// Strict: an unknown field means the payload is not what the transport
	// claimed it was, and guessing at it is how a wrong answer gets reported as
	// a right one.
	if err := protojson.Unmarshal(payload, traces); err != nil {
		return nil, fmt.Errorf("OTLP decode: %w", err)
	}
	if err := checkIDs(traces); err != nil {
		return nil, err
	}
	return traces, nil
}

// checkIDs reports an ID of a width OTLP has no room for.
//
// protojson reads whatever of a bytes field the base64 alphabet covers and puts
// no width constraint on the result, so a truncated or an overlong ID decodes
// with no error and names a trace that does not exist. The width of what comes
// out is the only evidence there is, and the check sits below the transports so
// that it runs once for all of them.
func checkIDs(traces *tracepb.TracesData) error {
	for _, resourceSpans := range traces.GetResourceSpans() {
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			for _, span := range scopeSpans.GetSpans() {
				if err := checkID(span.GetName(), "trace ID", span.GetTraceId(), traceIDBytes, false); err != nil {
					return err
				}
				if err := checkID(span.GetName(), "span ID", span.GetSpanId(), spanIDBytes, false); err != nil {
					return err
				}
				// A root span has no parent, and a link may name neither.
				if err := checkID(span.GetName(), "parent span ID", span.GetParentSpanId(), spanIDBytes, true); err != nil {
					return err
				}
				for _, link := range span.GetLinks() {
					if err := checkID(span.GetName(), "linked trace ID", link.GetTraceId(), traceIDBytes, true); err != nil {
						return err
					}
					if err := checkID(span.GetName(), "linked span ID", link.GetSpanId(), spanIDBytes, true); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func checkID(span, field string, id []byte, size int, optional bool) error {
	if len(id) == size || (optional && len(id) == 0) {
		return nil
	}
	return fmt.Errorf("OTLP decode: span %q carries a %d-byte %s, not %d", span, len(id), field, size)
}

func isArray(payload json.RawMessage) bool {
	trimmed := bytes.TrimLeft(payload, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}

func wrapResourceSpans(array json.RawMessage) json.RawMessage {
	wrapped := make([]byte, 0, len(array)+len(`{"resourceSpans":}`))
	wrapped = append(wrapped, `{"resourceSpans":`...)
	wrapped = append(wrapped, array...)
	return append(wrapped, '}')
}
