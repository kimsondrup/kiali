package otlp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnmarshalAnyValueVariants covers every variant an OTLP attribute value can be written as,
// read through the bridge Tempo's search answer needs. Kiali used to declare the value as one
// field, stringValue, so all but the first of these arrived as the empty string with nothing
// reported anywhere.
func TestUnmarshalAnyValueVariants(t *testing.T) {
	cases := map[string]struct {
		value string
		want  any
	}{
		"a string": {value: `{"stringValue":"HTTP/1.1"}`, want: "HTTP/1.1"},
		"a bool":   {value: `{"boolValue":true}`, want: true},
		"an int, as a string, which is how Tempo writes it": {value: `{"intValue":"503"}`, want: int64(503)},
		"an int, as a number":                               {value: `{"intValue":503}`, want: int64(503)},
		"a double":                                          {value: `{"doubleValue":1.5}`, want: 1.5},
		"a double, as a string":                             {value: `{"doubleValue":"1.5"}`, want: 1.5},
		"bytes":                                             {value: `{"bytesValue":"aGk="}`, want: []byte("hi")},
		"an array":                                          {value: `{"arrayValue":{"values":[{"stringValue":"a"}]}}`, want: []string{"a"}},
		"a map":                                             {value: `{"kvlistValue":{"values":[{"key":"k","value":{"stringValue":"v"}}]}}`, want: map[string]string{"k": "v"}},
		"no variant set":                                    {value: `{}`, want: nil},
		"a variant we do not know":                          {value: `{"somethingLaterValue":"x"}`, want: nil},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var attributes Attributes
			require.NoError(t, json.Unmarshal([]byte(`[{"key":"k","value":`+tc.value+`}]`), &attributes))
			require.Len(t, attributes, 1)
			assert.Equal(t, "k", attributes[0].GetKey())

			value := attributes[0].GetValue()
			switch want := tc.want.(type) {
			case nil:
				assert.Nil(t, value.GetValue())
			case string:
				assert.Equal(t, want, value.GetStringValue())
			case bool:
				assert.Equal(t, want, value.GetBoolValue())
			case int64:
				assert.Equal(t, want, value.GetIntValue())
			case float64:
				assert.Equal(t, want, value.GetDoubleValue())
			case []byte:
				assert.Equal(t, want, value.GetBytesValue())
			case []string:
				items := value.GetArrayValue().GetValues()
				require.Len(t, items, len(want))
				for i, item := range want {
					assert.Equal(t, item, items[i].GetStringValue())
				}
			case map[string]string:
				items := value.GetKvlistValue().GetValues()
				require.Len(t, items, len(want))
				for _, item := range items {
					assert.Equal(t, want[item.GetKey()], item.GetValue().GetStringValue())
				}
			}
		})
	}
}

// TestAttributesBridgeTolerance covers an attribute whose value this build cannot read. One
// attribute is not the document: a traces list that goes empty over one odd value is worse than
// a span that carries one tag with no value, so the key is kept and the rest of the span's
// attributes arrive intact.
func TestAttributesBridgeTolerance(t *testing.T) {
	t.Run("a list written as null", func(t *testing.T) {
		attributes := Attributes{{Key: "left over from a previous read"}}
		require.NoError(t, json.Unmarshal([]byte(`null`), &attributes))
		// encoding/json hands the literal null to UnmarshalJSON and documents it as meaning the
		// field is absent, so the list has to come back holding nothing at all rather than
		// whatever it held before.
		assert.NotNil(t, attributes)
		assert.Empty(t, attributes)
	})

	t.Run("a list that is empty", func(t *testing.T) {
		var attributes Attributes
		require.NoError(t, json.Unmarshal([]byte(`[]`), &attributes))
		assert.NotNil(t, attributes)
		assert.Empty(t, attributes)
	})

	t.Run("a value of the wrong type for its variant", func(t *testing.T) {
		body := `[{"key":"first","value":{"intValue":"not a number"}},` +
			`{"key":"second","value":{"stringValue":"kept"}}]`
		var attributes Attributes
		require.NoError(t, json.Unmarshal([]byte(body), &attributes))
		require.Len(t, attributes, 2)
		assert.Equal(t, "first", attributes[0].GetKey())
		assert.Nil(t, attributes[0].GetValue().GetValue())
		assert.Equal(t, "second", attributes[1].GetKey())
		assert.Equal(t, "kept", attributes[1].GetValue().GetStringValue())
	})

	t.Run("an attribute that is not an object", func(t *testing.T) {
		var attributes Attributes
		require.NoError(t, json.Unmarshal([]byte(`[7]`), &attributes))
		require.Len(t, attributes, 1)
		// attributeKey has nothing to read either, so the tag arrives with neither key nor value
		// rather than costing the response its traces.
		assert.Empty(t, attributes[0].GetKey())
	})

	t.Run("a list that is not a list", func(t *testing.T) {
		var attributes Attributes
		// A shape this far from an attribute list is the whole field being wrong, not one value,
		// so it is reported.
		assert.Error(t, json.Unmarshal([]byte(`{"key":"k"}`), &attributes))
	})
}
