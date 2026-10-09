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
	"maps"
	"slices"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Root keys the supported backends wrap their OTLP payload in.
const (
	// RootKeyResult is the grpc-gateway streaming wrapper Jaeger api_v3 puts
	// over HTTP: {"result": {"resourceSpans": [...]}}.
	RootKeyResult = "result"

	// RootKeyBatches is the name Tempo's trace detail gives the resource spans
	// array: {"batches": [...]}.
	RootKeyBatches = "batches"

	// RootKeyTrace is the object Tempo's /api/v2/traces/{id} route puts the
	// payload in, beside its query statistics: {"metrics": {...}, "trace":
	// {"resourceSpans": [...]}}.
	RootKeyTrace = "trace"
)

// unmarshalOptions drops a field the proto does not have rather than failing on
// it, which the OTLP specification requires rather than merely permits:
// "OTLP/JSON receivers MUST ignore message fields with unknown names and MUST
// unmarshal the message as if the unknown field was not present in the payload."
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
//
// So a field added by a later OTLP revision, or by a backend answering ahead of
// the proto Kiali builds against, must not be able to blank a trace. The cost is
// that such a field is dropped with nothing said about it; a body that is not
// what the transport claimed is caught by the envelope check below and by the
// width of the IDs it produces, which is what strictness was buying.
var unmarshalOptions = protojson.UnmarshalOptions{DiscardUnknown: true}

// The widths OTLP gives a trace and a span ID.
const (
	traceIDBytes = 16
	spanIDBytes  = 8
)

// Options declares how the body being decoded is framed and encoded. Both
// fields are properties of the transport, not of the payload, because neither
// can be told from the bytes.
type Options struct {
	// RootKey is the single object key the payload is wrapped in, empty when the
	// body already is an OTLP TracesData. The wrapped value is read as a
	// TracesData when it is an object and as the resource spans array when it is
	// an array, which is the whole difference between api_v3 and Tempo.
	RootKey string

	// IDEncoding is how the backend renders trace and span IDs. It has no usable
	// zero value, and a body decoded under the wrong encoding is caught by the
	// width of the IDs it produces rather than by protojson.
	IDEncoding IDEncoding
}

// Decode turns an OTLP/JSON body into a TracesData.
//
// A body may hold several concatenated JSON objects: api_v3 answers one today,
// but its IDL reserves the right to stream more of them under
// Transfer-Encoding: chunked, and a reader that took the first would truncate
// the answer silently.
func Decode(body []byte, opts Options) (*tracepb.TracesData, error) {
	if opts.IDEncoding == IDEncodingUnset {
		return nil, errors.New("OTLP decode: the transport must declare an ID encoding")
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	traces := &tracepb.TracesData{}
	objects := 0
	for {
		var object json.RawMessage
		if err := dec.Decode(&object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("OTLP decode: unreadable body: %w", err)
		}
		part, err := decodeObject(object, opts)
		if err != nil {
			return nil, err
		}
		traces.ResourceSpans = append(traces.ResourceSpans, part.GetResourceSpans()...)
		objects++
	}
	if objects == 0 {
		return nil, errors.New("OTLP decode: empty body")
	}
	return traces, nil
}

// decodeObject unwraps one JSON object's envelope and decodes what it holds.
func decodeObject(object json.RawMessage, opts Options) (*tracepb.TracesData, error) {
	payload := object
	if opts.RootKey != "" {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(object, &envelope); err != nil {
			return nil, fmt.Errorf("OTLP decode: unreadable body: %w", err)
		}
		wrapped, found := envelope[opts.RootKey]
		if !found {
			// An envelope with no keys at all is a backend saying the trace
			// holds no spans: a marshaller that leaves out an empty field has
			// nothing else to send. A body of some other shape always carries
			// keys, so this cannot turn one into a silent empty result.
			if len(envelope) == 0 {
				return &tracepb.TracesData{}, nil
			}
			return nil, fmt.Errorf("OTLP decode: body carries no %q key, only %q",
				opts.RootKey, slices.Sorted(maps.Keys(envelope)))
		}
		payload = wrapped
		if isArray(payload) {
			payload = wrapResourceSpans(payload)
		}
	}

	payload, err := normalizeIDs(payload, opts.IDEncoding)
	if err != nil {
		return nil, fmt.Errorf("OTLP decode: unreadable body: %w", err)
	}

	traces := &tracepb.TracesData{}
	if err := unmarshalOptions.Unmarshal(payload, traces); err != nil {
		return nil, fmt.Errorf("OTLP decode: %w", err)
	}
	if err := checkIDs(traces); err != nil {
		return nil, err
	}
	return traces, nil
}

// checkIDs reports an ID of a width OTLP has no room for.
//
// Hex and base64 share an alphabet, so protojson reads either one of them and
// reports no error whichever the transport declared; the width of what comes
// out is the only evidence left that a body was read under the wrong encoding,
// or that an ID was in neither encoding. Both transports can get this wrong, so
// the check sits below them and runs once.
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
