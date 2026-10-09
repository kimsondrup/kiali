package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Captured live: Jaeger 2.21.0 over HTTP and over gRPC, holding the same spans,
// and Tempo over HTTP.
const (
	apiv3SearchHTTP = "../../tracingtest/apiv3_search_http.json"
	apiv3TraceHTTP  = "../../tracingtest/apiv3_trace_http.json"
	apiv3TraceGRPC  = "../../tracingtest/apiv3_trace_grpc.json"
	tempoTraceHTTP  = "../../tracingtest/responseTrace.json"
)

func TestDecodeAPIV3SearchOverHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	traces, err := Decode(fixture(t, apiv3SearchHTTP), Options{RootKey: RootKeyResult, IDEncoding: Hex})
	require.NoError(err)

	// Three traces spread over six resource spans: a trace straddles them, so a
	// reader that takes one resource span for one trace loses spans.
	assert.Len(traces.GetResourceSpans(), 6)
	assert.Len(spans(traces), 9)
	assert.Len(traceIDs(traces), 3)
	assert.Equal([]string{
		"2132caa05ca64c82454600288b27f3b4",
		"a999b181dcef7e89335c110d4a7ea6fe",
		"d589cf359dfd4c0f70f68e501d8df307",
	}, traceIDs(traces))
}

func TestDecodeAPIV3TraceOverHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	traces, err := Decode(fixture(t, apiv3TraceHTTP), Options{RootKey: RootKeyResult, IDEncoding: Hex})
	require.NoError(err)

	all := spans(traces)
	require.Len(all, 3)
	for _, span := range all {
		assert.Len(span.GetTraceId(), 16)
		assert.Len(span.GetSpanId(), 8)
	}
	assert.Equal("2132caa05ca64c82454600288b27f3b4", hex.EncodeToString(all[0].GetTraceId()))
	assert.Equal("b5333aff2933f5c5", hex.EncodeToString(all[0].GetSpanId()))
	assert.Empty(all[0].GetParentSpanId())
	assert.Equal("9af7408b8320f22f", hex.EncodeToString(all[1].GetParentSpanId()))
}

// Both transports of the same backend must produce the same trace, which is the
// only check that the hex rewrite lands on the same bytes gRPC delivers natively.
func TestDecodeAPIV3TraceIsTheSameOverBothTransports(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	overHTTP, err := Decode(fixture(t, apiv3TraceHTTP), Options{RootKey: RootKeyResult, IDEncoding: Hex})
	require.NoError(err)
	overGRPC, err := Decode(fixture(t, apiv3TraceGRPC), Options{IDEncoding: Base64})
	require.NoError(err)

	httpSpans, grpcSpans := spans(overHTTP), spans(overGRPC)
	require.Len(grpcSpans, len(httpSpans))
	for i, span := range httpSpans {
		assert.Equal(grpcSpans[i].GetTraceId(), span.GetTraceId())
		assert.Equal(grpcSpans[i].GetSpanId(), span.GetSpanId())
		assert.Equal(grpcSpans[i].GetParentSpanId(), span.GetParentSpanId())
	}
}

// Tempo renames the resource spans array and renders real base64, so its detail
// body needs the same decoder with two different options and nothing else.
func TestDecodeTempoTraceOverHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	traces, err := Decode(fixture(t, tempoTraceHTTP), Options{RootKey: RootKeyBatches, IDEncoding: Base64})
	require.NoError(err)

	all := spans(traces)
	require.Len(all, 8)
	for _, span := range all {
		assert.Len(span.GetTraceId(), 16)
		assert.Len(span.GetSpanId(), 8)
	}
	assert.Equal("3ba55609c3cde49649cd77d1f9dcd936", hex.EncodeToString(all[0].GetTraceId()))
	assert.Equal("49cd77d1f9dcd936", hex.EncodeToString(all[0].GetSpanId()))
}

// The trap this package exists for: 32 hex characters are valid base64, so
// protojson decodes an api_v3 body in strict mode, reports no error, and hands
// back 24 bytes of a trace ID that does not exist.
func TestBareProtojsonCorruptsAPIV3IDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var root map[string]json.RawMessage
	require.NoError(json.Unmarshal(fixture(t, apiv3TraceHTTP), &root))

	traces := &tracepb.TracesData{}
	require.NoError(protojson.Unmarshal(root[RootKeyResult], traces))

	all := spans(traces)
	require.Len(all, 3)
	assert.Len(all[0].GetTraceId(), 24)
	assert.Len(all[0].GetSpanId(), 12)

	corrupted, err := base64.StdEncoding.DecodeString("2132caa05ca64c82454600288b27f3b4")
	require.NoError(err)
	assert.Equal(corrupted, all[0].GetTraceId())
	assert.Equal("db5df671a6b4e5c6bae1cf36e39e3ad34dbcf1bdbb7f76f8", hex.EncodeToString(all[0].GetTraceId()))
}

// Read under the encoding of the other transport, the same body yields IDs of a
// width OTLP has no room for. protojson reports nothing, because hex and base64
// share an alphabet, so the width is the only evidence there is. The reverse
// direction is harmless by construction and stays silent: the hex rewrite is a
// no-op on a base64 body.
func TestDecodeRejectsABodyReadUnderTheWrongIDEncoding(t *testing.T) {
	assert := assert.New(t)

	traces, err := Decode(fixture(t, apiv3TraceHTTP), Options{RootKey: RootKeyResult, IDEncoding: Base64})
	assert.ErrorContains(err, "24-byte trace ID")
	assert.Nil(traces)

	traces, err = Decode(fixture(t, apiv3SearchHTTP), Options{RootKey: RootKeyResult, IDEncoding: Base64})
	assert.ErrorContains(err, "24-byte trace ID")
	assert.Nil(traces)
}

// An ID declared as hex but in no encoding at all still reaches protojson,
// which reads whatever of it the base64 alphabet covers and reports nothing.
func TestDecodeRejectsAnIDThatIsNotHex(t *testing.T) {
	body := `{"result":{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"ZZZZ","spanId":"b5333aff2933f5c5","name":"n"}]}]}]}}`

	traces, err := Decode([]byte(body), Options{RootKey: RootKeyResult, IDEncoding: Hex})
	assert.ErrorContains(t, err, `span "n" carries a 3-byte trace ID, not 16`)
	assert.Nil(t, traces)
}

// An ID short of the 32 digits hex renders is zero-extended rather than left to
// protojson, which reads it as base64 and silently makes it another trace.
// api_v3 pads, so this is a guard rather than a shape it was seen to send.
func TestDecodeAcceptsAnUnpaddedHexID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	body := `{"result":{"resourceSpans":[{"scopeSpans":[{"spans":[{
		"traceId": "ba55609c3cde49649cd77d1f9dcd936",
		"spanId": "9cd77d1f9dcd936",
		"parentSpanId": "af7408b8320f22f",
		"name": "n",
		"links": [{"traceId": "999b181dcef7e89335c110d4a7ea6fe", "spanId": "307fa7d04e21497"}]
	}]}]}]}}`

	traces, err := Decode([]byte(body), Options{RootKey: RootKeyResult, IDEncoding: Hex})
	require.NoError(err)

	all := spans(traces)
	require.Len(all, 1)
	assert.Equal("0ba55609c3cde49649cd77d1f9dcd936", hex.EncodeToString(all[0].GetTraceId()))
	assert.Equal("09cd77d1f9dcd936", hex.EncodeToString(all[0].GetSpanId()))
	assert.Equal("0af7408b8320f22f", hex.EncodeToString(all[0].GetParentSpanId()))
	require.Len(all[0].GetLinks(), 1)
	assert.Equal("0999b181dcef7e89335c110d4a7ea6fe", hex.EncodeToString(all[0].GetLinks()[0].GetTraceId()))
	assert.Equal("0307fa7d04e21497", hex.EncodeToString(all[0].GetLinks()[0].GetSpanId()))
}

// A base64 ID short of its field is still base64, so protojson reads it and
// reports nothing whatever the transport declared.
func TestDecodeRejectsATruncatedID(t *testing.T) {
	body := `{"batches":[{"scopeSpans":[{"spans":[{"traceId":"ITLK","spanId":"tTM6/ykz9cU=","name":"n"}]}]}]}`

	traces, err := Decode([]byte(body), Options{RootKey: RootKeyBatches, IDEncoding: Base64})
	assert.ErrorContains(t, err, `span "n" carries a 3-byte trace ID, not 16`)
	assert.Nil(t, traces)
}

func TestDecodeRejectsAnUndeclaredIDEncoding(t *testing.T) {
	_, err := Decode(fixture(t, apiv3TraceHTTP), Options{RootKey: RootKeyResult})
	assert.ErrorContains(t, err, "ID encoding")
}

// api_v3's IDL reserves the right to answer several responses under
// Transfer-Encoding: chunked. Today it sends one.
func TestDecodeAccumulatesConcatenatedObjects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	one := fixture(t, apiv3TraceHTTP)
	body := append(append([]byte{}, one...), one...)

	traces, err := Decode(body, Options{RootKey: RootKeyResult, IDEncoding: Hex})
	require.NoError(err)
	assert.Len(spans(traces), 6)
	assert.Len(traceIDs(traces), 1)
}

func TestDecodeOddPayloads(t *testing.T) {
	cases := map[string]struct {
		body          string
		opts          Options
		resourceSpans int
		spans         int
		wantErr       string
	}{
		"api_v3 empty result": {
			body: `{"result":{}}`,
			opts: Options{RootKey: RootKeyResult, IDEncoding: Hex},
		},
		"api_v3 no resource spans": {
			body: `{"result":{"resourceSpans":[]}}`,
			opts: Options{RootKey: RootKeyResult, IDEncoding: Hex},
		},
		"tempo no batches": {
			body: `{"batches":[]}`,
			opts: Options{RootKey: RootKeyBatches, IDEncoding: Base64},
		},
		"bare traces data": {
			body: `{}`,
			opts: Options{IDEncoding: Base64},
		},
		"resource span with no scope spans": {
			body:          `{"result":{"resourceSpans":[{"resource":{}}]}}`,
			opts:          Options{RootKey: RootKeyResult, IDEncoding: Hex},
			resourceSpans: 1,
		},
		"scope span with no spans": {
			body:          `{"result":{"resourceSpans":[{"scopeSpans":[{}]}]}}`,
			opts:          Options{RootKey: RootKeyResult, IDEncoding: Hex},
			resourceSpans: 1,
		},
		"api_v3 one span": {
			body:          `{"result":{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"2132caa05ca64c82454600288b27f3b4","spanId":"b5333aff2933f5c5","name":"n"}]}]}]}}`,
			opts:          Options{RootKey: RootKeyResult, IDEncoding: Hex},
			resourceSpans: 1,
			spans:         1,
		},
		"tempo two spans in one batch": {
			body:          `{"batches":[{"scopeSpans":[{"spans":[{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"tTM6/ykz9cU=","name":"n"},{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"mvdAi4Mg8i8=","name":"m"}]}]}]}`,
			opts:          Options{RootKey: RootKeyBatches, IDEncoding: Base64},
			resourceSpans: 1,
			spans:         2,
		},
		// Tempo's other detail route, /api/v2/traces/{id}, holds the payload as
		// an object under a key of its own rather than as an array, which the
		// same root key reads unchanged.
		"an object under the root key": {
			body:          `{"metrics":{"inspectedBytes":"1234"},"trace":{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"tTM6/ykz9cU=","name":"n"}]}]}]}}`,
			opts:          Options{RootKey: RootKeyTrace, IDEncoding: Base64},
			resourceSpans: 1,
			spans:         1,
		},
		// The two cases the hand-written model answered by dropping the span: an end time it
		// could not parse left the span out of the trace and reported nothing. Tempo does not
		// send either shape, so the value of pinning them is the direction of the answer, not
		// the input: a timestamp that cannot be read is now the body's problem, not one span's.
		"an end time that is empty": {
			body:    `{"batches":[{"scopeSpans":[{"spans":[{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"tTM6/ykz9cU=","name":"n","startTimeUnixNano":"1693389472310270000","endTimeUnixNano":""}]}]}]}`,
			opts:    Options{RootKey: RootKeyBatches, IDEncoding: Base64},
			wantErr: `invalid value for fixed64 field endTimeUnixNano`,
		},
		"an end time that is not a number": {
			body:    `{"batches":[{"scopeSpans":[{"spans":[{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"tTM6/ykz9cU=","name":"n","startTimeUnixNano":"1693389472310270000","endTimeUnixNano":"soon"}]}]}]}`,
			opts:    Options{RootKey: RootKeyBatches, IDEncoding: Base64},
			wantErr: `invalid value for fixed64 field endTimeUnixNano`,
		},
		"an object under a root key the transport did not name": {
			body:    `{"metrics":{"inspectedBytes":"1234"},"trace":{"resourceSpans":[]}}`,
			opts:    Options{RootKey: RootKeyBatches, IDEncoding: Base64},
			wantErr: `carries no "batches" key`,
		},
		"a search answer sent as a trace detail": {
			body:    `{"traces":[]}`,
			opts:    Options{RootKey: RootKeyBatches, IDEncoding: Base64},
			wantErr: `carries no "batches" key`,
		},
		// An envelope with no keys at all is the one shape that means no spans
		// rather than a body Kiali could not read: a marshaller that leaves out
		// an empty field has nothing else to send. Every wrong shape below
		// carries keys, which is what keeps the two apart.
		"an envelope with no keys at all": {
			body: `{}`,
			opts: Options{RootKey: RootKeyResult, IDEncoding: Hex},
		},
		"a tempo envelope with no keys at all": {
			body: `{}`,
			opts: Options{RootKey: RootKeyBatches, IDEncoding: Base64},
		},
		"api_v3 not-found body": {
			body:    `{"error":{"httpCode":404,"message":"No traces found"}}`,
			opts:    Options{RootKey: RootKeyResult, IDEncoding: Hex},
			wantErr: `carries no "result" key`,
		},
		"an HTML error page": {
			body:    "<html><body>502 Bad Gateway</body></html>",
			opts:    Options{RootKey: RootKeyResult, IDEncoding: Hex},
			wantErr: "unreadable body",
		},
		"truncated json": {
			body:    `{"result":{"resourceSpans":[{"scopeSpans":`,
			opts:    Options{RootKey: RootKeyResult, IDEncoding: Hex},
			wantErr: "unreadable body",
		},
		"empty body": {
			body:    "",
			opts:    Options{RootKey: RootKeyResult, IDEncoding: Hex},
			wantErr: "empty body",
		},
		"whitespace only": {
			body:    "\n  \n",
			opts:    Options{RootKey: RootKeyResult, IDEncoding: Hex},
			wantErr: "empty body",
		},
		"a json array where an object belongs": {
			body:    `[]`,
			opts:    Options{RootKey: RootKeyResult, IDEncoding: Hex},
			wantErr: "unreadable body",
		},
		// The OTLP specification makes ignoring an unknown field a MUST, so a
		// field a later revision adds cannot blank the spans beside it. The cost
		// is that it is dropped with nothing said about it.
		"an unknown field beside the payload": {
			body:          `{"result":{"resourceSpans":[],"traceSummaries":[]}}`,
			opts:          Options{RootKey: RootKeyResult, IDEncoding: Hex},
			resourceSpans: 0,
		},
		"an unknown field beside a span": {
			body:          `{"result":{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"2132caa05ca64c82454600288b27f3b4","spanId":"b5333aff2933f5c5","name":"n","futureField":{"a":1}}]}]}]}}`,
			opts:          Options{RootKey: RootKeyResult, IDEncoding: Hex},
			resourceSpans: 1,
			spans:         1,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			traces, err := Decode([]byte(tc.body), tc.opts)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, traces)
				return
			}
			require.NoError(t, err)
			// A readable body with nothing in it decodes to an empty
			// TracesData. A nil one with a nil error is the silent empty the
			// callers above cannot tell from a backend that answered.
			require.NotNil(t, traces)
			assert.Len(t, traces.GetResourceSpans(), tc.resourceSpans)
			assert.Len(t, spans(traces), tc.spans)
		})
	}
}

// A body of the wrong shape is reported with the keys it actually carried, so
// that the error says which backend answered rather than only which key was
// looked for.
func TestDecodeNamesTheKeysAnEnvelopeCarried(t *testing.T) {
	assert := assert.New(t)

	cases := map[string]struct {
		body string
		opts Options
		want string
	}{
		"a jaeger body sent to the tempo detail path": {
			body: `{"data":[{"traceID":"abc","spans":[]}],"total":0}`,
			opts: Options{RootKey: RootKeyBatches, IDEncoding: Base64},
			want: `no "batches" key, only ["data" "total"]`,
		},
		"a tempo v2 body read under the v1 envelope": {
			body: `{"metrics":{"inspectedBytes":"1234"},"trace":{"resourceSpans":[]}}`,
			opts: Options{RootKey: RootKeyBatches, IDEncoding: Base64},
			want: `no "batches" key, only ["metrics" "trace"]`,
		},
		"an api_v3 error body": {
			body: `{"error":{"httpCode":404,"message":"No traces found"}}`,
			opts: Options{RootKey: RootKeyResult, IDEncoding: Hex},
			want: `no "result" key, only ["error"]`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			traces, err := Decode([]byte(tc.body), tc.opts)
			assert.ErrorContains(err, tc.want)
			assert.Nil(traces)
		})
	}
}

// Every form the two encodings permit for an enum and for a 64-bit timestamp,
// measured against the library rather than reimplemented: api_v3 renders
// status.code as the number 2 and Tempo renders kind as the name
// SPAN_KIND_SERVER, so both have to read and a decoder that handles one form has
// a gap on live data.
func TestDecodeWireFormsOfEnumsAndTimestamps(t *testing.T) {
	cases := map[string]struct {
		span      string
		wantKind  tracepb.Span_SpanKind
		wantCode  tracepb.Status_StatusCode
		wantStart uint64
		wantErr   string
	}{
		"a kind as the number OTLP/JSON requires": {
			span:     `"kind":2`,
			wantKind: tracepb.Span_SPAN_KIND_SERVER,
		},
		"a kind as the name Tempo sends": {
			span:     `"kind":"SPAN_KIND_SERVER"`,
			wantKind: tracepb.Span_SPAN_KIND_SERVER,
		},
		"a status code as the number api_v3 sends": {
			span:     `"status":{"code":2}`,
			wantCode: tracepb.Status_STATUS_CODE_ERROR,
		},
		"a status code as a name": {
			span:     `"status":{"code":"STATUS_CODE_ERROR","message":"upstream reset"}`,
			wantCode: tracepb.Status_STATUS_CODE_ERROR,
		},
		"a kind as a name and a status code as a number in one span": {
			span:     `"kind":"SPAN_KIND_CLIENT","status":{"code":2}`,
			wantKind: tracepb.Span_SPAN_KIND_CLIENT,
			wantCode: tracepb.Status_STATUS_CODE_ERROR,
		},
		"a kind as a number and a status code as a name in one span": {
			span:     `"kind":3,"status":{"code":"STATUS_CODE_OK"}`,
			wantKind: tracepb.Span_SPAN_KIND_CLIENT,
			wantCode: tracepb.Status_STATUS_CODE_OK,
		},
		// proto3 enums are open, so a number the generated code has no name for
		// is carried through rather than refused. Everything above the decode
		// reads an unrecognised kind the way it reads a missing one.
		"a kind number this build has no name for": {
			span:     `"kind":99`,
			wantKind: tracepb.Span_SpanKind(99),
		},
		"a status code number this build has no name for": {
			span:     `"status":{"code":42}`,
			wantCode: tracepb.Status_StatusCode(42),
		},
		// Measured: ignoring an unknown field also makes protojson ignore an
		// enum name it does not know, so a name from a later OTLP revision
		// arrives as the zero value with nothing said. The two forms below are
		// the price of the MUST above, and are pinned so a library change shows.
		"a kind name this build does not have": {
			span:     `"kind":"SPAN_KIND_FUTURE"`,
			wantKind: tracepb.Span_SPAN_KIND_UNSPECIFIED,
		},
		"a kind number written as a string": {
			span:     `"kind":"2"`,
			wantKind: tracepb.Span_SPAN_KIND_UNSPECIFIED,
		},
		"a timestamp as the quoted decimal the encoding writes": {
			span:      `"startTimeUnixNano":"1693389472310270000"`,
			wantStart: 1693389472310270000,
		},
		"a timestamp as a bare number": {
			span:      `"startTimeUnixNano":1693389472310270000`,
			wantStart: 1693389472310270000,
		},
		"a timestamp in exponent notation": {
			span:      `"startTimeUnixNano":1.6e18`,
			wantStart: 1600000000000000000,
		},
		// A value that is neither a name nor a number is reported rather than
		// quietly read as the zero value: the user sees a body Kiali could not
		// read instead of a trace that looks complete and is not.
		"a kind that is not a whole number": {
			span:    `"kind":2.5`,
			wantErr: "invalid value for enum field kind",
		},
		"a kind that is an object": {
			span:    `"kind":{"name":"server"}`,
			wantErr: "invalid value for enum field kind",
		},
		"a status code that is an array": {
			span:    `"status":{"code":[2]}`,
			wantErr: "invalid value for enum field code",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"batches":[{"scopeSpans":[{"spans":[{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"tTM6/ykz9cU=","name":"n",` + tc.span + `}]}]}]}`

			traces, err := Decode([]byte(body), Options{RootKey: RootKeyBatches, IDEncoding: Base64})
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, traces)
				return
			}
			require.NoError(t, err)

			all := spans(traces)
			require.Len(t, all, 1)
			assert.Equal(t, tc.wantKind, all[0].GetKind())
			assert.Equal(t, tc.wantCode, all[0].GetStatus().GetCode())
			assert.Equal(t, tc.wantStart, all[0].GetStartTimeUnixNano())
		})
	}
}
