package business

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/kubernetes/kubetest"
	"github.com/kiali/kiali/models"
	jsonModel "github.com/kiali/kiali/models/tracing/json"
	"github.com/kiali/kiali/tracing/otel/model/converter"
	"github.com/kiali/kiali/tracing/tempo/querypb"
)

func getLayer(t *testing.T, conf *config.Config) *Layer {
	config.Set(conf)
	s1 := kubetest.FakeService("Namespace", "reviews")
	s2 := kubetest.FakeService("Namespace", "httpbin")
	objects := []runtime.Object{
		kubetest.FakeNamespace("Namespace"),
		&s1,
		&s2,
	}

	k8s := kubetest.NewFakeK8sClient(objects...)
	return NewLayerBuilder(t, conf).WithClient(k8s).Build()
}

var trace1 = jsonModel.Trace{
	Spans: []jsonModel.Span{{
		ProcessID:     "t1_process_1",
		OperationName: "reviews.default.svc.cluster.local:8000/*",
		Tags: []jsonModel.KeyValue{{
			Key:   "dummy",
			Value: "dummy",
		}, {
			Key:   "node_id",
			Value: "sidecar~172.17.0.20~reviews-6d8996bff-ztg6z.default~default.svc.cluster.local",
		}},
	}, {
		ProcessID:     "t1_process_2",
		OperationName: "my-operation",
		Tags: []jsonModel.KeyValue{{
			Key:   "dummy",
			Value: "dummy",
		}},
	}},
	Processes: map[jsonModel.ProcessID]jsonModel.Process{
		"t1_process_1": {
			ServiceName: "reviews.default",
			Tags: []jsonModel.KeyValue{{
				Key:   "dummy",
				Value: "dummy",
			}},
		},
		"t1_process_2": {
			ServiceName: "my-pod",
			Tags: []jsonModel.KeyValue{{
				Key:   "dummy",
				Value: "dummy",
			}, {
				Key:   "hostname",
				Value: "my-pod-123456-789abc",
			}},
		},
	},
}

var trace2 = jsonModel.Trace{
	Spans: []jsonModel.Span{{
		ProcessID:     "t2_process_1",
		OperationName: "reviews.default.svc.cluster.local:8000/*",
		Tags: []jsonModel.KeyValue{{
			Key:   "node_id",
			Value: "sidecar~172.17.0.20~reviews-6d8996bff-ztg6z.default~default.svc.cluster.local",
		}},
	}, {
		ProcessID:     "t2_process_2",
		OperationName: "rating.default.svc.cluster.local:8000/*",
		Tags: []jsonModel.KeyValue{{
			Key:   "node_id",
			Value: "sidecar~172.17.0.20~rating-v2-6d8996bff-ztg6z.default~default.svc.cluster.local",
		}},
	}, {
		ProcessID:     "t2_process_3",
		OperationName: "my-operation",
		Tags:          []jsonModel.KeyValue{},
	}},
	Processes: map[jsonModel.ProcessID]jsonModel.Process{
		"t2_process_1": {
			ServiceName: "reviews.default",
			Tags:        []jsonModel.KeyValue{},
		},
		"t2_process_2": {
			ServiceName: "rating.default",
			Tags:        []jsonModel.KeyValue{},
		},
		"t2_process_3": {
			ServiceName: "rating",
			Tags: []jsonModel.KeyValue{{
				Key:   "hostname",
				Value: "rating-v2-6d8996bff-ztg6z",
			}},
		},
	},
}

var waypointTrace = jsonModel.Trace{
	Spans: []jsonModel.Span{{
		ProcessID:     "t1_process_1",
		OperationName: "reviews.default.svc.cluster.local:8000/*",
		Tags: []jsonModel.KeyValue{{
			Key:   "dummy",
			Value: "dummy",
		}, {
			Key:   "node_id",
			Value: "waypoint~10.244.0.19~waypoint-5f89fbb8b5-nxxmj.default~default.svc.cluster.local",
		}},
	}, {
		ProcessID:     "t1_process_2",
		OperationName: "my-operation",
		Tags: []jsonModel.KeyValue{{
			Key:   "dummy",
			Value: "dummy",
		}},
	}},
	Processes: map[jsonModel.ProcessID]jsonModel.Process{
		"t1_process_1": {
			ServiceName: "waypoint.default",
			Tags:        []jsonModel.KeyValue{},
		},
		"t1_process_2": {
			ServiceName: "bookinfo-gateway-istio.default",
			Tags:        []jsonModel.KeyValue{},
		},
	},
}

var ambientTagTrace = jsonModel.Trace{
	Spans: []jsonModel.Span{{
		ProcessID:     "at_process_1",
		OperationName: "waypoint-proxy-operation",
		Tags: []jsonModel.KeyValue{{
			Key:   "istio.destination_workload",
			Value: "reviews-v1",
		}, {
			Key:   "istio.destination_namespace",
			Value: "default",
		}, {
			Key:   "istio.destination_canonical_service",
			Value: "reviews",
		}, {
			Key:   "istio.source_workload",
			Value: "productpage-v1",
		}, {
			Key:   "istio.source_namespace",
			Value: "default",
		}},
	}},
	Processes: map[jsonModel.ProcessID]jsonModel.Process{
		"at_process_1": {
			ServiceName: "waypoint.default",
			Tags:        []jsonModel.KeyValue{},
		},
	},
}

func TestMatchingWorkload(t *testing.T) {
	assert := assert.New(t)
	tracingName := models.TracingName{App: "some-workload", Workload: "some-workload", Lookup: "some-workload"}
	assert.False(matchesWorkload(context.Background(), &trace1, "default", tracingName))
	tracingNameReviews := models.TracingName{App: "reviews", Workload: "reviews", Lookup: "reviews"}
	assert.True(matchesWorkload(context.Background(), &trace1, "default", tracingNameReviews))
	tracingNamePod := models.TracingName{App: "my-pod", Workload: "my-pod", Lookup: "my-pod"}
	assert.True(matchesWorkload(context.Background(), &trace1, "default", tracingNamePod))
}

func TestTracesToSpanWithoutFilter(t *testing.T) {
	assert := assert.New(t)

	r := models.TracingResponse{
		Data:               []jsonModel.Trace{trace1, trace2},
		TracingServiceName: "reviews.default",
	}
	reviewsTracingName := models.TracingName{App: "reviews", Lookup: "reviews"}
	spans := tracesToSpans(context.Background(), reviewsTracingName, &r, nil, config.NewConfig())
	assert.Len(spans, 2)
	assert.Equal("t1_process_1", string(spans[0].ProcessID))
	assert.Equal("t2_process_1", string(spans[1].ProcessID))

	r = models.TracingResponse{
		Data:               []jsonModel.Trace{trace1, trace2},
		TracingServiceName: "rating.default",
	}
	ratingsTracingName := models.TracingName{App: "rating", Lookup: "rating"}
	spans = tracesToSpans(context.Background(), ratingsTracingName, &r, nil, config.NewConfig())
	assert.Len(spans, 2)
	assert.Equal("t2_process_2", string(spans[0].ProcessID))
	assert.Equal("t2_process_3", string(spans[1].ProcessID))
}

// TestTracesToSpanAgreesAcrossProviders reads one captured Tempo search answer through the
// converter both of Tempo's search transports use, and selects its spans the way each provider
// is selected for: by the process table, and by the process embedded in a span. The two have to
// name the same spans, because the business layer is the one place that has no business knowing
// which provider answered.
func TestTracesToSpanAgreesAcrossProviders(t *testing.T) {
	const serviceName = "reviews.bookinfo"

	body, err := os.ReadFile("../tracing/tracingtest/response.json")
	require.NoError(t, err)
	var search querypb.SearchResponse
	require.NoError(t, protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, &search))
	require.NotEmpty(t, search.GetTraces())

	r := models.TracingResponse{TracingServiceName: serviceName}
	for _, trace := range search.GetTraces() {
		r.Data = append(r.Data, *converter.ConvertTraceMetadata(trace, serviceName))
	}

	app := models.TracingName{App: "reviews", Lookup: "reviews"}
	tempoConf, jaegerConf := config.NewConfig(), config.NewConfig()
	tempoConf.ExternalServices.Tracing.Provider = config.TempoProvider
	jaegerConf.ExternalServices.Tracing.Provider = config.JaegerProvider

	byTable := tracesToSpans(context.Background(), app, &r, nil, jaegerConf)
	byEmbeddedProcess := tracesToSpans(context.Background(), app, &r, nil, tempoConf)

	assert.NotEmpty(t, byTable)
	assert.Equal(t, byTable, byEmbeddedProcess)
}

func TestTracesToSpanWithServiceFilter(t *testing.T) {
	assert := assert.New(t)

	r := models.TracingResponse{
		Data:               []jsonModel.Trace{trace1, trace2},
		TracingServiceName: "reviews.default",
	}
	reviewsTracingName := models.TracingName{App: "reviews", Lookup: "reviews"}
	spans := tracesToSpans(context.Background(), reviewsTracingName, &r, operationSpanFilter(context.Background(), "default", "reviews"), config.NewConfig())
	assert.Len(spans, 2)
	assert.Equal("t1_process_1", string(spans[0].ProcessID))
	assert.Equal("t2_process_1", string(spans[1].ProcessID))

	r = models.TracingResponse{
		Data:               []jsonModel.Trace{trace1, trace2},
		TracingServiceName: "rating.default",
	}
	ratingsTracingName := models.TracingName{App: "rating", Lookup: "rating"}
	spans = tracesToSpans(context.Background(), ratingsTracingName, &r, operationSpanFilter(context.Background(), "default", "rating"), config.NewConfig())
	assert.Len(spans, 1)
	assert.Equal("t2_process_2", string(spans[0].ProcessID))
}

func TestTracesToSpanWithWorkloadFilter(t *testing.T) {
	assert := assert.New(t)

	r := models.TracingResponse{
		Data:               []jsonModel.Trace{trace1, trace2},
		TracingServiceName: "reviews.default",
	}
	tracingName := models.TracingName{App: "reviews", Workload: "reviews", Lookup: "reviews"}
	spans := tracesToSpans(context.Background(), tracingName, &r, wkdSpanFilter(context.Background(), "default", tracingName), config.NewConfig())
	assert.Len(spans, 2)
	assert.Equal("t1_process_1", string(spans[0].ProcessID))
	assert.Equal("t2_process_1", string(spans[1].ProcessID))

	r = models.TracingResponse{
		Data:               []jsonModel.Trace{trace1, trace2},
		TracingServiceName: "rating.default",
	}
	tracingRatingsName := models.TracingName{App: "rating", Workload: "rating-v2", Lookup: "rating"}
	spans = tracesToSpans(context.Background(), tracingRatingsName, &r, wkdSpanFilter(context.Background(), "default", tracingRatingsName), config.NewConfig())
	assert.Len(spans, 2)
	assert.Equal("t2_process_2", string(spans[0].ProcessID))
	assert.Equal("t2_process_3", string(spans[1].ProcessID))
}

func TestTracesToSpanWaypointWithWorkloadFilter(t *testing.T) {
	assert := assert.New(t)

	r := models.TracingResponse{
		Data:               []jsonModel.Trace{waypointTrace},
		TracingServiceName: "waypoint.default",
	}
	tracingName := models.TracingName{App: "reviews", Workload: "reviews", Lookup: "waypoint", WaypointName: "waypoint"}
	spans := tracesToSpans(context.Background(), tracingName, &r, wkdSpanFilter(context.Background(), "default", tracingName), config.NewConfig())
	assert.Len(spans, 1)
	// Process is empty here, because the span we are looking for is the service and the process is the waypoint
}

func TestTracesToSpanWaypointWithAmbientTagWorkloadFilter(t *testing.T) {
	assert := assert.New(t)

	r := models.TracingResponse{
		Data:               []jsonModel.Trace{ambientTagTrace},
		TracingServiceName: "waypoint.default",
	}
	tracingName := models.TracingName{App: "reviews", Workload: "reviews-v1", Lookup: "waypoint", WaypointName: "waypoint"}
	spans := tracesToSpans(context.Background(), tracingName, &r, wkdSpanFilter(context.Background(), "default", tracingName), config.NewConfig())
	assert.Len(spans, 1)
}

func TestOperationSpanFilterMatchesAmbientCanonicalService(t *testing.T) {
	assert := assert.New(t)

	filter := operationSpanFilter(context.Background(), "default", "reviews")
	span := jsonModel.Span{
		OperationName: "does-not-have-service-prefix",
		Tags: []jsonModel.KeyValue{
			{Key: "istio.destination_canonical_service", Value: "reviews"},
			{Key: "istio.destination_namespace", Value: "default"},
		},
	}
	assert.True(filter(&span))
}

func TestTracesToSpanWaypointWithAmbientTagsDoNotFallbackToAppPrefix(t *testing.T) {
	assert := assert.New(t)

	trace := jsonModel.Trace{
		Spans: []jsonModel.Span{{
			ProcessID:     "at_process_1",
			OperationName: "reviews.default.svc.cluster.local:9080/*",
			Tags: []jsonModel.KeyValue{
				{Key: "istio.destination_workload", Value: "reviews-v1"},
				{Key: "istio.destination_namespace", Value: "default"},
			},
		}},
		Processes: map[jsonModel.ProcessID]jsonModel.Process{
			"at_process_1": {
				ServiceName: "waypoint.default",
				Tags:        []jsonModel.KeyValue{},
			},
		},
	}

	r := models.TracingResponse{
		Data:               []jsonModel.Trace{trace},
		TracingServiceName: "waypoint.default",
	}
	tracingName := models.TracingName{App: "reviews", Workload: "reviews-v2", Lookup: "waypoint", WaypointName: "waypoint"}
	spans := tracesToSpans(context.Background(), tracingName, &r, wkdSpanFilter(context.Background(), "default", tracingName), config.NewConfig())
	assert.Len(spans, 0)
}

func TestTracingServiceNameNonDefaultDomain(t *testing.T) {
	cases := map[string]struct {
		namespaceSelector bool
		identityDomain    string
		namespace         string
		app               string
		expected          string
	}{
		"ns selector off returns app as-is": {
			namespaceSelector: false,
			identityDomain:    "svc.example.org",
			namespace:         "bookinfo",
			app:               "reviews",
			expected:          "reviews",
		},
		"ns selector on, app lacks domain, returns app.ns": {
			namespaceSelector: true,
			identityDomain:    "svc.example.org",
			namespace:         "bookinfo",
			app:               "reviews",
			expected:          "reviews.bookinfo",
		},
		"ns selector on, app contains domain, returns app as-is": {
			namespaceSelector: true,
			identityDomain:    "svc.example.org",
			namespace:         "bookinfo",
			app:               "reviews.bookinfo.svc.example.org",
			expected:          "reviews.bookinfo.svc.example.org",
		},
		"default domain, ns selector on, app lacks domain": {
			namespaceSelector: true,
			identityDomain:    "svc.cluster.local",
			namespace:         "default",
			app:               "httpbin",
			expected:          "httpbin.default",
		},
		"default domain, ns selector on, app contains domain": {
			namespaceSelector: true,
			identityDomain:    "svc.cluster.local",
			namespace:         "default",
			app:               "httpbin.default.svc.cluster.local",
			expected:          "httpbin.default.svc.cluster.local",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tracingServiceName(tc.namespaceSelector, tc.identityDomain, tc.namespace, tc.app)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestValidateConfiguration(t *testing.T) {
	assert := assert.New(t)
	conf := config.NewConfig()
	conf.ExternalServices.Tracing.Enabled = true
	layer := getLayer(t, conf)

	tracingConfig := config.TracingConfig{Enabled: true, InternalURL: "http://localhost:59999", UseGRPC: false}

	validConfig := layer.Tracing.ValidateConfiguration(context.TODO(), conf, &tracingConfig, "")

	assert.NotNil(validConfig)
	assert.NotNil(validConfig.Error)
	assert.Contains(validConfig.Error, "connection refused")

	tracingConfig = config.TracingConfig{Enabled: false, InternalURL: "http://localhost:59999", UseGRPC: false}
	validConfig = layer.Tracing.ValidateConfiguration(context.TODO(), conf, &tracingConfig, "")

	assert.NotNil(validConfig)
	assert.NotNil(validConfig.Error)
	assert.Contains(validConfig.Error, "Error creating tracing client")
}
