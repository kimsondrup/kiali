package json

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpanKindUnmarshal checks that a span kind decodes from the number the OTLP/JSON encoding
// requires and from the name Tempo sends.
func TestSpanKindUnmarshal(t *testing.T) {
	cases := map[string]struct {
		body     string
		expected SpanKind
	}{
		"a name":                             {body: `{"kind":"SPAN_KIND_SERVER"}`, expected: SpanKindServer},
		"a number":                           {body: `{"kind":2}`, expected: SpanKindServer},
		"another number":                     {body: `{"kind":3}`, expected: SpanKindClient},
		"an unknown number":                  {body: `{"kind":99}`, expected: SpanKindUnspecified},
		"a name the proto does not have yet": {body: `{"kind":"SPAN_KIND_FUTURE"}`, expected: "SPAN_KIND_FUTURE"},
		"a number that is not a kind":        {body: `{"kind":2.5}`, expected: SpanKindUnspecified},
		"neither a name nor a number":        {body: `{"kind":{"name":"server"}}`, expected: SpanKindUnspecified},
		"null":                               {body: `{"kind":null}`, expected: ""},
		"absent":                             {body: `{}`, expected: ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var span Span
			require.NoError(t, json.Unmarshal([]byte(tc.body), &span))
			assert.Equal(t, tc.expected, span.Kind)
		})
	}
}

// TestStatusCodeUnmarshal checks the same for the span status code.
func TestStatusCodeUnmarshal(t *testing.T) {
	cases := map[string]struct {
		body     string
		expected StatusCode
	}{
		"a name":                      {body: `{"status":{"code":"STATUS_CODE_ERROR"}}`, expected: StatusCodeError},
		"a number":                    {body: `{"status":{"code":2}}`, expected: StatusCodeError},
		"another number":              {body: `{"status":{"code":1}}`, expected: StatusCodeOk},
		"an unknown number":           {body: `{"status":{"code":42}}`, expected: StatusCodeUnset},
		"with a message":              {body: `{"status":{"message":"rate limit exceeded","code":2}}`, expected: StatusCodeError},
		"neither a name nor a number": {body: `{"status":{"code":[2]}}`, expected: StatusCodeUnset},
		"an empty status":             {body: `{"status":{}}`, expected: ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var span Span
			require.NoError(t, json.Unmarshal([]byte(tc.body), &span))
			assert.Equal(t, tc.expected, span.Status.Code)
		})
	}
}

// TestSpanKindUnmarshalWholeResponse checks that a span kind given as a number no longer fails
// the response it arrives in. A kind is one field of one span, and decoding it into a string
// field failed the entire body.
func TestSpanKindUnmarshalWholeResponse(t *testing.T) {
	body := `{"batches":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[
		{"spanId":"1bab5054d0d765b0","kind":2,"status":{"code":2}}
	]}]}]}`

	var data Data
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	require.Len(t, data.Batches, 1)
	require.Len(t, data.Batches[0].ScopeSpans, 1)
	require.Len(t, data.Batches[0].ScopeSpans[0].Spans, 1)
	assert.Equal(t, SpanKindServer, data.Batches[0].ScopeSpans[0].Spans[0].Kind)
	assert.Equal(t, StatusCodeError, data.Batches[0].ScopeSpans[0].Spans[0].Status.Code)
}

// TestAnyValueUnmarshal covers every variant the OTLP AnyValue oneof declares, and every form
// the encoding permits for the two numeric ones. A variant the model does not carry decodes to
// an empty string and the attribute reaches the UI with no value.
func TestAnyValueUnmarshal(t *testing.T) {
	cases := map[string]struct {
		body         string
		expectedText string
		expected     func(*testing.T, AnyValue)
	}{
		"a string": {
			body:         `{"key":"http.method","value":{"stringValue":"GET"}}`,
			expectedText: "GET",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.StringValue)
				assert.Equal(t, "GET", *v.StringValue)
			},
		},
		"a quoted integer, which is the form the encoding writes": {
			body:         `{"key":"http.response.status_code","value":{"intValue":"503"}}`,
			expectedText: "503",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.IntValue)
				assert.Equal(t, int64(503), *v.IntValue)
			},
		},
		"a bare integer, which the encoding also accepts": {
			body:         `{"key":"net.host.port","value":{"intValue":9080}}`,
			expectedText: "9080",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.IntValue)
				assert.Equal(t, int64(9080), *v.IntValue)
			},
		},
		"an integer in exponent notation": {
			body:         `{"key":"http.request.body.size","value":{"intValue":"1e2"}}`,
			expectedText: "100",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.IntValue)
				assert.Equal(t, int64(100), *v.IntValue)
			},
		},
		"a negative integer": {
			body:         `{"key":"grpc.status","value":{"intValue":"-1"}}`,
			expectedText: "-1",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.IntValue)
				assert.Equal(t, int64(-1), *v.IntValue)
			},
		},
		"an integer that is not a number is left unset": {
			body:         `{"key":"http.request.body.size","value":{"intValue":"not-a-number"}}`,
			expectedText: "",
			expected: func(t *testing.T, v AnyValue) {
				assert.Nil(t, v.IntValue)
			},
		},
		"an empty integer is not a zero": {
			body:         `{"key":"http.request.body.size","value":{"intValue":""}}`,
			expectedText: "",
			expected: func(t *testing.T, v AnyValue) {
				assert.Nil(t, v.IntValue)
			},
		},
		"a boolean": {
			body:         `{"key":"error","value":{"boolValue":true}}`,
			expectedText: "true",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.BoolValue)
				assert.True(t, *v.BoolValue)
			},
		},
		"a double": {
			body:         `{"key":"sampler.param","value":{"doubleValue":0.25}}`,
			expectedText: "0.25",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.DoubleValue)
				assert.Equal(t, 0.25, *v.DoubleValue)
			},
		},
		"a quoted double": {
			body:         `{"key":"sampler.param","value":{"doubleValue":"0.25"}}`,
			expectedText: "0.25",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.DoubleValue)
				assert.Equal(t, 0.25, *v.DoubleValue)
			},
		},
		"a double that is not a finite number": {
			body:         `{"key":"queue.ratio","value":{"doubleValue":"NaN"}}`,
			expectedText: "NaN",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.DoubleValue)
				assert.True(t, math.IsNaN(*v.DoubleValue))
			},
		},
		"an array": {
			body:         `{"key":"http.request.header.accept","value":{"arrayValue":{"values":[{"stringValue":"application/json"},{"intValue":"7"}]}}}`,
			expectedText: `["application/json",7]`,
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.ArrayValue)
				assert.Len(t, v.ArrayValue.Values, 2)
			},
		},
		"a kvlist": {
			body:         `{"key":"peer","value":{"kvlistValue":{"values":[{"key":"port","value":{"intValue":"9080"}}]}}}`,
			expectedText: `{"port":9080}`,
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.KvlistValue)
				assert.Len(t, v.KvlistValue.Values, 1)
			},
		},
		"bytes, which the encoding writes as base64": {
			body:         `{"key":"raw","value":{"bytesValue":"YWJj"}}`,
			expectedText: "YWJj",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.BytesValue)
				assert.Equal(t, "YWJj", *v.BytesValue)
			},
		},
		"no variant at all": {
			body:         `{"key":"empty","value":{}}`,
			expectedText: "",
			expected: func(t *testing.T, v AnyValue) {
				assert.Nil(t, v.StringValue)
				assert.Nil(t, v.IntValue)
			},
		},
		"an empty string is not an absent value": {
			body:         `{"key":"downstream_cluster","value":{"stringValue":""}}`,
			expectedText: "",
			expected: func(t *testing.T, v AnyValue) {
				require.NotNil(t, v.StringValue)
				assert.Empty(t, *v.StringValue)
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var attribute Attribute
			require.NoError(t, json.Unmarshal([]byte(tc.body), &attribute))
			assert.Equal(t, tc.expectedText, attribute.Value.String())
			tc.expected(t, attribute.Value)
		})
	}
}

// TestAnyValueUnmarshalKeepsTheResponse checks that an attribute value the model cannot read
// costs that one value and nothing else. json stops at the first error an UnmarshalJSON method
// returns, so reporting the value would lose every span in the response along with it.
func TestAnyValueUnmarshalKeepsTheResponse(t *testing.T) {
	body := `{"batches":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[
		{"spanId":"1bab5054d0d765b0","attributes":[
			{"key":"http.request.body.size","value":{"intValue":"not-a-number"}},
			{"key":"http.method","value":{"stringValue":"GET"}}
		]},
		{"spanId":"a56799db118373dc","name":"details.bookinfo"}
	]}]}]}`

	var data Data
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	require.Len(t, data.Batches, 1)
	require.Len(t, data.Batches[0].ScopeSpans, 1)

	spans := data.Batches[0].ScopeSpans[0].Spans
	require.Len(t, spans, 2)
	require.Len(t, spans[0].Attributes, 2)
	assert.Nil(t, spans[0].Attributes[0].Value.IntValue)
	assert.Equal(t, "GET", spans[0].Attributes[1].Value.String())
	assert.Equal(t, "details.bookinfo", spans[1].Name)
}
