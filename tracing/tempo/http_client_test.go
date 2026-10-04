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
	"github.com/kiali/kiali/tracing/jaeger/model/json"
)

const (
	responseFile        = "../tracingtest/response.json"
	responseAmbientFile = "../tracingtest/responseAmbient.json"
	responseTrace       = "../tracingtest/responseTrace.json"
	responseTypedSearch = "../tracingtest/responseTypedSearch.json"
	responseTypedTrace  = "../tracingtest/responseTypedTrace.json"
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
	assert.Equal(t, response.Data[0].TraceID, json.TraceID("2e299711ce47710289dc6640727404f"))
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
	assert.Nil(t, err)
	assert.NotNil(t, response)
	assert.Equal(t, response.Data.TraceID, json.TraceID("3ba55609c3cde49649cd77d1f9dcd936"))
	assert.Nil(t, response.Errors)
	assert.NotNil(t, response.Data)
	assert.Equal(t, len(response.Data.Spans), 8)
	assert.Equal(t, response.Data.Matched, 8)
}

// TestGetTraceSpanIDEncoding reads the span ids of a captured Tempo trace response. The response
// writes them as base64, which the protobuf JSON mapping defines, while Tempo's search API writes
// the same ids as hex - so the trace detail has to report the hex text the rest of Kiali uses.
//
// The fixture's eight spans are listed here by both forms, so the test says which text turns into
// which.
func TestGetTraceSpanIDEncoding(t *testing.T) {
	baseUrl := getBaseUrl()

	// as the capture writes them, and as the bytes they decode to
	ids := map[string]string{
		"Sc130fnc2TY=": "49cd77d1f9dcd936",
		"hHVrHasIVvo=": "84756b1dab0856fa",
		"EhAELDb4vpI=": "1210042c36f8be92",
		"6cj9Hes+iN0=": "e9c8fd1deb3e88dd",
		"0j3l/q4Zp20=": "d23de5feae19a76d",
		"dxkQSMt9rcQ=": "77191048cb7dadc4",
		"fFl79W3fGgM=": "7c597bf56ddf1a03",
		"6dPV8CBW4Lw=": "e9d3d5f02056e0bc",
	}

	byteValue, err := os.ReadFile(responseTrace)
	require.NoError(t, err)
	for base64ID := range ids {
		require.Contains(t, string(byteValue), base64ID, "the capture no longer holds this id")
	}

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(byteValue))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	require.NoError(t, err)

	response, err := tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, baseUrl, "3ba55609c3cde49649cd77d1f9dcd936")
	require.NoError(t, err)
	require.Equal(t, 8, len(response.Data.Spans))

	hexIDs := map[string]bool{}
	for _, hexID := range ids {
		hexIDs[hexID] = true
	}

	reported := map[json.SpanID]bool{}
	for _, span := range response.Data.Spans {
		reported[span.SpanID] = true
		for _, reference := range span.References {
			// a reference names another span of the same trace and has to agree with it
			assert.True(t, hexIDs[string(reference.SpanID)], "reference to %s", reference.SpanID)
		}
	}
	for base64ID, hexID := range ids {
		assert.True(t, reported[json.SpanID(hexID)], "span %s is not reported as %s", base64ID, hexID)
		assert.False(t, reported[json.SpanID(base64ID)], "span %s is still reported as base64", base64ID)
	}
}

// TestGetTraceTypedAttributes reads a captured trace whose attributes are not all strings. Each one
// has to arrive with its value and the type that describes it, not with an empty value.
func TestGetTraceTypedAttributes(t *testing.T) {
	baseUrl := getBaseUrl()

	byteValue, err := os.ReadFile(responseTypedTrace)
	assert.Nil(t, err)

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(byteValue))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)

	response, err := tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, baseUrl, "cafe9bc0903e18f6b914752f8ee577a5")
	assert.Nil(t, err)
	assert.Nil(t, response.Errors)
	assert.Equal(t, 12, len(response.Data.Spans))

	tags := map[string]json.KeyValue{}
	for _, span := range response.Data.Spans {
		// every id the response reports is hex text, so a span in the trace detail can be found
		// again in the search results it was opened from
		assert.Len(t, span.SpanID, 16)
		for _, tag := range span.Tags {
			tags[tag.Key] = tag
		}
	}

	assert.Equal(t, json.Int64Type, tags["http.status_code"].Type)
	assert.Equal(t, int64(200), tags["http.status_code"].Value)
	assert.Equal(t, json.Int64Type, tags["net.host.port"].Type)
	assert.Equal(t, int64(8080), tags["net.host.port"].Value)
	assert.Equal(t, json.Int64Type, tags["grpc.status"].Type)
	// an array is reported as JSON, which is what Jaeger's own OTLP translation does with one
	assert.Equal(t, json.StringType, tags["@jaeger@warnings"].Type)
	assert.Contains(t, tags["@jaeger@warnings"].Value, "clock skew adjustment disabled")
}

// TestGetTracesTypedAttributes reads the same attributes on the search path, where they arrive
// inside Tempo's own response shape rather than as OTLP.
func TestGetTracesTypedAttributes(t *testing.T) {
	baseUrl := getBaseUrl()

	byteValue, err := os.ReadFile(responseTypedSearch)
	assert.Nil(t, err)

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(byteValue))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)

	response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, "orders-api.api-orders", models.TracingQuery{})
	assert.Nil(t, err)
	assert.Equal(t, 1, len(response.Data))
	assert.Equal(t, 10, len(response.Data[0].Spans))

	tags := map[string]json.KeyValue{}
	for _, tag := range response.Data[0].Spans[0].Tags {
		tags[tag.Key] = tag
	}
	assert.Equal(t, json.Int64Type, tags["http.status_code"].Type)
	assert.Equal(t, int64(200), tags["http.status_code"].Value)
	assert.Equal(t, json.Int64Type, tags["net.host.port"].Type)
	assert.Equal(t, int64(8080), tags["net.host.port"].Value)
}

// TestGetTraceEmpty covers the body Tempo answers with for a trace that holds no spans, which
// is {} because the marshaller behind its trace API leaves out a field that is empty.
func TestGetTraceEmpty(t *testing.T) {
	baseUrl := getBaseUrl()

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)

	response, err := tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, baseUrl, "3ba55609c3cde49649cd77d1f9dcd936")
	assert.Nil(t, err)
	assert.NotNil(t, response)
	// NotNil as well as Empty: a nil slice or map marshals to JSON null, which is what the
	// frontend's transformTraceData chokes on, and Empty alone is satisfied by either.
	assert.NotNil(t, response.Data.Spans)
	assert.Empty(t, response.Data.Spans)
	assert.NotNil(t, response.Data.Processes)
	assert.NotNil(t, response.Data.Warnings)
}

// TestGetTraceTwoScopes covers a resource whose spans arrive in more than one group. A resource
// carries one group per instrumentation scope, so a service instrumented by two libraries sends
// two, and every group's spans have to be read.
func TestGetTraceTwoScopes(t *testing.T) {
	baseUrl := getBaseUrl()

	body := `{"batches":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"orders-api"}}]},"scopeSpans":[` +
		`{"scope":{"name":"agentgateway"},"spans":[{"spanId":"AQIDBAUGBwg=","name":"first","startTimeUnixNano":"1701779876570888000","endTimeUnixNano":"1701779876580888000"}]},` +
		`{"scope":{"name":"nginx"},"spans":[{"spanId":"CAcGBQQDAgE=","name":"second","startTimeUnixNano":"1701779876570888000","endTimeUnixNano":"1701779876580888000"}]}` +
		`]}]}`

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)

	response, err := tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, baseUrl, "cafe9bc0903e18f6b914752f8ee577a5")
	assert.Nil(t, err)
	require.Equal(t, 2, len(response.Data.Spans))
	assert.Equal(t, "first", response.Data.Spans[0].OperationName)
	assert.Equal(t, "second", response.Data.Spans[1].OperationName)
	assert.Equal(t, json.SpanID("0102030405060708"), response.Data.Spans[0].SpanID)
	assert.Equal(t, json.SpanID("0807060504030201"), response.Data.Spans[1].SpanID)
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
	assert.Nil(t, err)

	all, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, serviceName, models.TracingQuery{})
	assert.Nil(t, err)
	assert.Equal(t, 4, len(all.Data))

	failed, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, serviceName,
		models.TracingQuery{Tags: map[string]string{"error": "true"}})
	assert.Nil(t, err)
	require.Equal(t, 1, len(failed.Data))
	assert.Equal(t, json.TraceID("bb00000000000000000000000000000b"), failed.Data[0].TraceID)
	// the errored span reports the boolean tag Kiali's frontend reads, not the attribute
	require.Equal(t, 1, len(failed.Data[0].Spans))
	assert.Contains(t, failed.Data[0].Spans[0].Tags,
		json.KeyValue{Key: "error", Value: true, Type: json.BoolType})
}

// TestUnreadableBody covers a 200 body that cannot be read, which has to reach the caller as an
// error rather than as a trace with no spans: an empty trace reads as a backend with nothing to
// show.
func TestUnreadableBody(t *testing.T) {
	baseUrl := getBaseUrl()

	cases := map[string]struct {
		body string
		// the search endpoint answers in a shape of Kiali's own, so a body that is JSON but not
		// that shape is read as a search that matched nothing; only the trace endpoint can tell
		// an envelope it does not know from a trace with no spans
		searchFails bool
	}{
		"an envelope with no span list in it": {body: `{"data":[{"spans":[]}]}`},
		"not JSON at all":                     {body: `No traces found`, searchFails: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}
			})}

			tempoClient, err := NewOtelClient(context.TODO())
			assert.Nil(t, err)

			_, err = tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, baseUrl, "cafe9bc0903e18f6b914752f8ee577a5")
			assert.Error(t, err)

			_, err = tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, serviceName, models.TracingQuery{})
			if tc.searchFails {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
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
