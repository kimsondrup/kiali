// Package otlpjson reads the JSON form of an OTLP trace payload into the OpenTelemetry proto
// model, which carries the OTLP types as the proto defines them rather than as Kiali guesses
// at them: an attribute value keeps the variant it was written as, and an enum arrives whether
// it was written as its name or as its number.
//
// Both encodings have to be read, because the two backends Kiali can reach do not write the
// same one. The OTLP/JSON encoding is the protobuf JSON mapping with stated deviations from it:
// the two ids are hex rather than base64; "Values of enum fields MUST be encoded as integer
// values ... the enum name strings MUST NOT be used"; a field with an unknown name must be
// ignored rather than refused, quoted in full at unmarshalOptions below; and the keys of JSON
// objects are the field names in lowerCamelCase.
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
//
// Measured, the first two of those are what tells the backends apart. Tempo answers its trace
// endpoints in the plain protobuf JSON mapping - base64 ids and "SPAN_KIND_SERVER" by name -
// and so is not writing OTLP/JSON at all, while Jaeger's api_v3 writes hex ids and a kind of 2.
// protojson implements the mapping, so it reads an attribute, a kind, a status code and a 64 bit
// timestamp in every form either encoding permits, and the ids are the one thing left to repair.
//
// Neither backend puts the span list at the top of the document, either: Tempo's trace API calls
// it "batches" and Jaeger's api_v3 nests the OTLP body under "result". Both are read, so the
// envelope is the only part of a response handled by hand.
package otlpjson

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/kiali/kiali/log"
)

// The keys a trace response can hang its list of ResourceSpans from. The OTLP name is what the
// encoding itself defines; Tempo's own trace API calls the same list "batches", its v2 endpoint
// wraps an OTLP body in a "trace" object, and Jaeger's api_v3 wraps one in a "result" object -
// measured on both of its trace endpoints, which is what a gRPC server-streaming method looks
// like over JSON.
const (
	keyResourceSpans = "resourceSpans"
	keyBatches       = "batches"
	keyTrace         = "trace"
	keyResult        = "result"
)

// maxWrappers is how many objects the span list is allowed to be nested inside.
const maxWrappers = 1

// The sizes the OTLP proto fixes the two ids at.
const (
	traceIDSize = 16
	spanIDSize  = 8
)

// unmarshalOptions drops a field the proto does not have rather than failing on it, which the
// OTLP spec requires rather than merely permits: "OTLP/JSON receivers MUST ignore message fields
// with unknown names and MUST unmarshal the message as if the unknown field was not present in
// the payload." https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
//
// So strict decoding would be the wrong choice even though it reads the responses in this repo's
// fixtures: a field added by a later OTLP revision, or by a backend answering ahead of the proto
// Kiali builds against, must not be able to blank the traces list. The cost is that such a field
// is dropped with nothing said about it.
var unmarshalOptions = protojson.UnmarshalOptions{DiscardUnknown: true}

// UnmarshalTracesData reads a trace response, whichever of the envelopes above it uses. A
// response that uses none of them is an error rather than a trace with no spans, so that a
// shape this cannot read is reported instead of looking like a trace that holds nothing.
func UnmarshalTracesData(body []byte) (*tracev1.TracesData, error) {
	spans, err := resourceSpans(body, 0)
	if err != nil {
		return nil, err
	}

	data := &tracev1.TracesData{}
	if spans == nil {
		return data, nil
	}

	// the list is handed over under the name the proto gives it, so that only the envelope is
	// read by hand and everything inside it is read by protojson
	wrapped := make([]byte, 0, len(spans)+len(keyResourceSpans)+5)
	wrapped = append(wrapped, `{"`+keyResourceSpans+`":`...)
	wrapped = append(wrapped, spans...)
	wrapped = append(wrapped, '}')

	if err := unmarshalOptions.Unmarshal(wrapped, data); err != nil {
		return nil, fmt.Errorf("[OTLP JSON] reading the spans of a trace response: %w", err)
	}
	normalizeIDs(data)
	return data, nil
}

// normalizeIDs repairs the ids of a response that wrote them the way the OTLP/JSON encoding
// says to, which is the first of that encoding's deviations from the protobuf JSON mapping:
// "The traceId and spanId byte arrays are represented as case-insensitive hex-encoded strings;
// they are not base64-encoded as is defined in the standard Protobuf JSON Mapping."
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
// The next one, integer-only enums, needs no repair: protojson accepts both forms.
//
// protojson implements the mapping, so it reads such an id as base64 - and hex text is itself
// valid base64, so it does that without reporting anything: a 32 character trace id arrives as
// 24 bytes of nothing. Nothing Kiali queries today writes an id that way, Tempo writes base64
// on every JSON trace endpoint it has, so this is the encoding's own rule being honoured rather
// than a backend being worked around.
func normalizeIDs(data *tracev1.TracesData) {
	for _, resourceSpans := range data.GetResourceSpans() {
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			for _, span := range scopeSpans.GetSpans() {
				span.TraceId = decodeHexID(span.GetTraceId(), traceIDSize)
				span.SpanId = decodeHexID(span.GetSpanId(), spanIDSize)
				span.ParentSpanId = decodeHexID(span.GetParentSpanId(), spanIDSize)
				for _, link := range span.GetLinks() {
					link.TraceId = decodeHexID(link.GetTraceId(), traceIDSize)
					link.SpanId = decodeHexID(link.GetSpanId(), spanIDSize)
				}
			}
		}
	}
}

// decodeHexID returns the id an OTLP/JSON hex string was meant to be, and the id it was given
// for anything else.
//
// The test is the length, and for an id written at the length the encoding gives it the length
// decides exactly, not by guessing. Base64 of an id is padded to a whole number of groups, so
// base64 of a 16 or an 8 byte id decodes to 16 or 8 bytes; only text that is not base64 of an id
// can decode to the 24 and 12 bytes that 32 and 16 characters of unpadded base64 yield. Those
// two lengths are whole base64 groups, so encoding them again reproduces the text the backend
// wrote, character for character, and the id is read back out of it only if that text is hex
// throughout - which has to be checked rather than assumed, because a base64 alphabet holds
// every hex digit.
//
// What that leaves out is a hex id written shorter than its length, which cannot be repaired
// here and is left as protojson read it. The reason it cannot is worth writing down, because
// padding the text to length looks like an obvious fix: by the time this sees the id, protojson
// has already read the text as base64 and thrown the text away, and base64 of a length that is
// not a whole group drops the last few bits. Measured over a 16 character span id truncated to
// each shorter length: 15 characters come back as 11 bytes whose hex bears no relation to what
// was written, and re-encoding cannot recover the missing character. There is nothing left to
// pad.
//
// Where a short id ends up differs by which id it is, and one of the two does reach a response:
//
//   - a length that is 1 more than a multiple of 4 - 29 characters for a trace id, 13 for a
//     span id - is not valid base64 at all, so protojson rejects the whole document and the
//     caller gets an error. That is the right failure: the user sees a response that could not
//     be read rather than a trace that looks complete.
//   - a short trace id is harmless, because the trace id of a trace detail is the one the
//     request asked for and the one in the body is not used.
//   - a short span id is NOT harmless. A span id of 14 or 15 characters is reported as a 20 or
//     22 character hex "span id", which the frontend's span table and the Grafana deep link
//     then point at a span that does not exist.
//
// Nothing Kiali can query does this today - Tempo writes base64 on every JSON trace endpoint it
// has, and Jaeger's api_v3 zero-pads, measured at 32 and 16 characters on every sampled span -
// so the hole is in what a future backend could send, not in what one does. Tempo's search API
// really does strip a leading zero from a trace id, but a search result is not OTLP and does
// not come through here.
func decodeHexID(id []byte, size int) []byte {
	if len(id) != size*3/2 {
		return id
	}

	decoded := make([]byte, size)
	if _, err := hex.Decode(decoded, []byte(base64.RawStdEncoding.EncodeToString(id))); err != nil {
		return id
	}
	return decoded
}

// resourceSpans returns the list of ResourceSpans in a trace response, as it arrived. A nil
// list with no error means the response holds no spans at all.
func resourceSpans(body []byte, depth int) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("[OTLP JSON] reading the envelope of a trace response: %w", err)
	}

	switch {
	case envelope[keyResourceSpans] != nil:
		return envelope[keyResourceSpans], nil
	case envelope[keyBatches] != nil:
		return envelope[keyBatches], nil
	case len(envelope) == 0:
		// Tempo answers a trace that holds no spans with {}, because the marshaller behind its
		// trace API leaves out a field that is empty
		return nil, nil
	case envelope[keyTrace] != nil && depth < maxWrappers:
		return resourceSpans(envelope[keyTrace], depth+1)
	case envelope[keyResult] != nil && depth < maxWrappers:
		return resourceSpans(envelope[keyResult], depth+1)
	}

	return nil, fmt.Errorf("[OTLP JSON] no span list in a trace response: it has none of %q, %q, %q and %q, only %q",
		keyResourceSpans, keyBatches, keyTrace, keyResult, slices.Sorted(maps.Keys(envelope)))
}

// Attributes is a list of OTLP attributes carried inside a document that is not itself OTLP.
// Tempo's search API answers in a shape of its own and puts the attributes of a matched span
// in it unchanged, so they arrive through encoding/json, which cannot read an AnyValue: its
// variants are a protobuf oneof, and a oneof is a Go interface the generated code fills in.
// Each attribute is handed to protojson on its own instead.
//
// A list written as null needs no case of its own: encoding/json hands the literal null to the
// method below, and reading it into the raw list yields no elements and no error.
type Attributes []*commonv1.KeyValue

func (a *Attributes) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	attributes := make(Attributes, 0, len(raw))
	for _, item := range raw {
		attribute := &commonv1.KeyValue{}
		if err := unmarshalOptions.Unmarshal(item, attribute); err != nil {
			// one attribute is not the document. Reporting an error here fails the whole search
			// response, and a traces list that goes empty over one odd value is worse than a
			// trace that carries one tag it cannot show: the key is kept, the value left unset,
			// and the reason logged, which is more than was said about it before.
			log.Warningf("[OTLP JSON] Could not read the value of span attribute %q: %s", attributeKey(item), err)
			attributes = append(attributes, &commonv1.KeyValue{Key: attributeKey(item)})
			continue
		}
		attributes = append(attributes, attribute)
	}
	*a = attributes
	return nil
}

// attributeKey reads the key of an attribute whose value could not be read. A key is plain JSON
// text and arrives whatever the value holds.
func attributeKey(item json.RawMessage) string {
	var named struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(item, &named); err != nil {
		return ""
	}
	return named.Key
}
