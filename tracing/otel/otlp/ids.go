package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// IDEncoding is how a backend renders the trace and span IDs of an OTLP/JSON
// payload.
type IDEncoding int

const (
	// IDEncodingUnset is rejected rather than defaulted, so that a transport
	// cannot omit the one decision that fails quietly.
	IDEncodingUnset IDEncoding = iota

	// Hex is what Jaeger api_v3 renders over HTTP, against the OTLP/JSON
	// specification: traceId is 32 hex characters and spanId is 16.
	Hex

	// Base64 is the OTLP/JSON encoding of a bytes field, which Tempo renders and
	// protojson reads.
	Base64
)

// idFields are every OTLP/JSON field carrying a trace or span ID, with the
// width of the ID it carries.
var idFields = []struct {
	name string
	size int
}{
	{"traceId", traceIDBytes},
	{"spanId", spanIDBytes},
	{"parentSpanId", spanIDBytes},
}

// normalizeIDs rewrites the IDs of an OTLP/JSON payload into the base64
// protojson reads for a bytes field.
//
// This exists because 32 hex characters are themselves valid base64: protojson
// accepts api_v3's hex IDs in strict mode and hands back 24 bytes of garbage
// with no error, while every other field of the span decodes correctly.
func normalizeIDs(payload json.RawMessage, encoding IDEncoding) (json.RawMessage, error) {
	if encoding != Hex {
		return payload, nil
	}
	return mapArray(payload, "resourceSpans", func(resourceSpans json.RawMessage) (json.RawMessage, error) {
		return mapArray(resourceSpans, "scopeSpans", func(scopeSpans json.RawMessage) (json.RawMessage, error) {
			return mapArray(scopeSpans, "spans", hexIDsToBase64)
		})
	})
}

// hexIDsToBase64 re-encodes the IDs of one span, and of its links, which carry
// the same two fields.
func hexIDsToBase64(span json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(span, &fields); err != nil {
		return nil, err
	}
	for _, field := range idFields {
		raw, found := fields[field.name]
		if !found {
			continue
		}
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			// Leave it for protojson, which names the offending field.
			continue
		}
		encoded, isHex := hexToBase64(id, field.size)
		if !isHex {
			// Leave it for checkIDs: protojson reads anything the base64
			// alphabet covers without complaint, and the length is what gives
			// it away.
			continue
		}
		replacement, err := json.Marshal(encoded)
		if err != nil {
			return nil, err
		}
		fields[field.name] = replacement
	}
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return mapArray(rewritten, "links", hexIDsToBase64)
}

// hexToBase64 re-encodes a hex ID of size bytes, and reports false for anything
// that is not hex so the value is left as it stands.
//
// That makes the rewrite a no-op on an already-base64 payload rather than a
// corruption of it: the 8- and 16-byte IDs OTLP carries encode as 12 and 24
// base64 characters, both of which end in the '=' padding hex rejects.
//
// An ID short of its full width is zero-extended rather than refused, because a
// short hex ID is the same ID. Nothing is known to send one: api_v3 pads, and
// the alternative is worse than the guard, since hex rejects an odd number of
// digits and the value then falls through to protojson, which reads it as
// base64 and makes it another trace.
func hexToBase64(id string, size int) (string, bool) {
	if id == "" || len(id) > 2*size {
		return "", false
	}
	raw, err := hex.DecodeString(strings.Repeat("0", 2*size-len(id)) + id)
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(raw), true
}

// mapArray applies rewrite to every element of the array held under key in the
// JSON object, and returns the object with the results put back. An object
// without that key is returned as it came.
func mapArray(object json.RawMessage, key string, rewrite func(json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object, &fields); err != nil {
		return nil, err
	}
	raw, found := fields[key]
	if !found {
		return object, nil
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, err
	}
	for i, element := range elements {
		rewritten, err := rewrite(element)
		if err != nil {
			return nil, err
		}
		elements[i] = rewritten
	}
	replacement, err := json.Marshal(elements)
	if err != nil {
		return nil, err
	}
	fields[key] = replacement
	return json.Marshal(fields)
}
