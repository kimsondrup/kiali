package otlp

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Captured live from Tempo's /api/traces/{id}: one trace of eight spans.
const tempoTraceHTTP = "../../tracingtest/responseTrace.json"

func TestDecodeTempoTraceOverHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	traces, err := Decode(fixture(t, tempoTraceHTTP), RootKeyBatches)
	require.NoError(err)

	all := spans(traces)
	require.Len(all, 8)
	assert.Len(traceIDs(traces), 1)
	for _, span := range all {
		assert.Len(span.GetTraceId(), 16)
		assert.Len(span.GetSpanId(), 8)
	}
	assert.Equal("3ba55609c3cde49649cd77d1f9dcd936", hex.EncodeToString(all[0].GetTraceId()))
	assert.Equal("49cd77d1f9dcd936", hex.EncodeToString(all[0].GetSpanId()))
}

// An ID short of its field is still base64, so protojson reads it and reports
// nothing. What comes out names a trace that does not exist, and the width is
// the only evidence of it.
func TestDecodeRejectsATruncatedID(t *testing.T) {
	body := `{"batches":[{"scopeSpans":[{"spans":[{"traceId":"ITLK","spanId":"tTM6/ykz9cU=","name":"n"}]}]}]}`

	traces, err := Decode([]byte(body), RootKeyBatches)
	assert.ErrorContains(t, err, `span "n" carries a 3-byte trace ID, not 16`)
	assert.Nil(t, traces)
}

func TestDecodeOddPayloads(t *testing.T) {
	cases := map[string]struct {
		body          string
		rootKey       string
		resourceSpans int
		spans         int
		wantErr       string
	}{
		"no batches": {
			body:    `{"batches":[]}`,
			rootKey: RootKeyBatches,
		},
		"bare traces data": {
			body: `{}`,
		},
		"resource span with no scope spans": {
			body:          `{"batches":[{"resource":{}}]}`,
			rootKey:       RootKeyBatches,
			resourceSpans: 1,
		},
		"scope span with no spans": {
			body:          `{"batches":[{"scopeSpans":[{}]}]}`,
			rootKey:       RootKeyBatches,
			resourceSpans: 1,
		},
		"two spans in one batch": {
			body:          `{"batches":[{"scopeSpans":[{"spans":[{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"tTM6/ykz9cU=","name":"n"},{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"mvdAi4Mg8i8=","name":"m"}]}]}]}`,
			rootKey:       RootKeyBatches,
			resourceSpans: 1,
			spans:         2,
		},
		// Tempo's other detail route, /api/v2/traces/{id}, holds the payload as
		// an object under a key of its own rather than as an array, which the
		// same root key reads unchanged.
		"an object under the root key": {
			body:          `{"metrics":{"inspectedBytes":"1234"},"trace":{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"ITLKoFymTIJFRgAoiyfztA==","spanId":"tTM6/ykz9cU=","name":"n"}]}]}]}}`,
			rootKey:       "trace",
			resourceSpans: 1,
			spans:         1,
		},
		"an object under a root key the transport did not name": {
			body:    `{"metrics":{"inspectedBytes":"1234"},"trace":{"resourceSpans":[]}}`,
			rootKey: RootKeyBatches,
			wantErr: `carries no "batches" key`,
		},
		"root key missing": {
			body:    `{}`,
			rootKey: RootKeyBatches,
			wantErr: `carries no "batches" key`,
		},
		"a search answer sent as a trace detail": {
			body:    `{"traces":[]}`,
			rootKey: RootKeyBatches,
			wantErr: `carries no "batches" key`,
		},
		"an HTML error page": {
			body:    "<html><body>502 Bad Gateway</body></html>",
			rootKey: RootKeyBatches,
			wantErr: "unreadable body",
		},
		"truncated json": {
			body:    `{"batches":[{"scopeSpans":`,
			rootKey: RootKeyBatches,
			wantErr: "unreadable body",
		},
		// A body holding the payload twice is not that payload, and taking the
		// first object reports a part of an answer as the whole of it.
		"the payload sent twice": {
			body:    `{"batches":[]}{"batches":[]}`,
			rootKey: RootKeyBatches,
			wantErr: "data after the end of the payload",
		},
		"empty body": {
			body:    "",
			rootKey: RootKeyBatches,
			wantErr: "empty body",
		},
		"whitespace only": {
			body:    "\n  \n",
			rootKey: RootKeyBatches,
			wantErr: "empty body",
		},
		"a json array where an object belongs": {
			body:    `[]`,
			rootKey: RootKeyBatches,
			wantErr: "unreadable body",
		},
		"an unknown field inside the payload": {
			body:    `{"batches":[{"scopeSpans":[],"traceSummaries":[]}]}`,
			rootKey: RootKeyBatches,
			wantErr: "unknown field",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			traces, err := Decode([]byte(tc.body), tc.rootKey)
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
