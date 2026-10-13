package tracingcontract

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
	"github.com/kiali/kiali/tracing/otel/otlp"
)

// The widths OTLP gives the two IDs, and the hex text that carries them.
const (
	traceIDBytes  = 16
	spanIDBytes   = 8
	traceIDHexLen = 2 * traceIDBytes
	spanIDHexLen  = 2 * spanIDBytes
)

// idDepth bounds the gRPC search the ID rows read. They are about the spelling
// of one span's fields, so a handful of traces says as much as the window does.
const idDepth = 5

// tempoTracePath is Tempo's trace detail route, which Kiali reads as OTLP.
const tempoTracePath = "/api/traces/"

// spanIDFullWidth is the span-ID half of fullWidthHex.
var spanIDFullWidth = regexp.MustCompile(`^[0-9a-f]{16}$`)

// lowercaseHex matches a hex ID of any width, which is what a stripped one is.
var lowercaseHex = regexp.MustCompile(`^[0-9a-f]+$`)

// rawIDs are the ID fields of one span, kept as text, so the spelling on the
// wire is comparable with the spelling a decoder produced.
type rawIDs struct {
	name         string
	parentSpanID string
	spanID       string
	traceID      string
}

// TestAPIV3IDEncoding pins how api_v3 spells an ID on each of its transports.
//
// This is the one property of a trace body that the two transports of a single
// store disagree about and that no error reports: hex and base64 share the
// sixteen characters a hex ID is made of, so a body read under the wrong
// encoding decodes in strict mode without complaint and hands back IDs of the
// wrong width. The width is the whole of the evidence, which is why
// otlp.Options makes the encoding a declaration rather than something inferred.
func TestAPIV3IDEncoding(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			got := search(t, store, url.Values{"query.num_traces": {seedDepth}})
			require.Equal(t, http.StatusOK, got.status, "%s", got)
			spans := apiV3RawIDs(t, got)
			require.NotEmpty(t, spans, "the seeded window holds no span, so no row below means anything")

			t.Run("the HTTP gateway renders hex at full width", func(t *testing.T) {
				for _, span := range spans {
					assert.Regexp(t, fullWidthHex, span.traceID,
						"span %q carries a %d-character trace ID, where api_v3 renders %d lowercase hex",
						span.name, len(span.traceID), traceIDHexLen)
					assert.Regexp(t, spanIDFullWidth, span.spanID,
						"span %q carries a %d-character span ID, where api_v3 renders %d",
						span.name, len(span.spanID), spanIDHexLen)
					if span.parentSpanID != "" {
						assert.Regexp(t, spanIDFullWidth, span.parentSpanID,
							"span %q names its parent at a width of %d", span.name, len(span.parentSpanID))
					}
				}

				// The width is padding rather than an accident of the IDs this
				// window happens to hold, which only an ID carrying a leading
				// zero can show.
				assert.NotEmpty(t, leadingZeroTraceID(spans),
					"no trace ID in this window begins with a zero, so the full width above cannot be told from %d arbitrary hex characters: drive more traffic through the lab",
					traceIDHexLen)
			})

			t.Run("the shipped decoder reads it as hex and refuses it as base64", func(t *testing.T) {
				decoded, err := otlp.Decode(got.body, otlp.Options{RootKey: otlp.RootKeyResult, IDEncoding: otlp.Hex})
				require.NoError(t, err, "the shipped OTLP decoder could not read a live api_v3 body")
				assert.Equal(t, spans, decodedIDs(decoded),
					"every ID must survive the decode at the spelling the body gave it, span for span")

				// The trap, against a live body rather than a fixture. The
				// rewrite that avoids it is the only reason the row above
				// answers anything other than this one.
				wrong, err := otlp.Decode(got.body, otlp.Options{RootKey: otlp.RootKeyResult, IDEncoding: otlp.Base64})
				require.Error(t, err,
					"%d hex characters are themselves valid base64, so protojson reads them in strict mode and reports nothing", traceIDHexLen)
				assert.Nil(t, wrong)
				assert.Contains(t, err.Error(), "carries a 24-byte trace ID, not 16",
					"the width is what catches it, because nothing else in the body is wrong: %v", err)

				unset, err := otlp.Decode(got.body, otlp.Options{RootKey: otlp.RootKeyResult})
				require.Error(t, err,
					"an unset encoding must be refused rather than defaulted, because either default silently corrupts one of the two transports")
				assert.Nil(t, unset)
			})

			t.Run("the gRPC transport carries the same IDs as proto bytes", func(t *testing.T) {
				client, covered := grpcPeer(t, store.version)
				if !covered {
					t.Skipf("Jaeger %s is configured for HTTP only, so the pair this row compares is not available", store.version)
				}

				query := seededQuery()
				query.SearchDepth = idDepth
				traces := grpcTraces(t, client, query)

				var checked int
				for _, resource := range traces.GetResourceSpans() {
					for _, scope := range resource.GetScopeSpans() {
						for _, span := range scope.GetSpans() {
							checked++
							// No encoding question arises here, and that is
							// the assertion: the field is proto bytes, so its
							// width is OTLP's and the hex spelling is
							// something the reader chooses.
							assert.Len(t, span.GetTraceId(), traceIDBytes,
								"span %q carries a %d-byte trace ID over gRPC", span.GetName(), len(span.GetTraceId()))
							assert.Len(t, span.GetSpanId(), spanIDBytes,
								"span %q carries a %d-byte span ID over gRPC", span.GetName(), len(span.GetSpanId()))
						}
					}
				}
				require.NotZero(t, checked, "Jaeger %s answered the seeded search with no span", store.version)

				// And the bytes are the same bytes: the two transports differ
				// in spelling alone, so a trace found over one is the trace a
				// deep link resolves over the other.
				onGRPC := map[string]bool{}
				for _, span := range decodedIDs(traces) {
					onGRPC[span.traceID] = true
				}
				onHTTP := map[string]bool{}
				for _, span := range spans {
					onHTTP[span.traceID] = true
				}
				for id := range onGRPC {
					assert.True(t, onHTTP[id],
						"trace %s arrived over gRPC and is not among the %d the HTTP gateway reported for the same window", id, len(onHTTP))
				}
			})

			t.Run("a stripped trace ID and its padded form name one trace", func(t *testing.T) {
				// What makes Kiali's 1-to-32-hex guard safe and the padding of
				// a search answer cosmetic. A store that right-padded instead
				// would resolve a stripped ID to a different trace and answer
				// 200, so nothing above this line would notice.
				padded := requireLeadingZeroTraceID(t, spans)
				stripped := strings.TrimLeft(padded, "0")
				require.Less(t, len(stripped), traceIDHexLen, "%s has no leading zero to strip", padded)

				full := get(t, store.addr, tracePath+padded, nil)
				require.Equal(t, http.StatusOK, full.status, "%s", full)
				short := get(t, store.addr, tracePath+stripped, nil)
				require.Equal(t, http.StatusOK, short.status,
					"a trace ID short of its full width is the same ID, and the store resolves it: %s", short)

				assert.Equal(t, apiV3RawIDs(t, full), apiV3RawIDs(t, short),
					"%s and %s must answer one trace, span for span", padded, stripped)
				for _, span := range apiV3RawIDs(t, short) {
					assert.Equal(t, padded, span.traceID,
						"and the answer spells the ID at full width whichever form was asked for")
				}

				if client, covered := grpcPeer(t, store.version); covered {
					paddedIDs, _, err := getTrace(t, client, padded)
					require.NoError(t, err)
					strippedIDs, _, err := getTrace(t, client, stripped)
					require.NoError(t, err, "the gRPC transport resolves a stripped ID as its HTTP gateway does")
					assert.Equal(t, paddedIDs, strippedIDs)
					assert.Equal(t, []string{padded}, strippedIDs,
						"and answers the ID at full width, because the field is bytes and has no short form")
				}
			})

			t.Run("Kiali's ID guard admits what the store resolves", func(t *testing.T) {
				// The guard and the measured answer are asserted together,
				// because a guard narrower than the store refuses a deep link
				// that works, and a guard wider than it builds a request the
				// store answers 400.
				padded := requireLeadingZeroTraceID(t, spans)
				assert.Regexp(t, models.ValidTraceIDRe, padded)
				assert.Regexp(t, models.ValidTraceIDRe, strings.TrimLeft(padded, "0"),
					"a stripped ID has to pass, because Tempo spells about one search answer in sixteen that way and the deep link carries it")
				assert.NotRegexp(t, models.ValidTraceIDRe, malformedTraceID,
					"and an ID that is not hex has to be refused before the request is built")

				refused := get(t, store.addr, tracePath+malformedTraceID, nil)
				assert.Equal(t, http.StatusBadRequest, refused.status,
					"which is the answer the guard exists to avoid asking for: %s", refused)
			})
		})
	}
}

// TestTempoIDEncoding pins the other spelling, and the half of the hex trap
// that is not symmetric.
func TestTempoIDEncoding(t *testing.T) {
	addr := requireTempoHTTP(t)

	raw := tempoSearchIDs(t, addr)
	var stripped []string
	for _, id := range raw {
		if len(id) < traceIDHexLen {
			stripped = append(stripped, id)
		}
	}
	require.NotEmpty(t, stripped,
		"Tempo reported no trace ID short of %d characters among %d, and about one in sixteen is: drive more traffic through the lab",
		traceIDHexLen, len(raw))

	t.Run("the search answer strips leading zeros", func(t *testing.T) {
		for _, id := range stripped {
			assert.Regexp(t, lowercaseHex, id, "a short ID is still hex, only narrower")
			assert.NotEqual(t, byte('0'), id[0],
				"Tempo removes the leading zeros rather than moving them, so a short ID never begins with one")
		}
	})

	// Every row below is about one trace, and it is a stripped one: the padding
	// Kiali applies to a search answer is what turns it into the ID the trace
	// detail is asked for.
	padded := strings.Repeat("0", traceIDHexLen-len(stripped[0])) + stripped[0]
	detail := get(t, addr, tempoTracePath+padded, nil)
	require.Equal(t, http.StatusOK, detail.status,
		"Tempo must answer the detail of a trace its own search reported: %s", detail)

	t.Run("the trace detail wraps OTLP under batches with base64 IDs", func(t *testing.T) {
		assert.Equal(t, []string{otlp.RootKeyBatches}, topLevelKeys(t, detail),
			"Tempo names the resource spans array rather than wrapping a TracesData, which is why the root key is a transport declaration: %s", detail)

		spans := tempoRawIDs(t, detail)
		require.NotEmpty(t, spans, "the detail carries no span: %s", detail)
		for _, span := range spans {
			assert.NotRegexp(t, fullWidthHex, span.traceID,
				"span %q carries %q, where Tempo renders a bytes field as base64", span.name, span.traceID)

			decoded, err := base64.StdEncoding.DecodeString(span.traceID)
			require.NoError(t, err, "span %q carries %q, which is neither hex nor base64", span.name, span.traceID)
			assert.Len(t, decoded, traceIDBytes)
			assert.Equal(t, padded, hex.EncodeToString(decoded),
				"and it is the trace that was asked for, at all %d bytes: the search answer was short, the trace never is", traceIDBytes)

			spanID, err := base64.StdEncoding.DecodeString(span.spanID)
			require.NoError(t, err, "span %q carries span ID %q", span.name, span.spanID)
			assert.Len(t, spanID, spanIDBytes)
		}
	})

	t.Run("a declared encoding of hex is inert here, not a corruption", func(t *testing.T) {
		right, err := otlp.Decode(detail.body, otlp.Options{RootKey: otlp.RootKeyBatches, IDEncoding: otlp.Base64})
		require.NoError(t, err, "the shipped OTLP decoder could not read a live Tempo trace detail")
		ids := decodedIDs(right)
		require.NotEmpty(t, ids)
		for _, span := range ids {
			assert.Equal(t, padded, span.traceID,
				"the decoder reports the ID as hex at full width, which is the form the browser compares")
		}

		// The asymmetry, recorded rather than assumed: only the
		// hex-read-as-base64 direction is caught. A base64 ID carries '=' and
		// may carry '+' or '/', none of which hex accepts, so the rewrite
		// declines the value and protojson reads it correctly. A rewrite made
		// more permissive than that would corrupt this body instead, and this
		// is the assertion that says so.
		inert, err := otlp.Decode(detail.body, otlp.Options{RootKey: otlp.RootKeyBatches, IDEncoding: otlp.Hex})
		require.NoError(t, err,
			"declaring hex over a base64 body is a no-op, so this direction of the trap has no width to be caught by")
		assert.Equal(t, ids, decodedIDs(inert),
			"and it must stay a no-op: a rewrite that touched these IDs would resolve every span to a different trace with no error")

		// The root key is the other declaration, and it is the one that does
		// report a Tempo body read as an api_v3 one.
		_, err = otlp.Decode(detail.body, otlp.Options{RootKey: otlp.RootKeyResult, IDEncoding: otlp.Base64})
		require.Error(t, err, "a Tempo body is not an api_v3 body, and the envelope is what says so")
		assert.Contains(t, err.Error(), otlp.RootKeyResult, "%v", err)
	})

	t.Run("a stripped trace ID and its padded form name one trace", func(t *testing.T) {
		short := get(t, addr, tempoTracePath+stripped[0], nil)
		require.Equal(t, http.StatusOK, short.status,
			"Tempo resolves the short form it answered its own search with: %s", short)
		assert.Equal(t, tempoRawIDs(t, detail), tempoRawIDs(t, short),
			"%s and %s must answer one trace, span for span, which is what makes Kiali's padding cosmetic", padded, stripped[0])
	})
}

// grpcTraces runs an api_v3 gRPC search through the shipped stream reader and
// reports what it recovered, so the widths below are the ones Kiali sees.
func grpcTraces(t *testing.T, client apiv3.QueryServiceClient, query *apiv3.TraceQueryParameters) *tracepb.TracesData {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.FindTraces(ctx, &apiv3.FindTracesRequest{Query: query})
	require.NoError(t, err, "the api_v3 gRPC search was refused")
	traces, err := otlp.ReadStream(ctx, stream)
	require.NoError(t, err, "otlp.ReadStream failed on a live api_v3 stream")
	return traces
}

// apiV3RawIDs decodes an api_v3 body down to the ID text of every span, sorted,
// so two answers are comparable whatever order the store listed them in.
func apiV3RawIDs(t *testing.T, got answer) []rawIDs {
	t.Helper()
	var envelope struct {
		Result struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []rawSpan `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(got.body, &envelope),
		"the body is not readable as an api_v3 trace answer: %s", got)

	var ids []rawIDs
	for _, resource := range envelope.Result.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				ids = append(ids, span.ids())
			}
		}
	}
	return sortIDFields(ids)
}

// tempoRawIDs does the same for Tempo's trace detail, under its own root key.
func tempoRawIDs(t *testing.T, got answer) []rawIDs {
	t.Helper()
	var envelope struct {
		Batches []struct {
			ScopeSpans []struct {
				Spans []rawSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"batches"`
	}
	require.NoError(t, json.Unmarshal(got.body, &envelope),
		"the body is not readable as a Tempo trace detail: %s", got)

	var ids []rawIDs
	for _, batch := range envelope.Batches {
		for _, scope := range batch.ScopeSpans {
			for _, span := range scope.Spans {
				ids = append(ids, span.ids())
			}
		}
	}
	return sortIDFields(ids)
}

// rawSpan is the part of an OTLP/JSON span these assertions read.
type rawSpan struct {
	Name         string `json:"name"`
	ParentSpanID string `json:"parentSpanId"`
	SpanID       string `json:"spanId"`
	TraceID      string `json:"traceId"`
}

func (s rawSpan) ids() rawIDs {
	return rawIDs{name: s.Name, parentSpanID: s.ParentSpanID, spanID: s.SpanID, traceID: s.TraceID}
}

// decodedIDs reports the IDs of a decoded payload as hex, which is the one
// spelling both encodings can be compared at.
func decodedIDs(traces *tracepb.TracesData) []rawIDs {
	var ids []rawIDs
	for _, resource := range traces.GetResourceSpans() {
		for _, scope := range resource.GetScopeSpans() {
			for _, span := range scope.GetSpans() {
				ids = append(ids, rawIDs{
					name:         span.GetName(),
					parentSpanID: hex.EncodeToString(span.GetParentSpanId()),
					spanID:       hex.EncodeToString(span.GetSpanId()),
					traceID:      hex.EncodeToString(span.GetTraceId()),
				})
			}
		}
	}
	return sortIDFields(ids)
}

func sortIDFields(ids []rawIDs) []rawIDs {
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].traceID != ids[j].traceID {
			return ids[i].traceID < ids[j].traceID
		}
		return ids[i].spanID < ids[j].spanID
	})
	return ids
}

// leadingZeroTraceID reports a trace ID of the window that begins with a zero,
// which is the only ID whose rendered width says anything about padding.
func leadingZeroTraceID(spans []rawIDs) string {
	for _, span := range spans {
		if strings.HasPrefix(span.traceID, "0") {
			return span.traceID
		}
	}
	return ""
}

func requireLeadingZeroTraceID(t *testing.T, spans []rawIDs) string {
	t.Helper()
	id := leadingZeroTraceID(spans)
	require.NotEmpty(t, id,
		"no trace ID in this window begins with a zero, and about one in sixteen does: drive more traffic through the lab")
	return id
}
