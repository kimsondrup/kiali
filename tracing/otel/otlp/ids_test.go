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
	"google.golang.org/protobuf/proto"
)

func TestHexToBase64(t *testing.T) {
	assert := assert.New(t)

	encoded, isHex := hexToBase64("2132caa05ca64c82454600288b27f3b4", traceIDBytes)
	assert.True(isHex)
	assert.Equal("ITLKoFymTIJFRgAoiyfztA==", encoded)

	encoded, isHex = hexToBase64("b5333aff2933f5c5", spanIDBytes)
	assert.True(isHex)
	assert.Equal("tTM6/ykz9cU=", encoded)
}

// Hex refuses an odd number of digits, and a value that falls through here
// reaches protojson, which reads it as base64 without complaint and makes it
// another trace. So an ID short of its full width is zero-extended instead.
func TestHexToBase64PadsAnUnpaddedID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for short, padded := range map[string]string{
		"ba55609c3cde49649cd77d1f9dcd936": "0ba55609c3cde49649cd77d1f9dcd936",
		"a55609c3cde49649cd77d1f9dcd936":  "00a55609c3cde49649cd77d1f9dcd936",
		"3":                               "00000000000000000000000000000003",
	} {
		encoded, isHex := hexToBase64(short, traceIDBytes)
		require.True(isHex, short)
		raw, err := base64.StdEncoding.DecodeString(encoded)
		require.NoError(err)
		assert.Equal(padded, hex.EncodeToString(raw))
	}

	encoded, isHex := hexToBase64("9cd77d1f9dcd936", spanIDBytes)
	require.True(isHex)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(err)
	assert.Equal("09cd77d1f9dcd936", hex.EncodeToString(raw))
}

// Padding closes a gap on the left, never on the right: an ID wider than the
// field is not a short one and nothing can be assumed about it.
func TestHexToBase64RejectsAnOversizedID(t *testing.T) {
	assert := assert.New(t)

	_, isHex := hexToBase64("2132caa05ca64c82454600288b27f3b4ab", traceIDBytes)
	assert.False(isHex)

	_, isHex = hexToBase64("b5333aff2933f5c5ab", spanIDBytes)
	assert.False(isHex)
}

// A base64 ID can never be read as a hex one: the 16- and 8-byte IDs OTLP
// carries encode as 24 and 12 characters ending in '=' padding, and hex accepts
// neither the padding nor the rest of the base64 alphabet.
func TestHexToBase64RejectsBase64(t *testing.T) {
	assert := assert.New(t)

	for id, size := range map[string]int{
		"ITLKoFymTIJFRgAoiyfztA==": traceIDBytes,
		"O6VWCcPN5JZJzXfR+dzZNg==": traceIDBytes,
		"tTM6/ykz9cU=":             spanIDBytes,
		"Sc130fnc2TY=":             spanIDBytes,
	} {
		_, isHex := hexToBase64(id, size)
		assert.False(isHex, id)
	}
}

// The rewrite runs over whatever the transport declared as hex, so it has to be
// provably harmless on a body that is already base64.
func TestNormalizeIDsIsANoOpOnBase64(t *testing.T) {
	require := require.New(t)

	var root map[string]json.RawMessage
	require.NoError(json.Unmarshal(fixture(t, tempoTraceHTTP), &root))
	payload := wrapResourceSpans(root[RootKeyBatches])

	rewritten, err := normalizeIDs(payload, Hex)
	require.NoError(err)

	asIs, viaRewrite := &tracepb.TracesData{}, &tracepb.TracesData{}
	require.NoError(protojson.Unmarshal(payload, asIs))
	require.NoError(protojson.Unmarshal(rewritten, viaRewrite))
	require.True(proto.Equal(asIs, viaRewrite))
}

func TestNormalizeIDsRewritesSpansAndLinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	payload := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{
		"traceId": "2132caa05ca64c82454600288b27f3b4",
		"spanId": "b5333aff2933f5c5",
		"parentSpanId": "9af7408b8320f22f",
		"name": "hex",
		"links": [{"traceId": "a999b181dcef7e89335c110d4a7ea6fe", "spanId": "b307fa7d04e21497"}]
	}]}]}]}`)

	rewritten, err := normalizeIDs(payload, Hex)
	require.NoError(err)

	traces := &tracepb.TracesData{}
	require.NoError(protojson.Unmarshal(rewritten, traces))

	all := spans(traces)
	require.Len(all, 1)
	assert.Equal("2132caa05ca64c82454600288b27f3b4", hex.EncodeToString(all[0].GetTraceId()))
	assert.Equal("b5333aff2933f5c5", hex.EncodeToString(all[0].GetSpanId()))
	assert.Equal("9af7408b8320f22f", hex.EncodeToString(all[0].GetParentSpanId()))
	require.Len(all[0].GetLinks(), 1)
	assert.Equal("a999b181dcef7e89335c110d4a7ea6fe", hex.EncodeToString(all[0].GetLinks()[0].GetTraceId()))
	assert.Equal("b307fa7d04e21497", hex.EncodeToString(all[0].GetLinks()[0].GetSpanId()))
}

func TestNormalizeIDsLeavesBase64PayloadsUntouched(t *testing.T) {
	payload := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"ITLKoFymTIJFRgAoiyfztA=="}]}]}]}`)

	rewritten, err := normalizeIDs(payload, Base64)
	require.NoError(t, err)
	assert.Equal(t, payload, []byte(rewritten))
}

// Every ID field is optional in OTLP/JSON, and a span can carry none of them.
func TestNormalizeIDsToleratesMissingAndOddFields(t *testing.T) {
	require := require.New(t)

	for _, payload := range []string{
		`{}`,
		`{"resourceSpans":[]}`,
		`{"resourceSpans":null}`,
		`{"resourceSpans":[{}]}`,
		`{"resourceSpans":[{"scopeSpans":[{"spans":[{}]}]}]}`,
		`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":""}]}]}]}`,
		`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":12}]}]}]}`,
	} {
		_, err := normalizeIDs([]byte(payload), Hex)
		require.NoError(err, payload)
	}
}

// Decoding the gRPC fixture both ways pins that hexToBase64 and the published
// base64 agree on the bytes, not just on the length.
func TestHexAndBase64AgreeOnTheSameIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	traces, err := Decode(fixture(t, apiv3TraceGRPC), Options{IDEncoding: Base64})
	require.NoError(err)

	for _, span := range spans(traces) {
		encoded, isHex := hexToBase64(hex.EncodeToString(span.GetSpanId()), spanIDBytes)
		require.True(isHex)
		assert.Equal(base64.StdEncoding.EncodeToString(span.GetSpanId()), encoded)
	}
}
