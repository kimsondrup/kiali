package jaeger

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
	"strings"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/otel"
	"github.com/kiali/kiali/tracing/otel/otlp"
)

// servicesPath is the api_v3 HTTP route listing the services the store holds spans for.
const servicesPath = "/api/v3/services"

// HTTPClient reads Jaeger api_v3 through its HTTP gateway, which is served under the query
// service's base path at /api/v3. It holds no state: tracing.HTTPClientInterface hands it the
// http.Client and the base URL on every call.
type HTTPClient struct{}

// NewHTTPClient returns the api_v3 HTTP transport.
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{}
}

// GetAppTracesHTTP searches a service's traces in one request at the depth asked for.
//
// The tags go out as query.attributes, which Jaeger 2.20 and later apply on the server, and are
// applied again by keepTracesWithTags to the converted answer, because Jaeger 2.17 and older
// ignore query.attributes over HTTP and answer the newest traces of the service whatever they
// carry. On a gateway that filters, that second pass keeps what the server kept, except for the
// few tags keepTracesWithTags names that the converted trace does not carry.
//
// On a gateway that ignores the attributes, the depth applies before Kiali's filter, so a tag
// search can return fewer matching traces than the window holds: only the matches among the
// newest traces, and none when every match is older than the depth. The gRPC transport, and HTTP
// on Jaeger 2.20 and later, apply the tags before the depth and do not have this limit. Searching
// deeper to make up for it cost a 2.17 store seconds and tens of megabytes on every errors-only
// refresh of the traces page.
func (c *HTTPClient) GetAppTracesHTTP(ctx context.Context, client http.Client, baseURL *url.URL, serviceName string, q models.TracingQuery) (*models.TracingResponse, error) {
	tq, err := newTraceQuery(serviceName, q)
	if err != nil {
		// A window nothing can match is an empty answer, and is given without a request.
		return searchResponse(serviceName, nil, err)
	}

	values, err := tq.values()
	if err != nil {
		return nil, err
	}
	traces, err := c.getTraces(ctx, client, baseURL, searchPath, values, errEmptyResult)
	response, err := searchResponse(tq.serviceName, traces, err)
	if err != nil {
		return nil, err
	}
	response.Data = keepTracesWithTags(response.Data, tq.attributes)
	return response, nil
}

// GetTraceDetailHTTP reads one trace. A trace the store does not hold is (nil, nil), which the
// handler reports as a 404.
func (c *HTTPClient) GetTraceDetailHTTP(ctx context.Context, client http.Client, endpoint *url.URL, traceID string) (*models.TracingSingleTrace, error) {
	route, err := tracePath(traceID)
	if err != nil {
		return nil, err
	}
	traces, err := c.getTraces(ctx, client, endpoint, route, nil, errNotFound)
	return traceResponse(traces, err)
}

// GetServiceStatusHTTP is the health probe: it asks api_v3 for its services, so a backend that
// answers but does not serve api_v3 fails here with a message saying so, instead of passing a
// probe and then showing an empty trace list. A configured health_check_url is not consulted;
// Jaeger's probe never did.
func (c *HTTPClient) GetServiceStatusHTTP(ctx context.Context, client http.Client, baseURL *url.URL) (bool, error) {
	if _, err := c.GetServices(ctx, client, baseURL); err != nil {
		return false, err
	}
	return true, nil
}

// GetServices lists the services the store holds spans for.
func (c *HTTPClient) GetServices(ctx context.Context, client http.Client, baseURL *url.URL) ([]string, error) {
	body, code, where, err := get(ctx, client, baseURL, servicesPath, nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		statusErr := classifyHTTPStatus(code, body, errNotFound)
		// The services route has no "nothing" to report: any 404 means the route is not there.
		if code == http.StatusNotFound {
			return nil, notAPIv3Error(where, statusErr)
		}
		return nil, statusErr
	}
	services, err := DecodeServices(body)
	if err != nil {
		return nil, notAPIv3Error(where, err)
	}
	return services, nil
}

// DecodeServices reads the body of an api_v3 GET /api/v3/services. Discovery reads candidate
// endpoints through it too, so that it can only suggest an endpoint this client accepts. Every
// error it returns means the body is not an api_v3 services answer.
func DecodeServices(body []byte) ([]string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("the services answer is not a JSON object: %w", err)
	}
	if envelope == nil {
		return nil, errors.New("the services answer is null rather than a JSON object")
	}

	raw, found := envelope["services"]
	if !found {
		// protojson leaves out a repeated field with no element, so a store holding no span
		// answers {} rather than {"services":[]}.
		if len(envelope) == 0 {
			return []string{}, nil
		}
		if _, legacy := envelope["data"]; legacy {
			return nil, errors.New("the services answer has the key \"data\" instead of \"services\", which is the shape of the pre-api_v3 /api/services route")
		}
		return nil, fmt.Errorf("the services answer has no \"services\" key, only %s", strings.Join(slices.Sorted(maps.Keys(envelope)), ", "))
	}

	var services []string
	if err := json.Unmarshal(raw, &services); err != nil {
		return nil, fmt.Errorf("the services answer's \"services\" is not a list of names: %w", err)
	}
	if services == nil {
		services = []string{}
	}
	return services, nil
}

// getTraces requests an api_v3 route answering OTLP and decodes the answer. nothing is the
// sentinel the backend's "No traces found" stands for at this route; it is returned as is, for
// searchResponse or traceResponse to apply.
func (c *HTTPClient) getTraces(ctx context.Context, client http.Client, baseURL *url.URL, route string, query url.Values, nothing error) (*tracepb.TracesData, error) {
	body, code, where, err := get(ctx, client, baseURL, route, query)
	if err != nil {
		return nil, err
	}
	if err := classifyHTTPStatus(code, body, nothing); err != nil {
		// A 404 without the envelope is the router's own "404 page not found": no api_v3 here.
		if code == http.StatusNotFound && !errors.Is(err, nothing) {
			return nil, notAPIv3Error(where, err)
		}
		return nil, err
	}

	// api_v3 over HTTP is grpc-gateway rendering the gRPC stream: each message under a "result"
	// key, with IDs in hex rather than the base64 OTLP/JSON specifies. A 200 that does not decode
	// is most often the Jaeger UI's HTML, answered for any path under a wrong base path.
	traces, err := otlp.Decode(body, otlp.Options{IDEncoding: otlp.Hex, RootKey: otlp.RootKeyResult})
	if err != nil {
		return nil, notAPIv3Error(where, err)
	}
	return traces, nil
}

// get sends a GET to an api_v3 route under the base URL's path and returns the body, the status
// and the URL it reached, without its query, for messages to name. The status is the caller's
// to classify: otel.MakeRequest returns no error for an answer of any status.
func get(ctx context.Context, client http.Client, baseURL *url.URL, route string, query url.Values) ([]byte, int, string, error) {
	u := *baseURL
	// The base path joins for HTTP: a Jaeger served under /jaeger serves api_v3 at
	// /jaeger/api/v3.
	u.Path = path.Join(u.Path, route)
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	where := u.Redacted()
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	zl := getLoggerFromContextHTTPJaeger(ctx)
	zl.Debug().Msgf("Jaeger api_v3 request: %s", u.Redacted())

	body, code, err := otel.MakeRequest(ctx, client, u.String(), nil)
	if err != nil {
		zl.Debug().Msgf("Jaeger api_v3 request failed: %s [URL: %s]", err, u.Redacted())
		// HTTP sent to a gRPC port reads the HTTP/2 SETTINGS frame as a malformed status line,
		// which on its own does not say that use_grpc and the port disagree.
		if wrongTransport := HTTPWrongTransportError(where, err); wrongTransport != nil {
			return nil, 0, where, wrongTransport
		}
		return nil, 0, where, fmt.Errorf("Jaeger api_v3 request to %s failed: %w", where, err)
	}
	return body, code, where, nil
}
