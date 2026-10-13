package tests

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/models"
	jsonModel "github.com/kiali/kiali/models/tracing/json"
	"github.com/kiali/kiali/tests/integration/utils/kiali"
)

// tracingProviderEnv names the tracing provider this run installed Kiali with. Unset
// means the shipped default, jaeger, which is what every suite that runs these tests
// installs today.
const tracingProviderEnv = "TRACING_PROVIDER"

// configuredTracingProvider reports the provider under test.
//
// It comes from the run and not from GET /api/tracing, so that a test exercising a
// provider says which one it meant to exercise instead of trusting the server under
// test to report it.
func configuredTracingProvider() config.TracingProvider {
	if value := strings.TrimSpace(os.Getenv(tracingProviderEnv)); value != "" {
		return config.TracingProvider(value)
	}
	return config.JaegerProvider
}

func TestServiceTraces(t *testing.T) {
	require := require.New(t)
	name := "details"
	traces, statusCode, err := kiali.Traces("services", name, kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
}

func TestWorkloadTraces(t *testing.T) {
	require := require.New(t)
	name := "details-v1"
	traces, statusCode, err := kiali.Traces("workloads", name, kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
}

func TestAppTraces(t *testing.T) {
	require := require.New(t)
	name := "details"
	traces, statusCode, err := kiali.Traces("apps", name, kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
}

// TestTraceDetail fetches the detail of a trace the search reported. Each provider builds the
// detail on a path of its own, separate from the search, so the search tests above say nothing
// about it: the IDs have to come back in hex at the width the search reported them, or the UI
// cannot match the detail to the row it was opened from, and the spans have to carry their
// parent references, or the trace renders as unrelated spans.
func TestTraceDetail(t *testing.T) {
	assertTraceDetail(t)
}

func assertTraceDetail(t *testing.T) {
	t.Helper()
	require := require.New(t)
	traces, statusCode, err := kiali.Traces("services", "details", kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
	traceID := string(traces.Data[0].TraceID)

	trace, statusCode, err := kiali.TraceDetail(traceID)
	require.NoError(err)
	require.Equal(200, statusCode)
	require.NotNil(trace)
	require.Regexp(`^[0-9a-f]{32}$`, string(trace.Data.TraceID))
	require.Equal(traceID, string(trace.Data.TraceID))
	require.NotEmpty(trace.Data.Spans)

	// The root span has no parent, so only some span has to carry a reference, and only
	// when the trace has more than one span.
	referenced := false
	for _, span := range trace.Data.Spans {
		require.Regexp(`^[0-9a-f]{16}$`, string(span.SpanID))
		referenced = referenced || len(span.References) > 0
	}
	if len(trace.Data.Spans) > 1 {
		require.True(referenced, "no span of trace %s references its parent", traceID)
	}
}

func TestWrongTracesType(t *testing.T) {
	require := require.New(t)
	name := "details"
	traces, statusCode, err := kiali.Traces("wrong", name, kiali.BOOKINFO)
	require.NotEqual(200, statusCode)
	require.NotNil(err)
	require.Empty(traces)
}

func TestWrongNamespaceTraces(t *testing.T) {
	require := require.New(t)
	name := "details"
	traces, _, _ := kiali.Traces("apps", name, "wrong")
	require.Empty(traces.Data)
	require.Empty(traces.Errors)
}

func TestServiceSpans(t *testing.T) {
	require := require.New(t)
	name := "details"
	spans, statusCode, err := kiali.Spans("services", name, kiali.BOOKINFO)
	assertSpans(spans, statusCode, err, configuredTracingProvider(), require)
}

func TestAppSpans(t *testing.T) {
	require := require.New(t)
	name := "details"
	spans, statusCode, err := kiali.Spans("apps", name, kiali.BOOKINFO)
	assertSpans(spans, statusCode, err, configuredTracingProvider(), require)
}

func TestWorkloadSpans(t *testing.T) {
	require := require.New(t)
	name := "details-v1"
	spans, statusCode, err := kiali.Spans("workloads", name, kiali.BOOKINFO)
	assertSpans(spans, statusCode, err, configuredTracingProvider(), require)
}

func TestWrongTypeSpans(t *testing.T) {
	require := require.New(t)
	name := "details-v1"
	spans, statusCode, err := kiali.Spans("wrong", name, kiali.BOOKINFO)
	require.NotEqual(200, statusCode)
	require.NotNil(err)
	require.Empty(spans)
}

func TestWrongNamespaceSpans(t *testing.T) {
	require := require.New(t)
	name := "details-v1"
	spans, _, _ := kiali.Spans("apps", name, "wrong")
	require.Empty(spans)
}

func assertTraces(traces *models.TracingResponse, statusCode int, err error, require *require.Assertions) {
	if statusCode == 200 {
		require.NoError(err)
		require.NotNil(traces)
		require.NotEmpty(traces.Data)
		require.NotNil(traces.Data[0].TraceID)
		require.NotEmpty(traces.Data[0].Spans)
		for _, span := range traces.Data[0].Spans {
			require.NotNil(span.TraceID)
			require.Equal(span.TraceID, traces.Data[0].TraceID)
		}
	} else {
		require.Fail(fmt.Sprintf("Status code should be '200' but was: %d and error: %s", statusCode, err))
	}
}

// assertSpans requires an answer with spans in it, and parent references wherever a
// trace has more than one span.
//
// A span's references come only from its parent span ID, so a root span carries none
// and may come first; what has to hold is that a trace of several spans is not a set of
// unrelated roots. Tempo is excluded because its search answer carries no parent span
// IDs at all, so the exclusion is the provider's and not any one transport's.
func assertSpans(spans []models.TracingSpan, statusCode int, err error, provider config.TracingProvider, require *require.Assertions) {
	require.Equal(200, statusCode, "the span query failed: %s", err)
	require.NoError(err)
	require.NotEmpty(spans,
		"the span query answered 200 and no span: the client reached a backend and could not read a span list out of it")

	referenced := map[jsonModel.TraceID]bool{}
	traceSize := map[jsonModel.TraceID]int{}
	for _, span := range spans {
		require.NotEmpty(span.TraceID)
		traceSize[span.TraceID] = span.TraceSize
		for _, ref := range span.References {
			require.Equal(span.TraceID, ref.TraceID, "span %s references trace %s", span.SpanID, ref.TraceID)
			referenced[span.TraceID] = true
		}
	}

	if provider == config.TempoProvider {
		return
	}
	for traceID, size := range traceSize {
		if size > 1 {
			require.True(referenced[traceID],
				"trace %s has %d spans and none of its spans in the answer references a parent", traceID, size)
		}
	}
}

// absentTagKey is a tag key nothing in a mesh emits, so a search carrying it can only
// answer empty.
//
// It is not istio.cluster_id with a value no span carries, which is the obvious choice
// and is unreachable: the query parser overwrites that key with the request's cluster
// after reading the caller's tags. The key holds no dot and no hyphen because it is also
// rendered into TraceQL for Tempo.
const absentTagKey = "kiali_integration_absent_tag"

// synthesisedTags are tags Kiali's converter adds from span fields rather than reading
// from the store's attributes, so a backend cannot be asked to filter on them.
var synthesisedTags = map[string]bool{
	"error":                   true,
	"otel.scope.name":         true,
	"otel.scope.version":      true,
	"otel.status_code":        true,
	"otel.status_description": true,
	"span.kind":               true,
	models.IstioClusterTag:    true,
}

// TestTraceTagFilter drives both halves of the tag filter against one app: a tag no
// span carries, and a tag taken from the answer's own spans.
//
// The pairing is what makes either half mean anything. A filter that is dropped answers
// 200 with the whole window, which reads exactly like a filter that matched everything,
// so only the excluding half fails on it -- and only the matching half stops the
// excluding half passing because the answer was empty for an unrelated reason.
func TestTraceTagFilter(t *testing.T) {
	name := "details"

	unfiltered, statusCode, err := kiali.Traces("apps", name, kiali.BOOKINFO)
	assertTraces(unfiltered, statusCode, err, require.New(t))

	t.Run("a tag no span carries excludes every trace", func(t *testing.T) {
		require := require.New(t)
		search := kiali.DefaultTraceSearch()
		search.Tags = tagsJSON(t, absentTagKey, "nothing-carries-this")

		traces, statusCode, err := kiali.TracesWithSearch("apps", name, kiali.BOOKINFO, search)
		require.NoError(err)
		require.Equal(200, statusCode)
		require.NotNil(traces)
		require.Empty(traces.Data,
			"the trace search carried %s and answered %d traces: the filter never reached the store, or nothing applied it",
			search.Tags, len(traces.Data))

		spans, statusCode, err := kiali.SpansWithSearch("apps", name, kiali.BOOKINFO, search)
		require.NoError(err)
		require.Equal(200, statusCode)
		require.Empty(spans, "the span search carried %s and answered %d spans", search.Tags, len(spans))
	})

	t.Run("a tag the answer's own spans carry excludes nothing", func(t *testing.T) {
		require := require.New(t)
		key, value := matchingTag(t, unfiltered)
		search := kiali.DefaultTraceSearch()
		search.Tags = tagsJSON(t, key, value)

		traces, statusCode, err := kiali.TracesWithSearch("apps", name, kiali.BOOKINFO, search)
		require.NoError(err)
		require.Equal(200, statusCode)
		require.NotEmpty(traces.Data,
			"the trace search carried %s, taken from a span of the unfiltered answer, and matched nothing", search.Tags)

		spans, statusCode, err := kiali.SpansWithSearch("apps", name, kiali.BOOKINFO, search)
		require.NoError(err)
		require.Equal(200, statusCode)
		require.NotEmpty(spans, "the span search carried %s and matched nothing", search.Tags)
	})
}

func tagsJSON(t *testing.T, key, value string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{key: value})
	require.NoError(t, err)
	return string(raw)
}

// matchingTag picks a tag out of an answer that a second search must match.
//
// It is read off the answer rather than written down because the key spelling and the
// value are the backend's. It has to share a span with istio.cluster_id, because Kiali
// adds that key to every query and a backend matches all of a query's tags against one
// span.
func matchingTag(t *testing.T, traces *models.TracingResponse) (string, string) {
	t.Helper()

	for _, trace := range traces.Data {
		for _, span := range trace.Spans {
			if !carriesKey(span, models.IstioClusterTag) {
				continue
			}
			keys := []string{}
			values := map[string]string{}
			for _, tag := range span.Tags {
				value, ok := tag.Value.(string)
				if !ok || value == "" || tag.Type != jsonModel.StringType || synthesisedTags[tag.Key] {
					continue
				}
				keys = append(keys, tag.Key)
				values[tag.Key] = value
			}
			if len(keys) == 0 {
				continue
			}
			sort.Strings(keys)
			return keys[0], values[keys[0]]
		}
	}

	t.Skipf("no span of the %d traces answered carries %s beside a string tag to narrow on, so there is no tag this backend is known to match",
		len(traces.Data), models.IstioClusterTag)
	return "", ""
}

func carriesKey(span jsonModel.Span, key string) bool {
	for _, tag := range span.Tags {
		if tag.Key == key {
			return true
		}
	}
	return false
}

// TestUnknownTraceDetail asks for a well-formed trace ID no store holds.
//
// The backends say "unknown trace" in different ways -- an HTTP 404 envelope, a gRPC
// NotFound, or a gRPC stream that ends with no message -- and every one of them has to
// reach the browser as the same 404, not as an empty 200 on one transport and a 503 on
// the other.
func TestUnknownTraceDetail(t *testing.T) {
	assertUnknownTraceDetail(t)
}

func assertUnknownTraceDetail(t *testing.T) {
	t.Helper()
	require := require.New(t)

	traceID := randomTraceID(t)
	_, statusCode, err := kiali.TraceDetail(traceID)
	require.Error(err)
	require.Equal(404, statusCode, "trace %s is in no store and the detail answered %d: %s", traceID, statusCode, err)
}

func randomTraceID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	return hex.EncodeToString(raw)
}

// TestAppErrorTraces drives GET /api/namespaces/{ns}/apps/{app}/errortraces.
//
// It is the only route that reaches a tracing backend with no limit of its own and the
// only one that synthesises the tag error=true, so neither request shape is reachable
// from any other test. A backend that refuses a search with no depth answers it with a
// 503 here and nowhere else.
//
// The count is bounded by the window's own trace list rather than compared across
// backends: a count above the whole window is an error tag that narrowed nothing.
func TestAppErrorTraces(t *testing.T) {
	assertErrorTraces(t, "details", 10*time.Minute)
}

func assertErrorTraces(t *testing.T, app string, window time.Duration) {
	t.Helper()
	require := require.New(t)

	errorTraces, statusCode, err := kiali.ErrorTraces(kiali.BOOKINFO, app, window)
	require.NoError(err, "the error-trace count failed")
	require.Equal(200, statusCode)

	traces, statusCode, err := kiali.Traces("apps", app, kiali.BOOKINFO)
	assertTraces(traces, statusCode, err, require)
	require.LessOrEqual(errorTraces, len(traces.Data),
		"%d traces are in error out of the %d the same window holds", errorTraces, len(traces.Data))
}

// TestTracingConfigCheckRefusesAnUnreachableStore drives POST /api/tracing/check with a
// store nothing listens on. It is the one answer that is the same whatever the run
// configured, so it can be asserted in every suite: the probe has to report a failure.
func TestTracingConfigCheckRefusesAnUnreachableStore(t *testing.T) {
	require := require.New(t)

	validation, statusCode, err := kiali.TracingConfigCheck(config.TracingConfig{
		Enabled:      true,
		InternalURL:  "http://127.0.0.1:59999",
		Provider:     config.JaegerProvider,
		QueryTimeout: 5,
	})
	require.NoError(err)
	require.Equal(200, statusCode)
	require.NotEmpty(validation.Error,
		"nothing listens on the posted URL and the check reported %q", validation.Message)
}

// TestTracingConfigCheckRefusesAnUnknownProvider pins that the check holds a posted
// provider to the list config load accepts, instead of probing with a client Kiali
// would refuse to start with.
func TestTracingConfigCheckRefusesAnUnknownProvider(t *testing.T) {
	require := require.New(t)

	_, statusCode, err := kiali.TracingConfigCheck(config.TracingConfig{
		Enabled:     true,
		InternalURL: "http://127.0.0.1:59999",
		Provider:    "zipkin",
	})
	require.Error(err)
	require.Equal(400, statusCode)
}
