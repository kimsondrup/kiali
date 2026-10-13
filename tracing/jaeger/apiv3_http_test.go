package jaeger

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/models"
	jsonModel "github.com/kiali/kiali/models/tracing/json"
)

// Captured live from Jaeger 2.17.0, the version Istio 1.31's addon ships. The search holds four
// single-span traces of waypoint.tracegen, two of them failed (error="true"). The detail is the
// first failed one. tracing/tracingtest/README.adoc records how.
const (
	apiv3NotFoundHTTP    = "../tracingtest/apiv3_notfound_http.json"
	apiv3SearchHTTP217   = "../tracingtest/apiv3_search_http_217.json"
	apiv3ServicesHTTP217 = "../tracingtest/apiv3_services_http_217.json"
	apiv3TraceHTTP217    = "../tracingtest/apiv3_trace_http_217.json"
)

// The traces of apiv3_search_http_217.json, and which of them failed.
var (
	errorTraces217 = []string{"2ea535bec9fe41eb90bd2ae18c259e2d", "b7705fb4f9e547db3e51444a957eba03"}
	allTraces217   = []string{"2ea535bec9fe41eb90bd2ae18c259e2d", "a5eb2a617857e9836da580613578ffcf", "a95d19ab0fe7dd5f4f1ab0698588b558", "b7705fb4f9e547db3e51444a957eba03"}
)

// The router's own answer to a path it has no route for: a 404 with no api_v3 envelope.
const routerNotFoundBody = "404 page not found\n"

// recorded is what the fake backend saw: the last request, and the query of every request.
type recorded struct {
	mu       sync.Mutex
	path     string
	queries  []url.Values
	query    url.Values
	requests int
}

func (r *recorded) snapshot() (string, url.Values, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path, r.query, r.requests
}

func (r *recorded) allQueries() []url.Values {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.queries)
}

// fakeJaeger answers every request with the status and body given, labelled text/plain the way
// Jaeger 2.17.0 labels both its answers and its errors, and records what it was asked.
func fakeJaeger(t *testing.T, status int, body []byte) (*url.URL, *recorded) {
	t.Helper()
	return fakeJaegerFunc(t, func(url.Values) (int, []byte) { return status, body })
}

// fakeJaegerFunc is fakeJaeger answering each request with what answer returns for its query.
func fakeJaegerFunc(t *testing.T, answer func(query url.Values) (int, []byte)) (*url.URL, *recorded) {
	t.Helper()
	rec := &recorded{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.path = r.URL.Path
		rec.query = r.URL.Query()
		rec.queries = append(rec.queries, r.URL.Query())
		rec.requests++
		rec.mu.Unlock()
		status, body := answer(r.URL.Query())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	return u, rec
}

func readFixture(t *testing.T, file string) []byte {
	t.Helper()
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	return body
}

// A window that holds the 2.17.0 search fixture's traces. The fake backend does not read it; it
// only has to pass the request builder's guards.
func searchQuery() models.TracingQuery {
	return models.TracingQuery{
		End:   time.Date(2026, 10, 13, 0, 56, 17, 60_000_000, time.UTC),
		Start: time.Date(2026, 10, 13, 0, 56, 17, 0, time.UTC),
	}
}

func traceIDs(traces []jsonModel.Trace) []string {
	ids := make([]string, 0, len(traces))
	for _, trace := range traces {
		ids = append(ids, string(trace.TraceID))
	}
	slices.Sort(ids)
	return ids
}

func withPath(t *testing.T, u *url.URL, p string) *url.URL {
	t.Helper()
	c := *u
	c.Path = p
	return &c
}

func TestHTTPSearchURL(t *testing.T) {
	q := searchQuery()
	q.Tags = map[string]string{"istio.cluster_id": "Kubernetes", "error": "true"}
	q.MinDuration = 1500 * time.Microsecond

	for basePath, want := range map[string]string{
		"":         "/api/v3/traces",
		"/":        "/api/v3/traces",
		"/jaeger":  "/jaeger/api/v3/traces",
		"/jaeger/": "/jaeger/api/v3/traces",
	} {
		t.Run("base path "+basePath, func(t *testing.T) {
			base, rec := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3SearchHTTP217))
			_, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, withPath(t, base, basePath), "waypoint.tracegen", q)
			require.NoError(t, err)

			gotPath, _, _ := rec.snapshot()
			assert.Equal(t, want, gotPath)
			// Exactly these, every one snake_case: 2.17.0 refuses or ignores any other spelling.
			queries := rec.allQueries()
			require.Len(t, queries, 1)
			query := queries[0]
			assert.Equal(t, url.Values{
				"query.attributes":     {`{"error":"true","istio.cluster_id":"Kubernetes"}`},
				"query.duration_min":   {"0.001500000s"},
				"query.num_traces":     {"100"},
				"query.service_name":   {"waypoint.tracegen"},
				"query.start_time_max": {"2026-10-13T00:56:17.06Z"},
				"query.start_time_min": {"2026-10-13T00:56:17Z"},
			}, query)
		})
	}
}

// GetErrorTraces asks for no limit, and 2.17.0 answers a 0 or missing depth with a 500.
func TestHTTPSearchSendsAPositiveDepth(t *testing.T) {
	for limit, want := range map[int]string{0: "100", -1: "100", 5: "5", 20000: "10000"} {
		base, rec := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3SearchHTTP217))
		q := searchQuery()
		q.Limit = limit
		_, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "waypoint.tracegen", q)
		require.NoError(t, err)
		_, query, _ := rec.snapshot()
		assert.Equal(t, want, query.Get("query.num_traces"), "limit %d", limit)
		assert.NotContains(t, query, "query.search_depth")
	}
}

func TestHTTPSearchReadsCapturedAnswers(t *testing.T) {
	cases := map[string]struct {
		file    string
		service string
		traces  int
	}{
		"Jaeger 2.17.0": {file: apiv3SearchHTTP217, service: "waypoint.tracegen", traces: 4},
		"Jaeger 2.21.0": {file: apiv3SearchHTTP, service: "waypoint.tp-otlp-sf2ed7", traces: 3},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			base, _ := fakeJaeger(t, http.StatusOK, readFixture(t, tc.file))
			response, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, tc.service, searchQuery())
			require.NoError(t, err)
			require.NotNil(t, response)

			assert.Equal(t, tc.service, response.TracingServiceName)
			require.Len(t, response.Data, tc.traces)
			for _, trace := range response.Data {
				assert.Len(t, string(trace.TraceID), 32)
				require.NotEmpty(t, trace.Spans)
				for _, span := range trace.Spans {
					// Hex at full width, as the gateway renders it, and not hex misread as base64.
					assert.Regexp(t, "^[0-9a-f]{16}$", string(span.SpanID))
					assert.Equal(t, trace.TraceID, span.TraceID)
				}
			}
		})
	}
}

func TestHTTPSearchOfNothing(t *testing.T) {
	base, _ := fakeJaeger(t, http.StatusNotFound, readFixture(t, apiv3NotFoundHTTP))
	response, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "waypoint.tracegen", searchQuery())
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.NotNil(t, response.Data)
	assert.Empty(t, response.Data)
	assert.Equal(t, "waypoint.tracegen", response.TracingServiceName)
}

func TestHTTPTraceDetail(t *testing.T) {
	base, rec := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3TraceHTTP217))
	trace, err := NewHTTPClient().GetTraceDetailHTTP(context.Background(), http.Client{}, withPath(t, base, "/jaeger"), "b7705fb4f9e547db3e51444a957eba03")
	require.NoError(t, err)
	require.NotNil(t, trace)

	gotPath, query, _ := rec.snapshot()
	assert.Equal(t, "/jaeger/api/v3/traces/b7705fb4f9e547db3e51444a957eba03", gotPath)
	assert.Empty(t, query)
	assert.Equal(t, jsonModel.TraceID("b7705fb4f9e547db3e51444a957eba03"), trace.Data.TraceID)
	require.Len(t, trace.Data.Spans, 1)
	assert.Equal(t, jsonModel.SpanID("5f445603284287e9"), trace.Data.Spans[0].SpanID)
}

// An unknown trace is (nil, nil) whether the backend says so with the 404 envelope or with a 200
// carrying no span, so the handler answers 404 on both.
func TestHTTPTraceDetailOfAnUnknownTrace(t *testing.T) {
	for name, answer := range map[string]struct {
		body   []byte
		status int
	}{
		"404 envelope":      {body: readFixture(t, apiv3NotFoundHTTP), status: http.StatusNotFound},
		"200 with no spans": {body: []byte(`{"result":{}}`), status: http.StatusOK},
		"200 empty object":  {body: []byte(`{}`), status: http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			base, _ := fakeJaeger(t, answer.status, answer.body)
			trace, err := NewHTTPClient().GetTraceDetailHTTP(context.Background(), http.Client{}, base, "0123456789abcdef0123456789abcdef")
			require.NoError(t, err)
			assert.Nil(t, trace)
		})
	}
}

// Answers that are not api_v3 must say so on every call, rather than read as an empty store.
func TestHTTPNotAPIv3(t *testing.T) {
	for name, answer := range map[string]struct {
		body   string
		status int
	}{
		// A version before 1.54, or a mistyped route.
		"router 404": {body: routerNotFoundBody, status: http.StatusNotFound},
		// The Jaeger UI, answered for any path under a wrong base path.
		"200 HTML": {body: "<!doctype html><html><head><title>Jaeger UI</title></head></html>", status: http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			base, _ := fakeJaeger(t, answer.status, []byte(answer.body))
			client := NewHTTPClient()
			ctx := context.Background()

			_, searchErr := client.GetAppTracesHTTP(ctx, http.Client{}, base, "waypoint.tracegen", searchQuery())
			_, detailErr := client.GetTraceDetailHTTP(ctx, http.Client{}, base, "b7705fb4f9e547db3e51444a957eba03")
			_, servicesErr := client.GetServices(ctx, http.Client{}, base)
			ok, statusErr := client.GetServiceStatusHTTP(ctx, http.Client{}, base)
			assert.False(t, ok)

			for call, err := range map[string]error{"search": searchErr, "detail": detailErr, "services": servicesErr, "status": statusErr} {
				require.Error(t, err, call)
				assert.Contains(t, err.Error(), "api_v3", call)
				assert.Contains(t, err.Error(), "1.54", call)
				assert.Contains(t, err.Error(), base.Host, call)
			}
		})
	}
}

func TestHTTPBackendFailureCarriesItsMessage(t *testing.T) {
	body := []byte(`{"error":{"httpCode":500,"message":"search depth must be greater than 0 and less than max traces"}}`)
	base, _ := fakeJaeger(t, http.StatusInternalServerError, body)

	_, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "waypoint.tracegen", searchQuery())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "search depth must be greater than 0")

	_, err = NewHTTPClient().GetTraceDetailHTTP(context.Background(), http.Client{}, base, "b7705fb4f9e547db3e51444a957eba03")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "search depth must be greater than 0")
}

func TestHTTPServices(t *testing.T) {
	base, rec := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3ServicesHTTP217))
	services, err := NewHTTPClient().GetServices(context.Background(), http.Client{}, withPath(t, base, "/jaeger"))
	require.NoError(t, err)
	assert.Equal(t, []string{"waypoint.tracegen", "jaeger"}, services)

	gotPath, query, _ := rec.snapshot()
	assert.Equal(t, "/jaeger/api/v3/services", gotPath)
	assert.Empty(t, query)

	ok, err := NewHTTPClient().GetServiceStatusHTTP(context.Background(), http.Client{}, base)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestDecodeServices(t *testing.T) {
	services, err := DecodeServices([]byte(`{"services":["a","b"]}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, services)

	// protojson leaves out an empty repeated field.
	for _, empty := range []string{`{}`, `{"services":[]}`, `{"services":null}`} {
		services, err = DecodeServices([]byte(empty))
		require.NoError(t, err, empty)
		assert.NotNil(t, services, empty)
		assert.Empty(t, services, empty)
	}

	_, err = DecodeServices([]byte(`{"data":["a"],"total":1,"limit":0,"offset":0,"errors":null}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"data"`)
	assert.Contains(t, err.Error(), "/api/services")

	_, err = DecodeServices([]byte(`{"names":["a"],"other":1}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names, other")

	for _, bad := range []string{"<html></html>", "", "null", `["a"]`, `{"services":"a"}`} {
		_, err = DecodeServices([]byte(bad))
		assert.Error(t, err, bad)
	}
}

// GetServices reports a body without the services key as an endpoint that is not api_v3.
func TestHTTPServicesOfTheLegacyShape(t *testing.T) {
	base, _ := fakeJaeger(t, http.StatusOK, []byte(`{"data":["a"],"total":1,"limit":0,"offset":0,"errors":null}`))
	_, err := NewHTTPClient().GetServices(context.Background(), http.Client{}, base)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_v3")
	assert.Contains(t, err.Error(), "1.54")
	assert.Contains(t, err.Error(), `"data"`)
}

// HTTP sent to a gRPC port reads the server's HTTP/2 SETTINGS frame as a status line. The fake
// server writes the frame Jaeger's 16685 answers with.
func TestHTTPToAGRPCPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("\x00\x00\x06\x04\x00\x00\x00\x00\x00\x00\x05\x00\x00\x40\x00"))
			conn.Close()
		}
	}()

	base := &url.URL{Scheme: "http", Host: listener.Addr().String()}
	ok, err := NewHTTPClient().GetServiceStatusHTTP(context.Background(), http.Client{}, base)
	assert.False(t, ok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrong port or transport")
	assert.Contains(t, err.Error(), "malformed HTTP response")
}

// A dial error keeps its own text.
func TestHTTPConnectionRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	listener.Close()

	_, err = NewHTTPClient().GetServices(context.Background(), http.Client{}, &url.URL{Scheme: "http", Host: addr})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	assert.NotContains(t, err.Error(), "wrong port or transport")
}

// 2.17.0's gateway ignores query.attributes, so it answers all four traces to a search for the
// failed ones; the transport keeps only those.
func TestHTTPSearchFiltersByTag(t *testing.T) {
	cases := map[string]struct {
		tags map[string]string
		want []string
	}{
		"no tags":                {tags: nil, want: allTraces217},
		"error on the span":      {tags: map[string]string{"error": "true"}, want: errorTraces217},
		"cluster on the span":    {tags: map[string]string{"istio.cluster_id": "Kubernetes"}, want: allTraces217},
		"a cluster no span has":  {tags: map[string]string{"istio.cluster_id": "cluster1"}, want: []string{}},
		"tag only on process":    {tags: map[string]string{"telemetry.sdk.name": "envoy"}, want: allTraces217},
		"span and process":       {tags: map[string]string{"error": "true", "telemetry.sdk.name": "envoy"}, want: errorTraces217},
		"one of the two missing": {tags: map[string]string{"error": "true", "telemetry.sdk.name": "otel"}, want: []string{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			base, _ := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3SearchHTTP217))
			q := searchQuery()
			q.Tags = tc.tags
			response, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "waypoint.tracegen", q)
			require.NoError(t, err)
			require.NotNil(t, response.Data)
			assert.Equal(t, tc.want, traceIDs(response.Data))
			assert.Equal(t, "waypoint.tracegen", response.TracingServiceName)
		})
	}
}

// Values are compared as text, so a typed boolean from an SDK matches the filter Envoy's string
// "true" does, and a span that carries the key with another value does not.
func TestKeepTracesWithTagsComparesText(t *testing.T) {
	traces := []jsonModel.Trace{
		{
			Processes: map[jsonModel.ProcessID]jsonModel.Process{"p1": {}},
			Spans:     []jsonModel.Span{{ProcessID: "p1", Tags: []jsonModel.KeyValue{{Key: "error", Value: true}}}},
			TraceID:   "typed",
		},
		{
			Processes: map[jsonModel.ProcessID]jsonModel.Process{"p1": {}},
			Spans:     []jsonModel.Span{{ProcessID: "p1", Tags: []jsonModel.KeyValue{{Key: "error", Value: "false"}}}},
			TraceID:   "false",
		},
		{
			Processes: map[jsonModel.ProcessID]jsonModel.Process{"p1": {Tags: []jsonModel.KeyValue{{Key: "error", Value: "true"}}}},
			Spans:     []jsonModel.Span{{ProcessID: "p1"}},
			TraceID:   "process",
		},
		{
			// Every requested tag has to be on one span: here they are spread over two.
			Processes: map[jsonModel.ProcessID]jsonModel.Process{"p1": {}},
			Spans: []jsonModel.Span{
				{ProcessID: "p1", Tags: []jsonModel.KeyValue{{Key: "error", Value: "true"}}},
				{ProcessID: "p1", Tags: []jsonModel.KeyValue{{Key: "code", Value: int64(503)}}},
			},
			TraceID: "spread",
		},
	}

	assert.Equal(t, []string{"process", "spread", "typed"}, traceIDs(keepTracesWithTags(traces, map[string]string{"error": "true"})))
	assert.Equal(t, []string{"spread"}, traceIDs(keepTracesWithTags(traces, map[string]string{"code": "503"})))
	assert.Empty(t, traceIDs(keepTracesWithTags(traces, map[string]string{"error": "true", "code": "503"})))
	assert.NotNil(t, keepTracesWithTags(nil, map[string]string{"error": "true"}))

	// A key 2.20 and later read as a prefixed name is left to the server, which matched it.
	assert.Len(t, keepTracesWithTags(traces, map[string]string{"resource.k8s.namespace.name": "x", "scope.name": "x", "span.status": "x"}), len(traces))
	assert.Equal(t, []string{"spread"}, traceIDs(keepTracesWithTags(traces, map[string]string{"code": "503", "resource.k8s.namespace.name": "x"})))
}

// A request the builder refuses never reaches the backend, and neither does a window nothing can
// match, which is answered as an empty list.
func TestHTTPGuardsSendNothing(t *testing.T) {
	inverted := searchQuery()
	inverted.Start, inverted.End = inverted.End, inverted.Start

	for name, q := range map[string]models.TracingQuery{
		"inverted window": inverted,
		"equal window":    {End: inverted.End, Start: inverted.End},
		"zero window":     {},
	} {
		t.Run(name, func(t *testing.T) {
			base, rec := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3SearchHTTP217))
			response, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "waypoint.tracegen", q)
			require.NoError(t, err)
			require.NotNil(t, response.Data)
			assert.Empty(t, response.Data)
			assert.Equal(t, "waypoint.tracegen", response.TracingServiceName)
			_, _, requests := rec.snapshot()
			assert.Zero(t, requests)
		})
	}

	t.Run("no service", func(t *testing.T) {
		base, rec := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3SearchHTTP217))
		_, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "", searchQuery())
		require.Error(t, err)
		_, _, requests := rec.snapshot()
		assert.Zero(t, requests)
	})

	base, rec := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3TraceHTTP217))
	_, err := NewHTTPClient().GetTraceDetailHTTP(context.Background(), http.Client{}, base, "../services")
	require.Error(t, err)
	_, _, requests := rec.snapshot()
	assert.Zero(t, requests)
}

// A tagged search is one request at the depth asked for, carrying the tags as query.attributes
// so that a gateway that reads them filters on the server, and the answer is filtered again here,
// with no second request, whether or not the gateway read them.
func TestHTTPTaggedSearchSendsOneRequest(t *testing.T) {
	base, rec := fakeJaeger(t, http.StatusOK, readFixture(t, apiv3SearchHTTP217))
	q := searchQuery()
	q.Limit = len(allTraces217)
	q.Tags = map[string]string{"error": "true"}
	response, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "waypoint.tracegen", q)
	require.NoError(t, err)

	assert.Equal(t, errorTraces217, traceIDs(response.Data))
	assert.Empty(t, response.Errors)
	queries := rec.allQueries()
	require.Len(t, queries, 1)
	assert.Equal(t, `{"error":"true"}`, queries[0].Get("query.attributes"))
	assert.Equal(t, "4", queries[0].Get("query.num_traces"))
}

// On a gateway that ignores query.attributes, as 2.17.0's does, the depth picks the newest traces
// before Kiali's filter runs, so a tag search finds only the matches among those: here 10 of the
// 60 failed traces at the default depth of 100, and none when every failed trace is older than
// the newest 100. That is the documented limit of one request; gRPC and 2.20 and later answer
// the newest matches up to the depth.
func TestHTTPSearchOnAGatewayThatIgnoresAttributesUndercounts(t *testing.T) {
	cases := map[string]struct {
		failed func(rank int) bool
		want   int
	}{
		"matches straddle the depth": {failed: func(rank int) bool { return rank >= 90 && rank < 150 }, want: 10},
		"every match is older":       {failed: func(rank int) bool { return rank >= 100 }, want: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := syntheticStore(300, tc.failed)
			base, rec := fakeJaegerFunc(t, store.answer)
			q := searchQuery()
			q.Tags = map[string]string{"error": "true"}
			response, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "waypoint.tracegen", q)
			require.NoError(t, err)

			require.NotNil(t, response.Data)
			assert.Len(t, response.Data, tc.want)
			assert.Empty(t, response.Errors)
			queries := rec.allQueries()
			require.Len(t, queries, 1)
			assert.Equal(t, "100", queries[0].Get("query.num_traces"))
		})
	}
}

// A gateway that reads query.attributes (2.20.0 and later) answers only the traces it matched,
// and Kiali's second pass keeps them all, also for a key such as resource.k8s.namespace.name that
// the server matches and the converted trace does not carry under that name.
func TestHTTPSearchKeepsWhatAGatewayThatFiltersAnswered(t *testing.T) {
	cases := map[string]map[string]string{
		"a span attribute":     {"error": "true"},
		"a resource attribute": {"resource.k8s.namespace.name": "tracegen"},
		"both":                 {"error": "true", "resource.k8s.namespace.name": "tracegen"},
	}
	for name, tags := range cases {
		t.Run(name, func(t *testing.T) {
			// A store whose every trace failed stands for what a gateway that filters answers.
			store := syntheticStore(40, func(rank int) bool { return true })
			base, rec := fakeJaegerFunc(t, store.answer)
			q := searchQuery()
			q.Tags = tags
			response, err := NewHTTPClient().GetAppTracesHTTP(context.Background(), http.Client{}, base, "waypoint.tracegen", q)
			require.NoError(t, err)

			assert.Len(t, response.Data, 40)
			assert.Len(t, rec.allQueries(), 1)
		})
	}
}

// synthetic is a store of single-span traces of waypoint.tracegen, rank 0 the newest, that
// answers a search with its newest query.num_traces traces whatever the attributes ask for, as
// 2.17.0's gateway does. Each rank's trace ID is its rank plus one, in hex.
type synthetic struct {
	failed func(rank int) bool
	total  int
}

func syntheticStore(total int, failed func(rank int) bool) synthetic {
	return synthetic{failed: failed, total: total}
}

func (s synthetic) traceID(rank int) string {
	return fmt.Sprintf("%032x", rank+1)
}

func (s synthetic) answer(query url.Values) (int, []byte) {
	depth, err := strconv.Atoi(query.Get("query.num_traces"))
	if err != nil || depth <= 0 {
		return http.StatusInternalServerError, []byte(`{"error":{"httpCode":500,"message":"search depth must be greater than 0"}}`)
	}
	newest := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	b.WriteString(`{"result":{"resourceSpans":[`)
	for rank := range min(depth, s.total) {
		if rank > 0 {
			b.WriteByte(',')
		}
		start := newest.Add(-time.Duration(rank) * time.Millisecond).UnixNano()
		errorAttribute := ""
		if s.failed(rank) {
			errorAttribute = `,{"key":"error","value":{"stringValue":"true"}}`
		}
		fmt.Fprintf(&b, `{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"waypoint.tracegen"}},{"key":"k8s.namespace.name","value":{"stringValue":"tracegen"}}]},"scopeSpans":[{"spans":[{"traceId":"%s","spanId":"%016x","name":"waypoint","startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":[{"key":"istio.cluster_id","value":{"stringValue":"Kubernetes"}}%s]}]}]}`,
			s.traceID(rank), rank+1, start, start+int64(time.Millisecond), errorAttribute)
	}
	b.WriteString(`]}}`)
	return http.StatusOK, []byte(b.String())
}
