package tracingcontract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger"
	"github.com/kiali/kiali/tracing/otel/otlp"
)

// errorSpanAttribute is the flag Envoy sets on a failed span. Kiali's own error
// query is Tags{"error":"true"}, so this is the attribute GetErrorTraces narrows
// by.
const errorSpanAttribute = "error"

// statusCodeError is the OTLP Status_STATUS_CODE_ERROR enum as api_v3's HTTP
// gateway puts it on the wire: the number, never the name.
const statusCodeError = 2

// apiV3Span is one span of an api_v3 HTTP search answer, decoded as far as the
// signals Kiali reads and no further.
type apiV3Span struct {
	Attributes []struct {
		Key   string `json:"key"`
		Value struct {
			BoolValue   *bool   `json:"boolValue"`
			IntValue    *string `json:"intValue"`
			StringValue *string `json:"stringValue"`
		} `json:"value"`
	} `json:"attributes"`
	Name string `json:"name"`
	// Code is an int, and that is the assertion rather than a convenience: the
	// gateway renders the OTLP status enum as its number, so a reader expecting
	// STATUS_CODE_ERROR sees no error at all, and a backend that started
	// rendering the name would fail to decode here.
	Status struct {
		Code int `json:"code"`
	} `json:"status"`
	TraceID string `json:"traceId"`
}

// liveSpan carries the resource service.name with the span, because the two
// error signals are not emitted by the same resource: the error attribute is
// Envoy's alone, where a status code arrives from Envoy and from an
// application's own SDK alike.
type liveSpan struct {
	service string
	span    apiV3Span
}

func (s liveSpan) attribute(key string) (string, bool) {
	for _, attribute := range s.span.Attributes {
		if attribute.Key != key {
			continue
		}
		if attribute.Value.StringValue != nil {
			return *attribute.Value.StringValue, true
		}
		return "", true
	}
	return "", false
}

// liveSpans decodes every span of a search answer, with its resource service name.
func liveSpans(t *testing.T, a answer) []liveSpan {
	t.Helper()
	var envelope struct {
		Result struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []apiV3Span `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"result"`
	}
	if err := json.Unmarshal(a.body, &envelope); err != nil {
		t.Fatalf("api_v3 body is not readable as OTLP spans: %v (%s)", err, a)
	}
	var spans []liveSpan
	for _, resource := range envelope.Result.ResourceSpans {
		name := ""
		for _, attribute := range resource.Resource.Attributes {
			if attribute.Key == "service.name" {
				name = attribute.Value.StringValue
			}
		}
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				spans = append(spans, liveSpan{service: name, span: span})
			}
		}
	}
	return spans
}

// tracesCarrying reports the distinct traces holding at least one span with the
// attribute, which is the grain Kiali's tag filter works at: a trace is kept
// when any of its spans matches.
func tracesCarrying(spans []liveSpan, key, value string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, span := range spans {
		if got, present := span.attribute(key); !present || got != value {
			continue
		}
		if id := span.span.TraceID; id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// TestErrorSignalEnvoyEmits pins what a failing request leaves in the store, and
// so what Kiali's error query can narrow by.
//
// The premise is worth stating because it is easy to get wrong from an all-200
// sample, where every assertion below reads as "there are no errors": the
// signals only exist in a window holding failures, and the seed precondition is
// what makes that window real.
func TestErrorSignalEnvoyEmits(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			baseline := search(t, store, url.Values{"query.num_traces": {seedDepth}})
			require.Equal(t, http.StatusOK, baseline.status, "%s", baseline)

			spans := liveSpans(t, baseline)
			require.NotEmpty(t, spans, "the seeded window holds no span, so no row below means anything")

			allTraces := baseline.traceIDs(t)
			errorTraces := tracesCarrying(spans, errorSpanAttribute, "true")
			require.NotEmpty(t, errorTraces,
				"no span in the window carries %s=true: drive failing traffic through the lab, because every row here is about what a failure leaves behind",
				errorSpanAttribute)
			require.Less(t, len(errorTraces), len(allTraces),
				"every one of the %d seeded traces carries %s=true, so a filter on it cannot be shown to narrow: drive succeeding traffic too",
				len(allTraces), errorSpanAttribute)

			t.Run("the error flag is a string attribute, not a boolean one", func(t *testing.T) {
				// Envoy writes the OTLP attribute as a string. A reader taking
				// boolValue finds the key present and the value absent, which is
				// a span that silently is not an error.
				var checked int
				for _, span := range spans {
					for _, attribute := range span.span.Attributes {
						if attribute.Key != errorSpanAttribute {
							continue
						}
						checked++
						require.NotNil(t, attribute.Value.StringValue,
							"span %q carries %s with no stringValue: %+v", span.span.Name, errorSpanAttribute, attribute.Value)
						assert.Equal(t, "true", *attribute.Value.StringValue,
							"the only %s value Kiali's query matches is the string true", errorSpanAttribute)
						assert.Nil(t, attribute.Value.BoolValue,
							"%s must not arrive as a boolean, because Kiali's tag filter compares strings", errorSpanAttribute)
					}
				}
				require.NotZero(t, checked)
			})

			t.Run("a span status is the enum number, never its name", func(t *testing.T) {
				// This is the trap, and it is silent in both directions: a
				// comparison against the name finds no error, and a backend that
				// switched to the name would make the int above fail to decode
				// rather than quietly read as zero.
				assert.NotContains(t, string(baseline.body), "STATUS_CODE",
					"api_v3 renders the status enum as a number, so the name appearing here means the wire form changed")

				var withStatus, errorStatus int
				for _, span := range spans {
					if span.span.Status.Code == 0 {
						continue
					}
					withStatus++
					if span.span.Status.Code == statusCodeError {
						errorStatus++
					}
				}
				require.NotZero(t, withStatus,
					"no span in the window carries a status at all, so the number-versus-name question cannot be decided here")
				assert.Equal(t, withStatus, errorStatus,
					"every set status in this window is an error, so any other code means the sample changed")

				// The shipped decoder reads that number. Without this the row
				// pins the backend alone, and Kiali could still be the reader
				// that compares against the name.
				decoded, err := otlp.Decode(baseline.body, otlp.Options{RootKey: otlp.RootKeyResult, IDEncoding: otlp.Hex})
				require.NoError(t, err, "the shipped OTLP decoder could not read a live api_v3 body")
				var decodedErrors int
				for _, resource := range decoded.GetResourceSpans() {
					for _, scope := range resource.GetScopeSpans() {
						for _, span := range scope.GetSpans() {
							if span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR {
								decodedErrors++
							}
						}
					}
				}
				assert.Equal(t, errorStatus, decodedErrors,
					"the decoder must recover all %d error statuses from the numeric form", errorStatus)
			})

			t.Run("only Envoy's spans carry the cluster id Kiali filters by", func(t *testing.T) {
				// Kiali appends the cluster id to every trace query, so this is
				// not a property of the data but the reason an application's own
				// spans never reach a Kiali trace list, however well their
				// service name matches.
				var envoy, other, taggedOther int
				for _, span := range spans {
					_, tagged := span.attribute(models.IstioClusterTag)
					if span.service == service {
						envoy++
						assert.True(t, tagged,
							"span %q of %s carries no %s, so Kiali's own query would exclude it", span.span.Name, span.service, models.IstioClusterTag)
						continue
					}
					other++
					if tagged {
						taggedOther++
					}
				}
				require.NotZero(t, envoy)
				if other == 0 {
					// A lab whose only spans are a waypoint's has nothing to contrast them
					// with. That is a property of the seed, not a contract change, so it
					// skips rather than fails.
					t.Skipf("the window holds spans from %s alone: no application exports its own spans here, so this row cannot show the tag is selective", service)
				}
				assert.Zero(t, taggedOther,
					"%d spans outside %s carry %s: the tag is what makes an application's own spans invisible to Kiali, and this says it no longer does",
					taggedOther, service, models.IstioClusterTag)
			})

			t.Run("http narrows to exactly the failing traces where it narrows at all", func(t *testing.T) {
				narrowed := search(t, store, url.Values{
					"query.num_traces": {seedDepth},
					"query.attributes": {jsonMap(t, map[string]string{errorSpanAttribute: "true"})},
				})
				require.Equal(t, http.StatusOK, narrowed.status,
					"%s=true is a working api_v3 attribute, not a value no span carries: %s", errorSpanAttribute, narrowed)

				if store.caps.httpAttributeFilter {
					assert.Equal(t, errorTraces, narrowed.traceIDs(t),
						"the backend must answer exactly the %d traces whose spans carry %s=true", len(errorTraces), errorSpanAttribute)
					return
				}

				// The consequence, not just the inertness: with the filter
				// unread, every trace in the window is reported as an error
				// trace, which is a count a user reads as a broken mesh.
				assert.Equal(t, allTraces, narrowed.traceIDs(t),
					"Jaeger %s ignores query.attributes, so an unfiltered answer of all %d traces is what Kiali must narrow itself",
					store.version, len(allTraces))
			})

			t.Run("Kiali's HTTP transport narrows to exactly the failing traces on every version", func(t *testing.T) {
				// The shipped client re-applies the tags to what it converted, so a gateway
				// that ignores query.attributes (2.17.0) and one that honours it answer
				// Kiali alike. The depth is the suite's, above the seeded trace count,
				// because on 2.17.0 the depth applies before Kiali's filter does.
				response, err := jaeger.NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{},
					&url.URL{Scheme: "http", Host: store.addr}, service, shippedErrorQuery())
				require.NoError(t, err)
				assert.Equal(t, errorTraces, responseTraceIDs(response),
					"Kiali must report exactly the %d traces whose spans carry %s=true", len(errorTraces), errorSpanAttribute)
			})

			t.Run("Kiali's HTTP transport at GetErrorTraces' own depth", func(t *testing.T) {
				// GetErrorTraces sends no limit, which is a depth of 100, in one request. A
				// gateway that filters answers every failed trace up to that depth. On one that
				// ignores query.attributes the depth picks the newest 100 traces before Kiali's
				// filter runs, so the count is the failed traces among those: fewer than the
				// window holds when its newest 100 traces include successes. That undercount is
				// the documented price of not searching deeper.
				q := shippedErrorQuery()
				q.Limit = 0
				response, err := jaeger.NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{},
					&url.URL{Scheme: "http", Host: store.addr}, service, q)
				require.NoError(t, err)

				if store.caps.httpAttributeFilter {
					assert.Len(t, response.Data, min(len(errorTraces), 100),
						"Kiali must count min(%d, 100) failed traces at the default depth", len(errorTraces))
					assert.Subset(t, errorTraces, responseTraceIDs(response))
					return
				}

				newest := search(t, store, url.Values{"query.num_traces": {"100"}})
				require.Equal(t, http.StatusOK, newest.status, "%s", newest)
				var want []string
				for _, id := range newest.traceIDs(t) {
					if slices.Contains(errorTraces, id) {
						want = append(want, id)
					}
				}
				assert.ElementsMatch(t, want, responseTraceIDs(response),
					"Jaeger %s ignores query.attributes, so Kiali must count exactly the failed traces among the newest 100", store.version)
			})

			t.Run("grpc narrows to exactly the failing traces on every version", func(t *testing.T) {
				client, covered := grpcPeer(t, store.version)
				if !covered {
					t.Skipf("Jaeger %s is configured for HTTP only", store.version)
				}
				ids, _, err := findTraces(t, client, searchWithAttributes(map[string]string{errorSpanAttribute: "true"}))
				require.NoError(t, err)
				assert.Equal(t, errorTraces, ids,
					"over gRPC %s=true is honoured on every version, so the answer must be exactly the %d failing traces",
					errorSpanAttribute, len(errorTraces))

				response, err := jaeger.NewGRPCClient(client, time.Minute).FindTraces(context.Background(), service, shippedErrorQuery())
				require.NoError(t, err)
				assert.Equal(t, errorTraces, responseTraceIDs(response),
					"and Kiali's gRPC transport, which does not filter again, must report the same traces")
			})
		})
	}
}

// shippedErrorQuery is the query GetErrorTraces builds, over the suite's window and at the
// suite's depth.
func shippedErrorQuery() models.TracingQuery {
	depth, _ := strconv.Atoi(seedDepth)
	return models.TracingQuery{
		End:   windowMax,
		Limit: depth,
		Start: windowMin,
		Tags:  map[string]string{errorSpanAttribute: "true"},
	}
}

// responseTraceIDs reports the sorted trace IDs of a converted answer.
func responseTraceIDs(response *models.TracingResponse) []string {
	ids := []string{}
	if response == nil {
		return ids
	}
	for _, trace := range response.Data {
		ids = append(ids, string(trace.TraceID))
	}
	sort.Strings(ids)
	return ids
}
