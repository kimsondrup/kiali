package json

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

// OTEL

// ArrayValue is an OTLP array of values.
type ArrayValue struct {
	Values []AnyValue `json:"values"`
}

// KeyValueList is an OTLP map of values.
type KeyValueList struct {
	Values []Attribute `json:"values"`
}

// AnyValue is an OTLP attribute value. The proto declares it as a oneof over seven variants, so
// at most one of these fields is set. They are pointers so that a variant which is absent can be
// told apart from one that is present and empty.
// https://github.com/open-telemetry/opentelemetry-proto/blob/main/opentelemetry/proto/common/v1/common.proto
type AnyValue struct {
	ArrayValue  *ArrayValue   `json:"arrayValue,omitempty"`
	BoolValue   *bool         `json:"boolValue,omitempty"`
	BytesValue  *string       `json:"bytesValue,omitempty"`
	DoubleValue *float64      `json:"doubleValue,omitempty"`
	IntValue    *int64        `json:"intValue,omitempty"`
	KvlistValue *KeyValueList `json:"kvlistValue,omitempty"`
	StringValue *string       `json:"stringValue,omitempty"`
}

// UnmarshalJSON decodes an attribute value. The two numeric variants are read from their text,
// because the OTLP/JSON encoding writes a 64 bit integer as a decimal string and accepts either
// a string or a number for it, and writes a double as a number or as one of the special values
// "NaN", "Infinity" and "-Infinity".
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
//
// A variant whose value is not of its type is left unset rather than reported: encoding/json
// abandons the rest of the document at the first error an UnmarshalJSON method returns, so one
// attribute this model cannot read would cost every trace in the response.
func (v *AnyValue) UnmarshalJSON(data []byte) error {
	var variants struct {
		ArrayValue  *ArrayValue     `json:"arrayValue"`
		BoolValue   *bool           `json:"boolValue"`
		BytesValue  *string         `json:"bytesValue"`
		DoubleValue json.RawMessage `json:"doubleValue"`
		IntValue    json.RawMessage `json:"intValue"`
		KvlistValue *KeyValueList   `json:"kvlistValue"`
		StringValue *string         `json:"stringValue"`
	}
	// json skips a value it cannot read into its field and carries on, which is the behaviour
	// wanted here, so the error it reports for that value is dropped rather than returned.
	_ = json.Unmarshal(data, &variants)

	*v = AnyValue{
		ArrayValue:  variants.ArrayValue,
		BoolValue:   variants.BoolValue,
		BytesValue:  variants.BytesValue,
		DoubleValue: parseFloat64(variants.DoubleValue),
		IntValue:    parseInt64(variants.IntValue),
		KvlistValue: variants.KvlistValue,
		StringValue: variants.StringValue,
	}
	return nil
}

// parseInt64 reads the text of an OTLP 64 bit integer, which arrives quoted or bare and may be
// written in exponent notation either way.
func parseInt64(data []byte) *int64 {
	text := numberText(data)
	if text == "" {
		return nil
	}

	if value, err := strconv.ParseInt(text, 10, 64); err == nil {
		return &value
	}
	// exponent notation, which the encoding accepts for an integer and ParseInt does not read
	if value, err := strconv.ParseFloat(text, 64); err == nil &&
		value == math.Trunc(value) && value >= math.MinInt64 && value < math.MaxInt64 {
		whole := int64(value)
		return &whole
	}
	return nil
}

// parseFloat64 reads the text of an OTLP double, including the special values, which ParseFloat
// reads by their OTLP/JSON names.
func parseFloat64(data []byte) *float64 {
	text := numberText(data)
	if text == "" {
		return nil
	}

	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil
	}
	return &value
}

// numberText returns the text of a JSON number that may have arrived quoted, and the empty
// string for one that is absent or null.
func numberText(data []byte) string {
	text := string(bytes.Trim(bytes.TrimSpace(data), `"`))
	if text == "null" {
		return ""
	}
	return text
}

// value returns the value as a plain Go value, or nil when no variant is set. A bytes value is
// returned as the base64 text it arrives as, which is also how Jaeger renders a binary tag.
func (v AnyValue) value() any {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.IntValue != nil:
		return *v.IntValue
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.ArrayValue != nil:
		values := make([]any, 0, len(v.ArrayValue.Values))
		for _, item := range v.ArrayValue.Values {
			values = append(values, item.value())
		}
		return values
	case v.KvlistValue != nil:
		values := make(map[string]any, len(v.KvlistValue.Values))
		for _, item := range v.KvlistValue.Values {
			values[item.Key] = item.Value.value()
		}
		return values
	case v.BytesValue != nil:
		return *v.BytesValue
	}
	return nil
}

// String renders the value as text, for the call sites that only ever compare strings. An array
// and a kvlist are rendered as JSON, which is what Jaeger's own OTLP translator does with them.
func (v AnyValue) String() string {
	switch value := v.value().(type) {
	case nil:
		return ""
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64)
	default:
		text, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(text)
	}
}

type Attribute struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

type Event struct {
	TimeUnixNano string `json:"timeUnixNano"`
	Name         string `json:"name"`
}

type Status struct {
	Code string `json:"code"`
}

type Span struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	Name              string      `json:"name"`
	Kind              string      `json:"kind"`
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []Attribute `json:"attributes"`
	Events            []Event     `json:"events"`
	Status            Status      `json:"status"`
	ParentSpanId      string      `json:"parentSpanId"`
}

type ScopeSpan struct {
	Scope struct{} `json:"scope"`
	Spans []Span   `json:"spans"`
}

type Resource struct {
	Attributes []Attribute `json:"attributes"`
}

type Batch struct {
	Resource   Resource    `json:"resource"`
	ScopeSpans []ScopeSpan `json:"scopeSpans"`
}

type Data struct {
	Batches []Batch `json:"batches"`
}
