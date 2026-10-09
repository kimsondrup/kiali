package tempo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/model"
	"github.com/kiali/kiali/tracing/jaeger/model/json"
)

const (
	responseFile        = "../tracingtest/response.json"
	responseAmbientFile = "../tracingtest/responseAmbient.json"
	responseTrace       = "../tracingtest/responseTrace.json"
	responseMultiScope  = "../tracingtest/responseTraceMultiScope.json"
	responseTypedAttrs  = "../tracingtest/responseTraceTypedAttrs.json"
	responseTypedSearch = "../tracingtest/responseTypedSearch.json"
	responseTypedErrors = "../tracingtest/responseTypedErrors.json"
	tracingUrl          = "http://tracing.tempo"
	serviceName         = "productpage.bookinfo"
	ambientServiceName  = "waypoint.bookinfo"
)

type RoundTripFunc func(req *http.Request) *http.Response

func (f RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func getBaseUrl() *url.URL {
	baseUrl, _ := url.Parse(tracingUrl)
	return baseUrl
}

func TestGetTraces(t *testing.T) {
	baseUrl := getBaseUrl()

	resp, err := os.Open(responseFile)
	assert.Nil(t, err)
	defer resp.Close()

	byteValue, _ := io.ReadAll(resp)

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(byteValue))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)
	assert.NotNil(t, tempoClient)

	q := models.TracingQuery{
		Start:       time.Time{},
		End:         time.Time{},
		Tags:        nil,
		MinDuration: 0,
		Limit:       0,
		Cluster:     "",
	}
	response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, serviceName, q)
	assert.Nil(t, err)
	assert.NotNil(t, response)
	assert.Equal(t, response.TracingServiceName, serviceName)
	assert.Nil(t, response.Errors)
	assert.NotNil(t, response.Data)
	assert.Equal(t, response.Data[0].TraceID, json.TraceID("100cb753c787ed5657c8d88dafc176ed"))
	assert.Equal(t, len(response.Data[0].Spans), 3)
	assert.Equal(t, response.Data[0].Spans[0].OperationName, "productpage.bookinfo.svc.cluster.local:9080/productpage")
	assert.Equal(t, response.Data[0].Spans[0].Tags[0].Key, "node_id")
	assert.Equal(t, response.Data[0].Spans[0].Tags[0].Value, "sidecar~10.244.0.20~reviews-v1-667b5cc65d-4m24g.bookinfo~bookinfo.svc.cluster.local")
}

func TestGetAmbientTraces(t *testing.T) {
	baseUrl := getBaseUrl()

	resp, err := os.Open(responseAmbientFile)
	assert.Nil(t, err)
	defer resp.Close()

	byteValue, _ := io.ReadAll(resp)

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(byteValue))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)
	assert.NotNil(t, tempoClient)

	q := models.TracingQuery{
		Start:       time.Time{},
		End:         time.Time{},
		Tags:        nil,
		MinDuration: 0,
		Limit:       0,
		Cluster:     "",
	}
	response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, ambientServiceName, q)
	assert.Nil(t, err)
	assert.NotNil(t, response)
	assert.Equal(t, response.TracingServiceName, ambientServiceName)
	assert.Nil(t, response.Errors)
	assert.NotNil(t, response.Data)
	// Tempo answered this search with a 31 character trace ID, having stripped its leading zero.
	// Kiali reports the full width, so that the search answer and the trace detail name the same
	// trace.
	assert.Equal(t, response.Data[0].TraceID, json.TraceID("02e299711ce47710289dc6640727404f"))
	assert.Equal(t, len(response.Data[0].Spans), 4)
	assert.Equal(t, response.Data[0].Spans[1].OperationName, "reviews.bookinfo.svc.cluster.local:9080/*")
	assert.Equal(t, response.Data[0].Spans[1].Tags[2].Key, "node_id")
	assert.Equal(t, response.Data[0].Spans[1].Tags[2].Value, "waypoint~10.244.0.28~waypoint-5b7c754ccb-55jwk.bookinfo~bookinfo.svc.cluster.local")
}

func TestGetTrace(t *testing.T) {
	baseUrl := getBaseUrl()

	resp, err := os.Open(responseTrace)
	assert.Nil(t, err)
	defer resp.Close()

	byteValue, _ := io.ReadAll(resp)

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(byteValue))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)
	assert.NotNil(t, tempoClient)

	response, err := tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, baseUrl, "3ba55609c3cde49649cd77d1f9dcd936")
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, response.Data.TraceID, json.TraceID("3ba55609c3cde49649cd77d1f9dcd936"))
	assert.Nil(t, response.Errors)
	assert.NotNil(t, response.Data)
	assert.Equal(t, len(response.Data.Spans), 8)
	assert.Equal(t, response.Data.Matched, 8)

	// The IDs reach the browser in hex, which is what the trace ID route and the Jaeger-UI deep
	// links accept. Before the detail path decoded OTLP they arrived as the base64 the body
	// carries, "Sc130fnc2TY=" for this span.
	assert.Equal(t, json.SpanID("49cd77d1f9dcd936"), response.Data.Spans[0].SpanID)
	assert.Equal(t, json.SpanID("49cd77d1f9dcd936"), response.Data.Spans[1].References[0].SpanID)

	// Every span resolves in the process table, which is how the business layer selects the spans
	// of a workload. The table has one entry per distinct resource, not one per batch: two of the
	// six batches describe productpage.
	require.Len(t, response.Data.Processes, 5)
	services := map[string]int{}
	for _, span := range response.Data.Spans {
		process, found := response.Data.Processes[span.ProcessID]
		require.True(t, found, "span [%s] refers to process [%s], which the table does not hold", span.SpanID, span.ProcessID)
		require.NotNil(t, span.Process)
		assert.Equal(t, process.ServiceName, span.Process.ServiceName)
		services[process.ServiceName]++
	}
	assert.Equal(t, map[string]int{
		"istio-ingressgateway.istio-system": 1,
		"productpage.bookinfo":              3,
		"details.bookinfo":                  1,
		"reviews.bookinfo":                  2,
		"ratings.bookinfo":                  1,
	}, services)
}

// TestGetTraceEveryScope covers a resource carrying more than one instrumentation scope, which the
// waypoint and an application SDK reporting the same trace produce routinely. Reading the first
// scope only drops the rest of the spans with no error: here it would lose the otelmux span, which
// is also the parent of the one it keeps.
func TestGetTraceEveryScope(t *testing.T) {
	response, err := getTraceFromFile(t, responseMultiScope, "3fed459787ea5c51070a19c4e6cd3040")
	require.NoError(t, err)
	require.NotNil(t, response)

	require.Len(t, response.Data.Spans, 3)
	names := []string{}
	for _, span := range response.Data.Spans {
		names = append(names, span.OperationName)
	}
	assert.Contains(t, names, "GET /delay/{wait:[0-9]+}")

	// The parent of the first application span is the one the second scope carries, so the chain
	// only resolves when both scopes are read.
	byID := map[json.SpanID]json.Span{}
	for _, span := range response.Data.Spans {
		byID[span.SpanID] = span
	}
	parent := byID["3e1860bba5a4ac3d"].References[0].SpanID
	assert.Equal(t, json.SpanID("e5c939e83dcbc699"), parent)
	assert.Contains(t, byID, parent)
}

// TestGetTraceTypedAttributes covers the attribute types OTLP carries. A port, a payload size and a
// status code are intValue, and reading every attribute as a string reported them as empty.
func TestGetTraceTypedAttributes(t *testing.T) {
	response, err := getTraceFromFile(t, responseTypedAttrs, "4ce7f3617b6b969cad880718687d652d")
	require.NoError(t, err)
	require.NotNil(t, response)

	tags := map[string]json.KeyValue{}
	for _, span := range response.Data.Spans {
		if span.SpanID == "bd37d7ff12415f45" {
			for _, tag := range span.Tags {
				tags[tag.Key] = tag
			}
		}
	}
	require.NotEmpty(t, tags)

	for key, expected := range map[string]int64{
		"http.response.status_code": 200,
		"http.response.body.size":   80,
		"server.port":               9898,
		"network.peer.port":         60861,
	} {
		require.Contains(t, tags, key)
		assert.Equal(t, json.Int64Type, tags[key].Type, key)
		assert.Equal(t, expected, tags[key].Value, key)
	}
	assert.Equal(t, json.StringType, tags["http.request.method"].Type)
	assert.Equal(t, "GET", tags["http.request.method"].Value)
}

// TestOddTraceDetailBody covers the detail bodies that reach the zero-index reads the Tempo detail
// path used to make: every one of these panicked.
//
// The split is between a body with no keys at all, which is a backend saying the trace holds no
// spans, and a body of some other shape, which is a backend Kiali could not read. The first is
// answered with no trace and no error, which handlers/tracing.go turns into a 404 naming the
// trace; the second is reported.
func TestOddTraceDetailBody(t *testing.T) {
	cases := map[string]struct {
		body          string
		expectedError bool
	}{
		"an object with no keys at all":               {body: `{}`},
		"a trace with no batch at all":                {body: `{"batches":[]}`},
		"a batch with no scope spans":                 {body: `{"batches":[{"resource":{"attributes":[]}}]}`},
		"a scope with no span":                        {body: `{"batches":[{"scopeSpans":[{"scope":{}}]}]}`},
		"a search answer sent as detail":              {body: `{"traces":[]}`, expectedError: true},
		"an HTML error page from a proxy":             {body: `<html><body>502 Bad Gateway</body></html>`, expectedError: true},
		"a body that stops mid-way":                   {body: `{"batches":[{"scopeSpans":`, expectedError: true},
		"a Jaeger body from a misconfigured provider": {body: `{"data":[{"traceID":"3ba55609c3cde49649cd77d1f9dcd936","spans":[]}]}`, expectedError: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			response, err := getTraceFromBody(t, tc.body, "3ba55609c3cde49649cd77d1f9dcd936")
			if tc.expectedError {
				require.Error(t, err, "a body Kiali cannot read must not reach the user as a trace")
				assert.Nil(t, response)
				return
			}
			// A body with no span is a trace Tempo does not have, which the handler reports as a
			// 404 rather than as an empty trace page.
			require.NoError(t, err)
			assert.Nil(t, response)
		})
	}
}

func getTraceFromFile(t *testing.T, file string, traceID string) (*model.TracingSingleTrace, error) {
	t.Helper()

	body, err := os.ReadFile(file)
	require.NoError(t, err)
	return getTraceFromBody(t, string(body), traceID)
}

func getTraceFromBody(t *testing.T, body string, traceID string) (*model.TracingSingleTrace, error) {
	t.Helper()

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	require.NoError(t, err)

	return tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, getBaseUrl(), traceID)
}

// TestGetTracesErrorsOnly covers the Errors only filter on the search path, in the shape Tempo
// 3.1.0 answers in. Tempo writes no span-level status there: asked for one with select(status),
// as prepareTraceQL does, it answers with an attribute keyed "status" whose value is the TraceQL
// status intrinsic as text. Measured over a live Tempo, its three values are "error", "ok" and
// "unset".
//
// The fourth trace's span carries no status attribute at all. That one is defensive rather than
// measured: every span of every captured search response in ../tracingtest carries the
// attribute, and a span that arrives without it has to read as a span that did not fail.
// TestGetTypedSearchAttributes covers the attributes of a span Tempo's search API matched, which
// are OTLP attributes embedded in a document that is not OTLP. Kiali used to declare the value as
// one field, stringValue, so every int, bool, double, array and kvlist reached the UI with a key
// and no value.
//
// Measured, Envoy writes every attribute as a stringValue - 3471 of 3471 on a mesh whose apps
// export nothing - so this matters for a mesh with instrumented applications and changes nothing
// on an Istio-only one. The fixture carries the typed forms a mesh like that sends.
func TestGetTypedSearchAttributes(t *testing.T) {
	baseUrl := getBaseUrl()

	body, err := os.ReadFile(responseTypedSearch)
	require.NoError(t, err)

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(body))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	require.NoError(t, err)

	response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, "typeprobe.devex", models.TracingQuery{})
	require.NoError(t, err)
	require.Len(t, response.Data, 1)
	require.Len(t, response.Data[0].Spans, 2)

	// getServiceName reads the matched span's service.name through the same bridge, and it is the
	// span's process that carries the answer: TracingServiceName is overwritten by the caller
	// with the service that was queried, so it cannot report whether the bridge worked.
	require.NotNil(t, response.Data[0].Spans[0].Process)
	assert.Equal(t, "typeprobe.devex", response.Data[0].Spans[0].Process.ServiceName)

	tags := map[string]json.KeyValue{}
	for _, tag := range response.Data[0].Spans[0].Tags {
		tags[tag.Key] = tag
	}
	assert.Equal(t, json.KeyValue{Key: "component", Value: int64(7), Type: json.Int64Type}, tags["component"])
	assert.Equal(t, json.KeyValue{Key: "response_flags", Value: true, Type: json.BoolType}, tags["response_flags"])
	assert.Equal(t, json.KeyValue{Key: "http.method", Value: "GET", Type: json.StringType}, tags["http.method"])
	// the "status" attribute is the TraceQL intrinsic, folded into the tag Kiali's frontend reads
	assert.Equal(t, json.KeyValue{Key: "error", Value: true, Type: json.BoolType}, tags["error"])
	assert.NotContains(t, tags, "status")

	// the second span did not fail, so it carries the intrinsic as its own tag and no error tag
	ok := map[string]json.KeyValue{}
	for _, tag := range response.Data[0].Spans[1].Tags {
		ok[tag.Key] = tag
	}
	assert.Equal(t, json.KeyValue{Key: "status", Value: "ok", Type: json.StringType}, ok["status"])
	assert.NotContains(t, ok, "error")
}

// TestGetTracesErrorsOnlyFromFixture covers the Errors only filter over a captured search answer
// rather than an inline body, so the shape the filter reads is the shape a backend sent.
func TestGetTracesErrorsOnlyFromFixture(t *testing.T) {
	baseUrl := getBaseUrl()

	body, err := os.ReadFile(responseTypedErrors)
	require.NoError(t, err)

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(body))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	require.NoError(t, err)

	failed, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, "errorprobe.devex",
		models.TracingQuery{Tags: map[string]string{"error": "true"}})
	require.NoError(t, err)
	require.Len(t, failed.Data, 1)
	// Tempo stripped the leading zeros of this trace ID down to 29 characters
	assert.Equal(t, json.TraceID("000e0e0e0e0e0e0e0e0e0e0e0e0e0e01"), failed.Data[0].TraceID)
	require.Len(t, failed.Data[0].Spans, 2)
	assert.Contains(t, failed.Data[0].Spans[0].Tags,
		json.KeyValue{Key: "error", Value: true, Type: json.BoolType})
}

func TestGetTracesErrorsOnly(t *testing.T) {
	baseUrl := getBaseUrl()

	span := func(id, attributes string) string {
		return `{"spanID":"` + id + `","startTimeUnixNano":"1701779876570888000","durationNanos":"1000",` +
			`"attributes":[{"key":"service.name","value":{"stringValue":"orders-api"}}` + attributes + `]}`
	}
	status := func(value string) string {
		return `,{"key":"status","value":{"stringValue":"` + value + `"}}`
	}
	body := `{"traces":[` +
		`{"traceID":"aa00000000000000000000000000000a","spanSet":{"spans":[` + span("0101010101010101", status("unset")) + `]}},` +
		`{"traceID":"bb00000000000000000000000000000b","spanSet":{"spans":[` + span("0202020202020202", status("error")) + `]}},` +
		`{"traceID":"cc00000000000000000000000000000c","spanSet":{"spans":[` + span("0303030303030303", status("ok")) + `]}},` +
		`{"traceID":"dd00000000000000000000000000000d","spanSet":{"spans":[` + span("0404040404040404", "") + `]}}` +
		`]}`

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	require.NoError(t, err)

	all, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, serviceName, models.TracingQuery{})
	require.NoError(t, err)
	assert.Equal(t, 4, len(all.Data))

	failed, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, serviceName,
		models.TracingQuery{Tags: map[string]string{"error": "true"}})
	require.NoError(t, err)
	require.Equal(t, 1, len(failed.Data))
	assert.Equal(t, json.TraceID("bb00000000000000000000000000000b"), failed.Data[0].TraceID)
	// the errored span reports the boolean tag Kiali's frontend reads, not the attribute
	require.Equal(t, 1, len(failed.Data[0].Spans))
	assert.Contains(t, failed.Data[0].Spans[0].Tags,
		json.KeyValue{Key: "error", Value: true, Type: json.BoolType})
}

// TestUnreadableSearchBody covers a 200 whose body is not a Tempo search answer, so a wrong
// endpoint gives a meaningful error instead of an empty trace list.
func TestUnreadableSearchBody(t *testing.T) {
	bodies := map[string]string{
		"an object with no keys at all":      `{}`,
		"an error Tempo reports as a 200":    `{"error":"no org id","status":"error"}`,
		"a trace detail sent to the search":  `{"batches":[]}`,
		"a body wrapped in something else":   `{"result":{"traces":[]}}`,
		"an HTML error page from a proxy":    `<html><body>502 Bad Gateway</body></html>`,
		"a list where an object is expected": `[]`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
				}
			})}

			tempoClient, err := NewOtelClient(context.TODO())
			require.NoError(t, err)

			response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, getBaseUrl(), serviceName, models.TracingQuery{})
			require.Error(t, err, "a body Kiali cannot read must not reach the user as an empty result")
			assert.Empty(t, response.Data)
		})
	}
}

// TestEmptySearch verifies that a valid search answer with no traces reports no traces.
func TestEmptySearch(t *testing.T) {
	bodies := map[string]string{
		"no traces and no metrics to report": `{"traces":[],"metrics":{}}`,
		"metrics only":                       `{"metrics":{"inspectedTraces":42,"inspectedBytes":"1024"}}`,
		"an empty traces list only":          `{"traces":[]}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
				}
			})}

			tempoClient, err := NewOtelClient(context.TODO())
			require.NoError(t, err)

			response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, getBaseUrl(), serviceName, models.TracingQuery{})
			require.NoError(t, err)
			assert.Empty(t, response.Data)
		})
	}
}

func TestErrorResponse(t *testing.T) {
	baseUrl := getBaseUrl()

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(`invalid TraceQL query: parse error at line 1, col 99: syntax error: unexpected IDENTIFIER`)),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)
	assert.NotNil(t, tempoClient)

	q := models.TracingQuery{
		Start:       time.Time{},
		End:         time.Time{},
		Tags:        nil,
		MinDuration: 0,
		Limit:       0,
		Cluster:     "",
	}
	response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, serviceName, q)
	assert.NotNil(t, err)
	assert.NotNil(t, response)
	assert.Nil(t, response.Data)
	assert.Equal(t, response.TracingServiceName, serviceName)
}

// Test prepare query method
func TestQuery(t *testing.T) {
	baseUrl := getBaseUrl()

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)
	assert.NotNil(t, tempoClient)

	q := models.TracingQuery{
		Start:       time.Time{},
		End:         time.Time{},
		Tags:        nil,
		MinDuration: 0,
		Limit:       0,
		Cluster:     "",
	}
	query := tempoClient.GetTraceQLQuery(context.Background(), baseUrl, serviceName, q)
	assert.NotNil(t, query)
	rawQuery, err := url.QueryUnescape(query)
	assert.Nil(t, err)
	assert.Contains(t, rawQuery, fmt.Sprintf(".service.name = \"%s\"", serviceName))
	// Verify it contains all the selects
	assert.Contains(t, rawQuery, "select(status, .service_name, .node_id, .component, .upstream_cluster, .http.method, .response_flags, .istio.destination_workload, .istio.destination_namespace, .istio.destination_instance_name, .istio.destination_canonical_service, .istio.source_workload, .istio.source_namespace, .istio.source_instance_name, .istio.source_canonical_service, resource.hostname, name)")
	// Verify it doesn't contain the cluster tag
	assert.NotContains(t, rawQuery, models.IstioClusterTag)
	// Verify it contains spans limit
	assert.Contains(t, rawQuery, "spss=10")
	// Verify it contains start
	assert.Contains(t, rawQuery, "start=")
	// Verify it contains end
	assert.Contains(t, rawQuery, "end=")

	// Test tags
	q2 := models.TracingQuery{
		Start: time.Time{},
		End:   time.Time{},
		Tags: map[string]string{
			"istio.mesh_id":    "mesh_hack",
			"istio.cluster_id": "east",
			"custom":           "value",
		},
		MinDuration: 0,
		Limit:       0,
		Cluster:     "",
	}
	query2 := tempoClient.GetTraceQLQuery(context.Background(), baseUrl, serviceName, q2)
	assert.NotNil(t, query2)
	rawQuery2, err2 := url.QueryUnescape(query2)
	assert.Nil(t, err2)
	assert.Contains(t, rawQuery2, fmt.Sprintf(".service.name = \"%s\"", serviceName))
	assert.Contains(t, rawQuery2, ".istio.mesh_id = \"mesh_hack\"")
	assert.Contains(t, rawQuery2, ".custom = \"value\"")
	// Should contain Cluster tag
	assert.Contains(t, rawQuery2, ".istio.cluster_id = \"east\"")

	query3 := tempoClient.GetTraceQLQuery(context.Background(), baseUrl, serviceName, q2)
	assert.NotNil(t, query3)
	rawQuery3, err3 := url.QueryUnescape(query3)
	assert.Nil(t, err3)
	assert.Contains(t, rawQuery3, fmt.Sprintf(".service.name = \"%s\"", serviceName))
	assert.Contains(t, rawQuery3, ".istio.mesh_id = \"mesh_hack\"")
	assert.Contains(t, rawQuery3, ".custom = \"value\"")
	assert.Contains(t, rawQuery3, ".istio.cluster_id = \"east\"")
}
