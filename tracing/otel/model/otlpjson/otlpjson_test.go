package otlpjson

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	// a real Tempo /api/traces/{id} response, in Tempo's own "batches" envelope. Its spans
	// start on 2023-12-05, so it predates the Tempo running today; ../../../tracingtest/README.adoc
	// gives every fixture's provenance and the command to capture it again.
	responseTrace = "../../../tracingtest/responseTrace.json"
	// a real Tempo 3.1.0 /api/search response to Kiali's own TraceQL, in Tempo's search shape
	responseTypedSearch = "../../../tracingtest/responseTypedSearch.json"
)

// TestUnmarshalEnvelopes covers the envelopes a trace response can arrive in. A response that
// uses none of them has to be reported rather than read as a trace with no spans.
func TestUnmarshalEnvelopes(t *testing.T) {
	spans := `[{"scopeSpans":[{"spans":[{"name":"one"}]}]}]`

	cases := map[string]struct {
		body      string
		wantSpans int
		wantErr   bool
	}{
		"OTLP, as the encoding defines it": {body: `{"resourceSpans":` + spans + `}`, wantSpans: 1},
		"Tempo's trace API":                {body: `{"batches":` + spans + `}`, wantSpans: 1},
		"Tempo's v2 trace API":             {body: `{"trace":{"resourceSpans":` + spans + `}}`, wantSpans: 1},
		"Jaeger's api_v3":                  {body: `{"result":{"resourceSpans":` + spans + `}}`, wantSpans: 1},
		"a trace that holds no spans":      {body: `{}`, wantSpans: 0},
		"an explicitly empty span list":    {body: `{"resourceSpans":[]}`, wantSpans: 0},
		"an unrecognised shape":            {body: `{"traces":[]}`, wantErr: true},
		"not an object at all":             {body: `[]`, wantErr: true},
		"a wrapper nested twice":           {body: `{"trace":{"trace":{"resourceSpans":` + spans + `}}}`, wantErr: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := UnmarshalTracesData([]byte(tc.body))
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, data.GetResourceSpans(), tc.wantSpans)
		})
	}
}

// TestUnmarshalCapturedResponse reads the captured Tempo trace response, whose ids are base64
// because that is what Tempo writes, and checks the fields the converter goes on to read.
func TestUnmarshalCapturedResponse(t *testing.T) {
	body, err := os.ReadFile(responseTrace)
	require.NoError(t, err)

	data, err := UnmarshalTracesData(body)
	require.NoError(t, err)
	assert.Len(t, data.GetResourceSpans(), 6)

	span := data.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	assert.Equal(t, "3ba55609c3cde49649cd77d1f9dcd936", hex.EncodeToString(span.GetTraceId()))
	assert.Equal(t, "49cd77d1f9dcd936", hex.EncodeToString(span.GetSpanId()))
	assert.Equal(t, tracev1.Span_SPAN_KIND_CLIENT, span.GetKind())
	assert.Equal(t, uint64(1701779876570888000), span.GetStartTimeUnixNano())
	assert.Equal(t, "10.244.0.1", span.GetAttributes()[0].GetValue().GetStringValue())
}

// TestUnmarshalJaegerAPIv3 reads a Jaeger api_v3 body as it really arrives, trimmed to one
// span. Two things about it are not Tempo's shape: the OTLP payload is nested under "result",
// which is what a gRPC server-streaming method looks like over JSON, and the ids are the hex
// text the OTLP/JSON encoding mandates rather than base64. Measured against a live Jaeger
// 2.20.0, on both its FindTraces and its GetTrace endpoint.
func TestUnmarshalJaegerAPIv3(t *testing.T) {
	const body = `{"result":{"resourceSpans":[{` +
		`"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"waypoint.bookinfo"}}]},` +
		`"scopeSpans":[{"scope":{"name":"envoy","version":"3bb16354647d089b9a79020c9fc86cbbc5fcf84b/1.39.2-dev/Clean/RELEASE/BoringSSL"},` +
		`"spans":[{"traceId":"5d0e0849a1eda30cefb3477ad706f301","spanId":"d7994c02b19fdc42",` +
		`"name":"details.bookinfo.svc.cluster.local:9080/*","kind":2,` +
		`"startTimeUnixNano":"1791032992019454593","endTimeUnixNano":"1791032992021454593",` +
		`"status":{}}]}]}]}}`

	data, err := UnmarshalTracesData([]byte(body))
	require.NoError(t, err)
	require.Len(t, data.GetResourceSpans(), 1)

	scopeSpans := data.GetResourceSpans()[0].GetScopeSpans()[0]
	assert.Equal(t, "envoy", scopeSpans.GetScope().GetName())

	span := scopeSpans.GetSpans()[0]
	assert.Equal(t, "5d0e0849a1eda30cefb3477ad706f301", hex.EncodeToString(span.GetTraceId()))
	assert.Equal(t, "d7994c02b19fdc42", hex.EncodeToString(span.GetSpanId()))
	assert.Equal(t, tracev1.Span_SPAN_KIND_SERVER, span.GetKind())
}

// TestUnmarshalSpanDialects covers the two JSON dialects a backend can answer in. The OTLP/JSON
// encoding writes an enum as its number and permits a 64 bit integer as a bare number; the
// protobuf JSON mapping it derives from writes an enum as its name and an integer as a string,
// which is what Tempo sends. protojson reads either, so neither form needs code here.
func TestUnmarshalSpanDialects(t *testing.T) {
	cases := map[string]string{
		"the protobuf JSON mapping, as Tempo writes it": `{"kind":"SPAN_KIND_SERVER","status":{"code":"STATUS_CODE_ERROR"},"startTimeUnixNano":"1701779876570888000"}`,
		"OTLP/JSON, as its own rules require":           `{"kind":2,"status":{"code":2},"startTimeUnixNano":1701779876570888000}`,
		"the two forms mixed":                           `{"kind":2,"status":{"code":"STATUS_CODE_ERROR"},"startTimeUnixNano":"1701779876570888000"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := UnmarshalTracesData([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[` + body + `]}]}]}`))
			require.NoError(t, err)

			span := data.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
			assert.Equal(t, tracev1.Span_SPAN_KIND_SERVER, span.GetKind())
			assert.Equal(t, tracev1.Status_STATUS_CODE_ERROR, span.GetStatus().GetCode())
			assert.Equal(t, uint64(1701779876570888000), span.GetStartTimeUnixNano())
		})
	}
}

// TestUnmarshalAnyValueVariants covers every variant an OTLP attribute value can be written as.
// Kiali used to declare the value as one field, stringValue, so all but the first of these
// decoded to the empty string with no error reported anywhere.
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
		"an array":                                          {value: `{"arrayValue":{"values":[{"stringValue":"a"}]}}`, want: "a"},
		"a map":                                             {value: `{"kvlistValue":{"values":[{"key":"k","value":{"stringValue":"v"}}]}}`, want: "v"},
		"no variant set":                                    {value: `{}`, want: nil},
		"a variant we do not know":                          {value: `{"somethingLaterValue":"x"}`, want: nil},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"attributes":[{"key":"k","value":` + tc.value + `}]}]}]}]}`
			data, err := UnmarshalTracesData([]byte(body))
			require.NoError(t, err)

			value := data.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0].GetAttributes()[0].GetValue()
			switch want := tc.want.(type) {
			case nil:
				assert.Nil(t, value.GetValue())
			case string:
				// a string, or the only leaf of the array or the map
				assert.Contains(t, value.String(), want)
			case bool:
				assert.Equal(t, want, value.GetBoolValue())
			case int64:
				assert.Equal(t, want, value.GetIntValue())
			case float64:
				assert.Equal(t, want, value.GetDoubleValue())
			case []byte:
				assert.Equal(t, want, value.GetBytesValue())
			}
		})
	}
}

// TestAttributesBridge reads the attributes of a span matched by Tempo's search API, which
// arrive inside Tempo's own shape and so go through encoding/json rather than protojson.
//
// The two typed keys are .component and .response_flags, because those are keys Kiali's own
// select list names: an attribute only reaches this path if prepareTraceQL asked for it.
func TestAttributesBridge(t *testing.T) {
	body, err := os.ReadFile(responseTypedSearch)
	require.NoError(t, err)

	var response struct {
		Traces []struct {
			SpanSet struct {
				Spans []struct {
					Attributes Attributes `json:"attributes"`
				} `json:"spans"`
			} `json:"spanSet"`
		} `json:"traces"`
	}
	require.NoError(t, json.Unmarshal(body, &response))

	values := map[string]*commonv1.AnyValue{}
	for _, attribute := range response.Traces[0].SpanSet.Spans[0].Attributes {
		values[attribute.GetKey()] = attribute.GetValue()
	}
	assert.Equal(t, int64(7), values["component"].GetIntValue())
	assert.True(t, values["response_flags"].GetBoolValue())
	assert.Equal(t, "typeprobe.devex", values["service.name"].GetStringValue())
}

// TestAttributesBridgeTolerance covers what the bridge does with a list it cannot read in full.
// A search response carries one trace per matched trace, so an error here would answer a search
// with nothing at all; one attribute keeping its key and losing its value is what the model it
// replaces did, except that now something is said about it.
func TestAttributesBridgeTolerance(t *testing.T) {
	cases := map[string]struct {
		body string
		want []*commonv1.KeyValue
	}{
		"a list written as null": {body: `null`},
		"a list that is empty":   {body: `[]`, want: []*commonv1.KeyValue{}},
		"a value of the wrong type for its variant": {
			body: `[{"key":"first","value":{"intValue":"not a number"}},{"key":"second","value":{"stringValue":"kept"}}]`,
			want: []*commonv1.KeyValue{
				{Key: "first"},
				{Key: "second", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "kept"}}},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var attributes Attributes
			require.NoError(t, json.Unmarshal([]byte(tc.body), &attributes))
			require.Len(t, attributes, len(tc.want))
			for i, want := range tc.want {
				assert.Equal(t, want.GetKey(), attributes[i].GetKey())
				assert.Equal(t, want.GetValue().GetStringValue(), attributes[i].GetValue().GetStringValue())
			}
		})
	}
}

// TestUnmarshalHexIDs covers the one place the OTLP/JSON encoding departs from the protobuf
// JSON mapping. Both forms have to be read, and the hex one is the dangerous one: hex text is
// valid base64, so protojson reads it without reporting anything.
func TestUnmarshalHexIDs(t *testing.T) {
	const (
		traceID      = "887a6ab0bc0f8966281b801f5a8398b6"
		spanID       = "b2f7af65d533fede"
		parentSpanID = "1234567890abcdef"
	)

	cases := map[string]struct {
		ids string
	}{
		"hex, as the OTLP/JSON encoding requires": {
			ids: `"traceId":"` + traceID + `","spanId":"` + spanID + `","parentSpanId":"` + parentSpanID + `"`,
		},
		"hex in upper case, which the encoding calls case-insensitive": {
			ids: `"traceId":"887A6AB0BC0F8966281B801F5A8398B6","spanId":"B2F7AF65D533FEDE","parentSpanId":"1234567890ABCDEF"`,
		},
		"base64, as the protobuf JSON mapping defines it and Tempo writes it": {
			ids: `"traceId":"iHpqsLwPiWYoG4AfWoOYtg==","spanId":"svevZdUz/t4=","parentSpanId":"EjRWeJCrze8="`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"resourceSpans":[{"scopeSpans":[{"spans":[{` + tc.ids +
				`,"links":[{"traceId":"` + traceID + `","spanId":"` + spanID + `"}]}]}]}]}`
			data, err := UnmarshalTracesData([]byte(body))
			require.NoError(t, err)

			span := data.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
			assert.Equal(t, traceID, hex.EncodeToString(span.GetTraceId()))
			assert.Equal(t, spanID, hex.EncodeToString(span.GetSpanId()))
			assert.Equal(t, parentSpanID, hex.EncodeToString(span.GetParentSpanId()))
			assert.Equal(t, traceID, hex.EncodeToString(span.GetLinks()[0].GetTraceId()))
			assert.Equal(t, spanID, hex.EncodeToString(span.GetLinks()[0].GetSpanId()))
		})
	}
}

// TestDecodeHexID covers the lengths the repair is allowed to touch, since the length is the
// whole of what tells the two forms apart.
func TestDecodeHexID(t *testing.T) {
	// the 24 bytes protojson reads a 32 character hex trace id as
	misread, err := base64.RawStdEncoding.DecodeString("887a6ab0bc0f8966281b801f5a8398b6")
	require.NoError(t, err)
	require.Len(t, misread, 24)
	assert.Equal(t, "887a6ab0bc0f8966281b801f5a8398b6", hex.EncodeToString(decodeHexID(misread, traceIDSize)))

	// an id of the size the proto gives it is already what it should be, whatever it holds
	id := []byte("0123456789abcdef")
	assert.Equal(t, id, decodeHexID(id, traceIDSize))
	assert.Nil(t, decodeHexID(nil, traceIDSize))

	// 24 bytes whose base64 text is not hex throughout are left alone. A base64 alphabet holds
	// every hex digit, so the text has to be read rather than assumed.
	notHex, err := base64.RawStdEncoding.DecodeString("887a6ab0bc0f8966281b801f5a8398bZ")
	require.NoError(t, err)
	assert.Equal(t, notHex, decodeHexID(notHex, traceIDSize))

	// a hex id written shorter than its length is not recognised, which is the limit of reading
	// the length: Tempo's search API drops the leading zeros of a trace id, and an id written
	// that way inside an OTLP body would stay as protojson read it
	short, err := base64.RawStdEncoding.DecodeString("ee9204f76db57d0aa38482c1243cea1")
	require.NoError(t, err)
	require.Len(t, short, 23)
	assert.Equal(t, short, decodeHexID(short, traceIDSize))
}
