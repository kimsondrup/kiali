package otlp

import (
	"encoding/json"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/kiali/kiali/log"
)

// Attributes is a list of OTLP attributes carried inside a document that is not itself OTLP.
// Tempo's search API answers in a shape of its own and puts the attributes of a matched span in
// it unchanged, so they arrive through encoding/json, which cannot read an AnyValue: its variants
// are a protobuf oneof, and a oneof is a Go interface the generated code fills in. Each attribute
// is handed to protojson on its own instead, under the same options Decode reads a whole payload
// with.
//
// A list written as null needs no case of its own: encoding/json hands the literal null to the
// method below, and reading it into the raw list yields no elements and no error.
type Attributes []*commonpb.KeyValue

func (a *Attributes) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	attributes := make(Attributes, 0, len(raw))
	for _, item := range raw {
		attribute := &commonpb.KeyValue{}
		if err := unmarshalOptions.Unmarshal(item, attribute); err != nil {
			// One attribute is not the document. Reporting an error here fails the whole search
			// response, and a traces list that goes empty over one odd value is worse than a
			// trace that carries one tag it cannot show: the key is kept, the value left unset,
			// and the reason logged.
			log.Warningf("OTLP attributes: could not read the value of span attribute %q: %s", attributeKey(item), err)
			attributes = append(attributes, &commonpb.KeyValue{Key: attributeKey(item)})
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
