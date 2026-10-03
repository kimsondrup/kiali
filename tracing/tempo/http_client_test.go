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
	responseTypedErrors = "../tracingtest/responseTypedErrors.json"
	responseTypedTrace  = "../tracingtest/responseTypedTrace.json"

	// the trace id responseTypedTrace.json carries, which is the one the trace it was captured
	// from really had
	typedTraceID       = "60bad1536be1cd2f5b39bf02883c9656"
	tracingUrl         = "http://tracing.tempo"
	serviceName        = "productpage.bookinfo"
	ambientServiceName = "waypoint.bookinfo"
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

// TestGetTraceTypedAttributes reads a captured trace whose attributes are not all strings. Each
// one used to arrive with its key and an empty value, because the model declared an attribute
// value as a single stringValue field, so every other variant decoded to "" with no error.
//
// No span an Istio proxy exports can show this: Envoy stringifies every attribute before
// exporting it. The fixture is a trace through an agentgateway and an nginx OpenTelemetry
// module instead; ../tracingtest/README.adoc says where it came from.
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

	response, err := tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, baseUrl, typedTraceID)
	assert.Nil(t, err)
	assert.Nil(t, response.Errors)
	assert.Equal(t, 4, len(response.Data.Spans))

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
	// a zero that is an integer, not the empty string the old model made of it
	assert.Equal(t, int64(0), tags["grpc.status"].Value)
	// an array is reported as JSON, which is what Jaeger's own OTLP translation does with one
	assert.Equal(t, json.StringType, tags["@jaeger@warnings"].Type)
	assert.Contains(t, tags["@jaeger@warnings"].Value, "clock skew adjustment disabled")
}

// TestGetTraceScopeTags reads the instrumentation scope of the same captured trace, whose two
// resources were instrumented by different libraries. The scope used to decode into an empty
// struct, so the converter never saw it and these two tags could not be reported at all.
func TestGetTraceScopeTags(t *testing.T) {
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

	response, err := tempoClient.GetTraceDetailHTTP(context.Background(), httpClient, baseUrl, typedTraceID)
	assert.Nil(t, err)

	scopes := map[string]string{}
	for _, span := range response.Data.Spans {
		name, version := "", ""
		reported := false
		for _, tag := range span.Tags {
			switch tag.Key {
			case "otel.scope.name":
				name = tag.Value.(string)
			case "otel.scope.version":
				version = tag.Value.(string)
				reported = true
			}
		}
		assert.NotEmpty(t, name)
		// agentgateway sets a scope name and no version, and the version tag is left out
		// rather than reported empty - convertScope's own choice, as its doc comment says,
		// since the OpenTelemetry mapping to non-OTLP formats prescribes nothing about an
		// empty field. Asserted on the tag's presence, because an absent tag and one whose
		// value is "" read the same out of the map below.
		assert.Equal(t, name == "nginx", reported)
		scopes[name] = version
	}

	assert.Equal(t, map[string]string{"agentgateway": "", "nginx": "1.31.6"}, scopes)
}

// TestGetTracesTypedAttributes reads typed attributes on the search path, where they arrive
// inside Tempo's own response shape rather than as OTLP.
//
// The keys are .component and .response_flags, not http.status_code: an attribute reaches this
// path only if prepareTraceQL's select list named it, and those two are in that list. Captured
// from a live Tempo 3.1.0 answering Kiali's own TraceQL.
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

	response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, "typeprobe.devex", models.TracingQuery{})
	assert.Nil(t, err)
	assert.Equal(t, 1, len(response.Data))
	assert.Equal(t, 2, len(response.Data[0].Spans))

	tags := map[string]json.KeyValue{}
	for _, tag := range response.Data[0].Spans[0].Tags {
		tags[tag.Key] = tag
	}
	assert.Equal(t, json.Int64Type, tags["component"].Type)
	assert.Equal(t, int64(7), tags["component"].Value)
	assert.Equal(t, json.BoolType, tags["response_flags"].Type)
	assert.Equal(t, true, tags["response_flags"].Value)
}

// TestGetTracesErrorsFixture reads a live Tempo 3.1.0 answer to Kiali's own Errors only query,
// which is the Errors only path end to end rather than a hand-written body.
//
// The trace holds two spans: one that failed and one that did not. The failed one is kept
// because its "status" attribute - the TraceQL status intrinsic prepareTraceQL selects - came
// back as "error", and it reaches the caller carrying Kiali's boolean error tag.
//
// Note which type http.status_code has here: a string. It has to be, and that is a limit of the
// filter rather than of this fixture. Kiali builds the condition .http.status_code != "200"
// (prepareTraceQL via printOperator, which quotes any operand of Go type string), and TraceQL
// will not compare a quoted string against an int-typed attribute. Measured against the same
// live Tempo: a span whose http.status_code is {"intValue":"503"} is returned by the plain query
// and by .http.status_code != 200 unquoted, and is NOT returned by the query Kiali sends. So no
// Errors only response Kiali can receive carries an int-typed http.status_code. A typed
// attribute does reach the search path, through .component and .response_flags, which is what
// TestGetTracesTypedAttributes covers.
func TestGetTracesErrorsFixture(t *testing.T) {
	baseUrl := getBaseUrl()

	byteValue, err := os.ReadFile(responseTypedErrors)
	assert.Nil(t, err)

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(byteValue))),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)

	failed, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, "errorprobe.devex",
		models.TracingQuery{Tags: map[string]string{"error": "true"}})
	assert.Nil(t, err)
	require.Equal(t, 1, len(failed.Data))
	require.Equal(t, 2, len(failed.Data[0].Spans))

	tags := map[string]json.KeyValue{}
	for _, tag := range failed.Data[0].Spans[0].Tags {
		tags[tag.Key] = tag
	}
	assert.Equal(t, json.KeyValue{Key: "error", Value: true, Type: json.BoolType}, tags["error"])
	assert.Equal(t, json.StringType, tags["http.status_code"].Type)
	assert.Equal(t, "503", tags["http.status_code"].Value)
	// the span that did not fail keeps the attribute as the plain string Tempo sent
	assert.Contains(t, failed.Data[0].Spans[1].Tags,
		json.KeyValue{Key: "status", Value: "unset", Type: json.StringType})
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
	// NotNil as well as Empty: a nil slice marshals to JSON null, which is what the frontend
	// chokes on, and Empty alone is satisfied by either.
	assert.NotNil(t, response.Data.Spans)
	assert.Empty(t, response.Data.Spans)
	assert.NotNil(t, response.Data.Processes)
	assert.NotNil(t, response.Data.Warnings)
}

// TestGetTraceTwoScopes covers a resource whose spans arrive in more than one group. A resource
// carries one group per instrumentation scope, so a service instrumented by two libraries sends
// two, and only the first used to be read.
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

// TestUnreadableBody covers a 200 body that cannot be read. A trace detail used to be answered
// with a trace holding no spans whatever the body was, which reads as a backend that has nothing
// to show rather than as a response Kiali could not parse.
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

// TestGetTracesWithoutSpanSet covers a search result that reports no matched spans, which the
// service name used to be taken from without looking at whether there was one.
func TestGetTracesWithoutSpanSet(t *testing.T) {
	baseUrl := getBaseUrl()

	httpClient := http.Client{Transport: RoundTripFunc(func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"traces":[{"traceID":"cafe9bc0903e18f6b914752f8ee577a5","spanSet":{}}]}`)),
		}
	})}

	tempoClient, err := NewOtelClient(context.TODO())
	assert.Nil(t, err)

	response, err := tempoClient.GetAppTracesHTTP(context.Background(), httpClient, baseUrl, serviceName, models.TracingQuery{})
	assert.Nil(t, err)
	assert.Equal(t, 1, len(response.Data))
	assert.NotNil(t, response.Data[0].Spans)
	assert.Empty(t, response.Data[0].Spans)
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
