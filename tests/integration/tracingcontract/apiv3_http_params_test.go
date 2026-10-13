package tracingcontract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/models"
)

// TestAPIV3HTTPParameterSpelling pins the one parameter spelling every supported
// Jaeger reads.
//
// api_v3's HTTP gateway is generated from the proto, so each field has two
// names: the proto field name and its lowerCamelCase JSON name. Which of the two
// the gateway accepts changed between Jaeger versions, and a rejected parameter
// is not reported as unknown — it is simply absent, which turns a narrow query
// into a wide one or into a complaint about a missing required field.
//
// Every row therefore asserts the version's own answer rather than a lowest
// common denominator, because the version that diverges, 2.17.0, is the one
// Istio 1.31's add-on ships and CI lands on.
func TestAPIV3HTTPParameterSpelling(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			// snake_case with query.num_traces is the only spelling that works
			// on every supported version, and the yardstick the rest compare to.
			baseline := search(t, store, url.Values{"query.num_traces": {seedDepth}})
			require.Equal(t, http.StatusOK, baseline.status,
				"Jaeger %s refused the one spelling every supported version reads: %s", store.version, baseline)
			baselineIDs := baseline.traceIDs(t)
			require.NotEmpty(t, baselineIDs, "the seeded window is empty, so no row below means anything")

			t.Run("camelCase window is absent, not rejected", func(t *testing.T) {
				got := get(t, store.addr, searchPath, url.Values{
					"query.serviceName":  {service},
					"query.startTimeMin": {formatTime(windowMin)},
					"query.startTimeMax": {formatTime(windowMax)},
					"query.searchDepth":  {seedDepth},
				})
				if store.caps.camelCaseSearchParams {
					assert.Equal(t, http.StatusOK, got.status, "camelCase is read here: %s", got)
					return
				}
				// The gateway does not read the camelCase names at all, so the
				// window is missing and it says which fields it wanted.
				assert.Equal(t, http.StatusBadRequest, got.status, "camelCase must be refused here: %s", got)
				assert.Contains(t, string(got.body), "query.start_time_min",
					"the refusal must name the snake_case window parameters: %s", got)
				assert.Contains(t, string(got.body), "query.start_time_max",
					"the refusal must name the snake_case window parameters: %s", got)
			})

			t.Run("query.search_depth is not the depth parameter", func(t *testing.T) {
				got := search(t, store, url.Values{"query.search_depth": {seedDepth}})
				if store.caps.searchDepthParam {
					assert.Equal(t, http.StatusOK, got.status, "query.search_depth is read here: %s", got)
					return
				}
				// The depth is unread, so the search carries none, which api_v3
				// refuses. Same answer as omitting it, which is what makes this
				// spelling indistinguishable from forgetting the depth.
				assert.Equal(t, http.StatusInternalServerError, got.status,
					"query.search_depth must leave the search with no depth here: %s", got)
				assert.Contains(t, string(got.body), "search depth must be greater than 0",
					"the refusal must be about the depth: %s", got)
			})

			t.Run("a search with no depth", func(t *testing.T) {
				got := search(t, store, nil)
				if store.caps.depthOptionalHTTP {
					// Accepted, and unbounded: the whole window comes back. So a
					// defaulted positive depth bounds the answer here rather
					// than only avoiding the refusal below.
					require.Equal(t, http.StatusOK, got.status, "a depthless search is accepted here: %s", got)
					assert.NotEmpty(t, got.traceIDs(t), "an accepted depthless search must still answer traces: %s", got)
					return
				}
				assert.Equal(t, http.StatusInternalServerError, got.status,
					"api_v3 requires a positive depth here, so Kiali must default one: %s", got)
				assert.Contains(t, string(got.body), "search depth must be greater than 0", "%s", got)
			})

			t.Run("query.num_traces=0 is refused everywhere", func(t *testing.T) {
				got := search(t, store, url.Values{"query.num_traces": {"0"}})
				assert.Equal(t, http.StatusInternalServerError, got.status,
					"a zero depth must be refused, which is why Kiali may never send one: %s", got)
			})

			t.Run("a depth above the ceiling is refused, never clamped", func(t *testing.T) {
				above := strconv.Itoa(models.MaxTracingLimit + 1)
				got := search(t, store, url.Values{"query.num_traces": {above}})
				if store.caps.depthHardCeiling > 0 {
					require.Equal(t, models.MaxTracingLimit, store.caps.depthHardCeiling,
						"Jaeger %s validates the depth itself, so Kiali's clamp has to be that ceiling", store.version)
					assert.Equal(t, http.StatusBadRequest, got.status,
						"a depth past the version's own ceiling is a parameter refusal, which is what makes the clamp necessary: %s", got)
					assert.Contains(t, string(got.body), "query.num_traces",
						"the refusal must name the parameter it refused: %s", got)
					return
				}
				// No ceiling of the version's own, so the depth is bounded by
				// the store instead. Whether that bound is reached depends on
				// how the store is sized, and what holds either way is that it
				// is never a parameter refusal: Kiali cannot read a 400 here as
				// "too large" and retry smaller.
				assert.NotEqual(t, http.StatusBadRequest, got.status,
					"Jaeger %s has no depth ceiling of its own, so a depth of %s must not be a parameter refusal: %s", store.version, above, got)
				if got.status != http.StatusOK {
					assert.Contains(t, string(got.body), "less than max traces",
						"a depth the store cannot serve must say so: %s", got)
				}
			})

			t.Run("camelCase durationMin narrows nothing", func(t *testing.T) {
				// The quieter half of the same defect: an unread minimum
				// duration does not fail, it widens. A row asserting only the
				// status passes on the bug, so both rows assert the narrowing.
				minDuration := splittingDuration(t, baseline)
				camel := search(t, store, url.Values{"query.num_traces": {seedDepth}, "query.durationMin": {minDuration}})
				require.Equal(t, http.StatusOK, camel.status, "%s", camel)

				snake := search(t, store, url.Values{"query.num_traces": {seedDepth}, "query.duration_min": {minDuration}})
				require.Equal(t, http.StatusOK, snake.status, "%s", snake)
				assert.Less(t, len(snake.traceIDs(t)), len(baselineIDs),
					"query.duration_min must narrow the %d seeded traces, or this row proves nothing: %s", len(baselineIDs), snake)

				if store.caps.camelCaseDurationMin {
					assert.Equal(t, len(snake.traceIDs(t)), len(camel.traceIDs(t)),
						"both spellings narrow here: %s", camel)
					return
				}
				assert.True(t, bytes.Equal(baseline.body, camel.body),
					"query.durationMin must be inert here, answering the unfiltered body byte for byte: %s", camel)
			})
		})
	}
}

// splittingDuration picks a minimum duration that keeps some of the answer's traces and
// drops others, read off the answer rather than written down because how long a span
// takes is the lab's and not the contract's. It is the midpoint of the widest gap between
// the traces' longest spans, rounded to microseconds, so neither neighbour sits on the
// threshold whatever precision the store compares at.
func splittingDuration(t *testing.T, a answer) string {
	t.Helper()
	var envelope struct {
		Result struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						EndTimeUnixNano   string `json:"endTimeUnixNano"`
						StartTimeUnixNano string `json:"startTimeUnixNano"`
						TraceID           string `json:"traceId"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(a.body, &envelope), "%s", a)

	longest := map[string]int64{}
	for _, rs := range envelope.Result.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				start, err := strconv.ParseInt(span.StartTimeUnixNano, 10, 64)
				require.NoError(t, err)
				end, err := strconv.ParseInt(span.EndTimeUnixNano, 10, 64)
				require.NoError(t, err)
				if end-start > longest[span.TraceID] {
					longest[span.TraceID] = end - start
				}
			}
		}
	}
	durations := make([]int64, 0, len(longest))
	for _, d := range longest {
		durations = append(durations, d)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	gap, at := int64(0), 0
	for i := 1; i < len(durations); i++ {
		if durations[i]-durations[i-1] > gap {
			gap, at = durations[i]-durations[i-1], i
		}
	}
	if gap < 2*int64(time.Microsecond) {
		t.Skipf("the %d traces of the window last too nearly alike for a minimum duration to split them", len(durations))
	}
	// Whole microseconds with the ASCII unit, which every version's Go duration parser
	// reads; Duration.String would spell sub-millisecond values with "µs".
	threshold := time.Duration(durations[at-1] + gap/2).Round(time.Microsecond)
	return strconv.FormatInt(threshold.Microseconds(), 10) + "us"
}
