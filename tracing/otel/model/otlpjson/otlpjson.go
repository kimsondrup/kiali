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
	return data, nil
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
