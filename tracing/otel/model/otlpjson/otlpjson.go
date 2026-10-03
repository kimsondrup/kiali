// Package otlpjson reads the JSON form of an OTLP trace payload into the OpenTelemetry proto
// model, which carries the OTLP types as the proto defines them rather than as Kiali guesses
// at them: an attribute value keeps the variant it was written as, and an enum arrives whether
// it was written as its name or as its number.
//
// Both encodings have to be read. The OTLP/JSON encoding is the protobuf JSON mapping with one
// documented divergence, and protojson implements that mapping, so it reads a span attribute,
// a span kind, a status code and a 64 bit timestamp in every form either encoding permits.
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
package otlpjson

import (
	"bytes"
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
// encoding itself defines and what Jaeger's OTLP endpoint answers with; Tempo's own trace API
// calls the same list "batches", and its v2 endpoint wraps an OTLP body in one more object.
const (
	keyResourceSpans = "resourceSpans"
	keyBatches       = "batches"
	keyTrace         = "trace"
)

// maxWrappers is how many objects the span list is allowed to be nested inside.
const maxWrappers = 1

// The sizes the OTLP proto fixes the two ids at.
const (
	traceIDSize = 16
	spanIDSize  = 8
)

// unmarshalOptions drops a field the proto does not have rather than failing on it. Nothing
// measured needs that: strict decoding reads every captured Tempo and Jaeger response here. It
// is a trace response's own field that a later OTLP revision, or a backend that answers ahead
// of the proto Kiali builds against, would add - and such a field must not be able to blank
// the traces list. The cost is that it is then dropped with nothing said about it.
var unmarshalOptions = protojson.UnmarshalOptions{DiscardUnknown: true}

// jsonNull is the literal encoding/json hands an UnmarshalJSON method for a field written as
// null, which it documents as meaning the field is absent.
var jsonNull = []byte("null")

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
// says to. That encoding is the protobuf JSON mapping with one divergence, and it names it:
// "The traceId and spanId byte arrays are represented as case-insensitive hex-encoded strings;
// they are not base64-encoded as is defined in the standard Protobuf JSON Mapping."
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
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
// What that leaves out is a hex id written shorter than its length, which Tempo's search API
// does: it reports a trace id with its leading zeros dropped. Such an id is not recognised here
// and is left as protojson read it. It does not reach a response - a search result is not OTLP
// and does not come through here, and the trace id of a trace detail is the one the request
// asked for - but a backend that dropped a leading zero from an id inside an OTLP body would go
// unrepaired.
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
	}

	return nil, fmt.Errorf("[OTLP JSON] no span list in a trace response: it has none of %q, %q and %q, only %q",
		keyResourceSpans, keyBatches, keyTrace, slices.Sorted(maps.Keys(envelope)))
}

// Attributes is a list of OTLP attributes carried inside a document that is not itself OTLP.
// Tempo's search API answers in a shape of its own and puts the attributes of a matched span
// in it unchanged, so they arrive through encoding/json, which cannot read an AnyValue: its
// variants are a protobuf oneof, and a oneof is a Go interface the generated code fills in.
// Each attribute is handed to protojson on its own instead.
type Attributes []*commonv1.KeyValue

func (a *Attributes) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, jsonNull) {
		return nil
	}

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

// Status is an OTLP span status carried the same way, so that its code arrives as the enum the
// proto declares rather than as whichever text or number the backend wrote it as.
type Status struct {
	Code tracev1.Status_StatusCode
}

func (s *Status) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, jsonNull) {
		return nil
	}

	status := &tracev1.Status{}
	if err := unmarshalOptions.Unmarshal(data, status); err != nil {
		// as with an attribute: a status that cannot be read costs one span its error tag, and
		// must not cost the search its traces
		log.Warningf("[OTLP JSON] Could not read the status of a span: %s", err)
		return nil
	}
	s.Code = status.GetCode()
	return nil
}
