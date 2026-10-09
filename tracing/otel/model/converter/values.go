package converter

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/kiali/kiali/log"
	jaegerModels "github.com/kiali/kiali/tracing/jaeger/model/json"
)

// keyValueFromAttribute carries the type of an OTLP attribute over to Jaeger's typed key-value,
// so that a port, a payload size or a status code reaches the UI as the number it is. The second
// return reports a value written as a variant this build cannot read, which the caller names in
// one warning per span rather than one per attribute.
func keyValueFromAttribute(attribute *commonpb.KeyValue) (jaegerModels.KeyValue, bool) {
	plain := plainValue(attribute.GetValue())
	unread := plain == nil && attribute.GetValue().GetValue() != nil
	value, valueType := attributeValue(plain)
	return jaegerModels.KeyValue{Key: attribute.GetKey(), Value: value, Type: valueType}, unread
}

// attributeValue maps an OTLP attribute value, as plainValue returns it, onto a Jaeger tag value
// and the tag type that describes it.
//
// The type is carried rather than stringified because the attribute arrives with it: OTLP states
// which variant was written, Kiali's tag model has a field for it, and turning an int64 into text
// here would discard something no later reader can recover.
func attributeValue(value any) (any, jaegerModels.ValueType) {
	switch plain := value.(type) {
	case string:
		return plain, jaegerModels.StringType
	case bool:
		return plain, jaegerModels.BoolType
	case int64:
		return plain, jaegerModels.Int64Type
	case float64:
		// Finite by construction: plainValue hands a non-finite double over as text, because one
		// nested in an array or a map would fail the json.Marshal below.
		return plain, jaegerModels.Float64Type
	case []byte:
		// Rendered as base64 by encoding/json, which is the form OTLP itself writes bytes in.
		return plain, jaegerModels.BinaryType
	case nil:
		// No variant set, or one this build cannot read: the proto's profiling variant, or a
		// variant a later revision adds. Either is read as though the value were absent, and
		// warnUnreadVariants says which keys the second case applied to.
		return "", jaegerModels.StringType
	default:
		// An array or a map. Jaeger's key-value has no composite type, and Jaeger's own OTLP
		// translation renders these as JSON as well.
		text, err := json.Marshal(plain)
		if err != nil {
			log.Errorf("Could not render an attribute value of type [%T]: %s", plain, err)
			return "", jaegerModels.StringType
		}
		return string(text), jaegerModels.StringType
	}
}

// plainValue returns an OTLP attribute value as a plain Go value, nested values included, and nil
// when no variant of it is set.
//
// A double that is not a finite number comes back as its text rather than as a float64. JSON has
// no literal for NaN or an infinity, so json.Marshal refuses one, and refuses the whole value it
// sits in: left as a float64, a single NaN inside an array would cost that attribute every one of
// its elements, and a bare one would cost the whole Kiali response.
func plainValue(value *commonpb.AnyValue) any {
	switch variant := value.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return variant.StringValue
	case *commonpb.AnyValue_BoolValue:
		return variant.BoolValue
	case *commonpb.AnyValue_IntValue:
		return variant.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return finiteOrText(variant.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return variant.BytesValue
	case *commonpb.AnyValue_ArrayValue:
		items := variant.ArrayValue.GetValues()
		values := make([]any, 0, len(items))
		for _, item := range items {
			values = append(values, plainValue(item))
		}
		return values
	case *commonpb.AnyValue_KvlistValue:
		items := variant.KvlistValue.GetValues()
		values := make(map[string]any, len(items))
		for _, item := range items {
			values[item.GetKey()] = plainValue(item.GetValue())
		}
		return values
	}
	return nil
}

// finiteOrText returns a double as itself, or as its text when it is NaN or an infinity, so that
// the value can be written as JSON wherever it appears, nested or not.
func finiteOrText(value float64) any {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	return value
}

// warnUnreadVariants reports the attributes of one owner - a span, or the resource a span belongs
// to - whose value was written as an AnyValue variant this build cannot read, and which therefore
// arrive as a tag with an empty value. The only such variant today is string_value_strindex,
// which the proto reserves for the Profiling signal and whose own generated comment instructs a
// receiver of any other signal to "Log an error or warning indicating an unexpected field
// intended for the Profiling signal and process the data as if this value were absent or empty".
//
// A value with no variant set at all is not reported: an unset value is legal OTLP and says what
// a missing tag says.
//
// One line per owner, naming every key, rather than one line per attribute: a trace detail
// carries a few hundred attribute values and a search answer a few thousand, so a line each would
// bury the one span that has the problem. The owner is named so it can be found in Jaeger or
// Grafana.
func warnUnreadVariants(owner string, keys []string) {
	if len(keys) == 0 {
		return
	}
	log.Warningf("Could not read the value of attribute(s) %s of [%s]: written as an OTLP value variant this build does not know. Reporting them as empty",
		strings.Join(keys, ", "), owner)
}
