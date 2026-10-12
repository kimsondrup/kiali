package tempo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/store"
	"github.com/kiali/kiali/tracing/otel"
	"github.com/kiali/kiali/tracing/otel/model/converter"
	"github.com/kiali/kiali/tracing/otel/otlp"
	"github.com/kiali/kiali/tracing/tempo/querypb"
	"github.com/kiali/kiali/util"
)

const (
	expirationCheckInterval = 1 * time.Minute
	TTL                     = 5 * time.Minute
)

type OtelHTTPClient struct {
	TempoCache store.Store[string, *models.TracingSingleTrace]
}

// New client
func NewOtelClient(ctx context.Context) (otelClient *OtelHTTPClient, err error) {

	otelHTTPClient := &OtelHTTPClient{}
	conf := config.Get()
	if conf.ExternalServices.Tracing.TempoConfig.CacheEnabled {
		s := store.New[string, *models.TracingSingleTrace]()
		fifoStore := store.NewFIFOStore(s, conf.ExternalServices.Tracing.TempoConfig.CacheCapacity, "tempo")
		otelHTTPClient.TempoCache = store.NewExpirationStore[string, *models.TracingSingleTrace](ctx, fifoStore, util.AsPtr(TTL), util.AsPtr(expirationCheckInterval))
	}

	return otelHTTPClient, nil
}

// GetAppTracesHTTP search traces
func (oc *OtelHTTPClient) GetAppTracesHTTP(ctx context.Context, client http.Client, baseURL *url.URL, serviceName string, q models.TracingQuery) (response *models.TracingResponse, err error) {
	url := *baseURL
	url.Path = path.Join(url.Path, "/api/search")
	if q.End.Before(q.Start) {
		return nil, fmt.Errorf("end time must be greater than start time")
	}
	oc.prepareTraceQL(ctx, &url, serviceName, q)

	r, err := oc.queryTracesHTTP(ctx, client, &url)

	if r != nil {
		r.TracingServiceName = serviceName
	}
	return r, err
}

// GetTraceDetailHTTP get one trace by trace ID
func (oc *OtelHTTPClient) GetTraceDetailHTTP(ctx context.Context, client http.Client, endpoint *url.URL, traceID string) (*models.TracingSingleTrace, error) {
	u := *endpoint
	if config.Get().ExternalServices.Tracing.TempoConfig.CacheEnabled {
		if result, isCached := oc.TempoCache.Get(traceID); isCached {
			return result, nil
		}
	}

	zl := getLoggerFromContextHTTPTempo(ctx)

	u.Path = path.Join(u.Path, "/api/traces/", traceID)
	resp, code, reqError := otel.MakeRequest(ctx, client, u.String(), nil)
	if reqError != nil {
		zl.Error().Msgf("[HTTP Tempo] API Tempo query error: %s [code: %d, URL: %v]", reqError, code, u)
		return nil, reqError
	}
	if code != 200 {
		errorMsg := fmt.Sprintf("[HTTP Tempo] Error returning traces: %s", resp)
		zl.Error().Msg(errorMsg)
		var errorTrace []models.StructuredError
		errorTrace = append(errorTrace, models.StructuredError{TraceID: traceID, Code: code, Msg: errorMsg})
		return &models.TracingSingleTrace{Errors: errorTrace}, errors.New(errorMsg)
	}

	if len(resp) == 0 {
		return nil, errors.New("[HTTP Tempo] empty body response")
	}

	// Tempo's trace detail is OTLP under a key of its own naming, with its IDs in the base64 the
	// OTLP/JSON mapping gives a bytes field.
	traces, errDecode := otlp.Decode(resp, otlp.Options{IDEncoding: otlp.Base64, RootKey: otlp.RootKeyBatches})
	if errDecode != nil {
		zl.Error().Msgf("[HTTP Tempo] Error reading the trace detail response: %s [URL: %v]", errDecode, u)
		return nil, errDecode
	}

	// A 200 carrying no span is a trace Tempo does not have, which the caller reports as not
	// found. Reporting it as a trace with the requested ID and no spans shows an empty trace page
	// instead.
	trace := converter.TraceFromOTLP(traces)
	if trace == nil {
		return nil, nil
	}

	single := &models.TracingSingleTrace{Data: *trace}
	if config.Get().ExternalServices.Tracing.TempoConfig.CacheEnabled {
		oc.TempoCache.Set(traceID, single)
	}
	return single, nil
}

// GetServiceStatusHTTP get service status
func (oc *OtelHTTPClient) GetServiceStatusHTTP(ctx context.Context, client http.Client, baseURL *url.URL) (bool, error) {
	var u url.URL
	healthCheckUrl := config.Get().ExternalServices.Tracing.HealthCheckUrl
	if healthCheckUrl != "" {
		url, err := u.Parse(healthCheckUrl)
		if err != nil {
			return false, fmt.Errorf("[HTTP Tempo] Error %s incorrect healthCheckUrl", err)
		}
		u = *url
	} else {
		u = *baseURL
		u.Path = path.Join(u.Path, "/api/echo")
	}

	_, status, reqError := otel.MakeRequest(ctx, client, u.String(), nil)
	if status != 200 {
		return false, fmt.Errorf("[HTTP Tempo] Error %d getting status", status)
	}
	return reqError == nil, reqError
}

// GetServices reports the services Tempo knows of.
func (oc *OtelHTTPClient) GetServices(ctx context.Context, client http.Client, baseURL *url.URL) (serviceList []string, err error) {
	var u url.URL
	servicesList := []string{}

	u = *baseURL
	u.Path = path.Join(u.Path, "/status/services")

	zl := getLoggerFromContextHTTPTempo(ctx)
	r, code, reqError := otel.MakeRequest(ctx, client, u.String(), nil)
	// A failed request has no body to read, so unmarshalling it reports an empty JSON input and
	// the reason the request failed - a refused connection, an expired certificate - is lost.
	if reqError != nil {
		zl.Error().Msgf("[HTTP Tempo] API Tempo query error: %s [code: %d, URL: %v]", reqError, code, u)
		return nil, reqError
	}
	if errMarshal := json.Unmarshal(r, &servicesList); errMarshal != nil {
		zl.Error().Msgf("[HTTP Tempo] Error unmarshalling Tempo response: %s", errMarshal)
		return nil, errMarshal
	}
	return servicesList, nil
}

// queryTracesHTTP
func (oc *OtelHTTPClient) queryTracesHTTP(ctx context.Context, client http.Client, u *url.URL) (*models.TracingResponse, error) {

	zl := getLoggerFromContextHTTPTempo(ctx)

	// HTTP and GRPC requests co-exist, but when minDuration is present, for HTTP it requires a unit (ms)
	// https://github.com/kiali/kiali/issues/3939
	minDuration := u.Query().Get("minDuration")
	if minDuration != "" && !strings.HasSuffix(minDuration, "ms") {
		query := u.Query()
		query.Set("minDuration", minDuration)
		u.RawQuery = query.Encode()
	}
	resp, code, reqError := otel.MakeRequest(ctx, client, u.String(), nil)
	if reqError != nil {
		zl.Error().Msgf("Tempo API query error: %s [code: %d, URL: %v]", reqError, code, u)
		return &models.TracingResponse{}, reqError
	}
	if code != 200 {
		errorMsg := fmt.Sprintf("[HTTP Tempo] Tempo API query error: %s [code: %d, URL: %v]", resp, code, u)
		zl.Error().Msg(errorMsg)
		return &models.TracingResponse{}, errors.New(errorMsg)
	}
	limit, err := strconv.Atoi(u.Query().Get("limit"))
	if err != nil {
		limit = 0
	}
	// Handle a failed unmarshal rather than dropping it, so a backend that is not Tempo gives a
	// meaningful error.
	response, errUnmarshal := unmarshal(ctx, resp, u)
	if errUnmarshal != nil {
		return &models.TracingResponse{}, errUnmarshal
	}

	return oc.transformTrace(response, limit), nil
}

// transformTrace reports the traces a Tempo search matched.
func (oc *OtelHTTPClient) transformTrace(traces *querypb.SearchResponse, limit int) *models.TracingResponse {
	var response models.TracingResponse
	serviceName := ""

	for i, trace := range traces.GetTraces() {
		if limit != 0 && i >= limit {
			break
		}
		// a matched trace carries the spans that matched, and a trace that reports only its
		// statistics carries none; the service name then stays whatever the last one gave
		if spans := trace.GetSpanSet().GetSpans(); len(spans) > 0 {
			serviceName = getServiceName(spans[0].GetAttributes())
		}
		response.Data = append(response.Data, *converter.ConvertTraceMetadata(trace, serviceName))
	}

	response.TracingServiceName = serviceName
	return &response
}

// The keys a Tempo search answer carries. It always has at least one of them, including when it
// matched nothing.
const (
	keyTraces  = "traces"
	keyMetrics = "metrics"
)

// unmarshalSearch drops a field the IDL does not declare rather than failing on it, which the
// OTLP specification requires of the attributes the answer embeds and which Tempo's own habit of
// growing the answer between releases requires of the rest of it: a backend answering ahead of
// the generated stubs must not be able to blank the traces list. None of the captured responses
// needs the leniency - it is there for the Tempo that has not shipped yet.
//
// What strictness would have bought is bought instead by the envelope check below, which is what
// tells a Tempo search answer from any other JSON object.
var unmarshalSearch = protojson.UnmarshalOptions{DiscardUnknown: true}

func unmarshal(ctx context.Context, r []byte, u *url.URL) (*querypb.SearchResponse, error) {
	zl := getLoggerFromContextHTTPTempo(ctx)

	// Fail when the body is not shaped like a Tempo search answer, so the caller gets an error
	// rather than a result: every field of the answer is optional, so any JSON object decodes
	// into it cleanly and comes out as no traces.
	var envelope map[string]json.RawMessage
	if errEnvelope := json.Unmarshal(r, &envelope); errEnvelope != nil {
		zl.Error().Msgf("[HTTP Tempo] Error unmarshalling Tempo API response: %s [URL: %v]", errEnvelope, u)
		return nil, errEnvelope
	}
	if envelope[keyTraces] == nil && envelope[keyMetrics] == nil {
		errShape := fmt.Errorf("[HTTP Tempo] not a Tempo search response: it has none of %q and %q, only %q",
			keyTraces, keyMetrics, slices.Sorted(maps.Keys(envelope)))
		zl.Error().Msgf("%s [URL: %v]", errShape, u)
		return nil, errShape
	}

	var response querypb.SearchResponse
	if errMarshal := unmarshalSearch.Unmarshal(r, &response); errMarshal != nil {
		zl.Error().Msgf("[HTTP Tempo] Error unmarshalling Tempo API response: %s [URL: %v]", errMarshal, u)
		return nil, errMarshal
	}

	return &response, nil
}

// prepareTraceQL set the query in TraceQL format
func (oc *OtelHTTPClient) prepareTraceQL(ctx context.Context, u *url.URL, tracingServiceName string, query models.TracingQuery) {
	q := url.Values{}
	q.Set("start", fmt.Sprintf("%d", query.Start.Unix()))
	q.Set("end", fmt.Sprintf("%d", query.End.Unix()))

	// For Ambient, the service name is the waypoint
	queryPart := queryTags(TraceQL{operator1: ".service.name", operand: EQUAL, operator2: tracingServiceName}, query.Tags)

	selects := []string{
		"status",
		".service_name",
		".node_id",
		".component",
		".upstream_cluster",
		".http.method",
		".response_flags",
		".istio.destination_workload",
		".istio.destination_namespace",
		".istio.destination_instance_name",
		".istio.destination_canonical_service",
		".istio.source_workload",
		".istio.source_namespace",
		".istio.source_instance_name",
		".istio.source_canonical_service",
		"resource.hostname",
		"name",
	}
	trace := TraceQL{operator1: Subquery{queryPart}, operand: AND, operator2: Subquery{}}
	queryQL := fmt.Sprintf("%s| %s", printOperator(trace), printSelect(selects))

	q.Set("q", queryQL)
	if query.MinDuration > 0 {
		q.Set("minDuration", fmt.Sprintf("%dms", query.MinDuration.Milliseconds()))
	}
	// By default, the number of spans returned is 3. All are needed to calculate avg and heatmap
	q.Set("spss", "10")
	if query.Limit > 0 {
		q.Set("limit", strconv.Itoa(query.Limit))
	}
	u.RawQuery = q.Encode()

	getLoggerFromContextHTTPTempo(ctx).Debug().Msgf("[HTTP Tempo] Prepared Tempo API query: %v", u)
}

// GetTraceQLQuery returns the raw query in TraceQL format
func (oc *OtelHTTPClient) GetTraceQLQuery(ctx context.Context, u *url.URL, tracingServiceName string, query models.TracingQuery) string {
	oc.prepareTraceQL(ctx, u, tracingServiceName, query)
	return u.RawQuery
}

func getServiceName(attributes []*commonpb.KeyValue) string {
	for _, attb := range attributes {
		if attb.GetKey() == "service.name" {
			return attb.GetValue().GetStringValue()
		}
	}
	return ""
}

// ConstructTempoTenantURL constructs a tenant-specific URL for Tempo provider if needed.
// This function checks if the URL is just the base server URL (no path or minimal path)
// and if so, appends the tenant-specific path.
func ConstructTempoTenantURL(u *url.URL, cfgTracing *config.TracingConfig, isInternalURL bool) {
	if cfgTracing.Provider == config.TempoProvider && cfgTracing.TempoConfig.Tenant != "" && isInternalURL {
		// Check if the URL is just the base server URL (no path or minimal path)
		// If so, append the tenant-specific path
		if u.Path == "" || u.Path == "/" {
			u.Path = fmt.Sprintf("/api/traces/v1/%s/tempo", cfgTracing.TempoConfig.Tenant)
			log.Debugf("Constructed Tempo tenant URL: %s", u.String())
		}
	}
}
