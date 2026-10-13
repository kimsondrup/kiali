package jaeger

import (
	"errors"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kiali/kiali/models"
	jsonModel "github.com/kiali/kiali/models/tracing/json"
	"github.com/kiali/kiali/tracing/otel/model/converter"
)

// searchResponse turns a search's decoded payload into Kiali's response, and is where the empty
// and failure policies are applied for both transports. A transport's job ends at handing over
// the payload and the classified status.
func searchResponse(serviceName string, traces *tracepb.TracesData, err error) (*models.TracingResponse, error) {
	// A search that matched nothing is an answer, not a failure.
	if errors.Is(err, errEmptyResult) {
		return emptyResponse(serviceName), nil
	}
	if err != nil {
		return nil, err
	}

	response := emptyResponse(serviceName)
	response.Data = converter.TracesFromOTLP(traces)
	return response, nil
}

// traceResponse turns a trace detail's decoded payload into Kiali's response. A trace the backend
// does not hold is (nil, nil), which handlers/tracing.go reports as a 404. That covers all three
// ways api_v3 says so: an HTTP 404 carrying its envelope, gRPC NotFound (Jaeger 2.21), and gRPC
// OK with an empty stream (2.17 and 2.20), which arrives here as a payload with no span.
func traceResponse(traces *tracepb.TracesData, err error) (*models.TracingSingleTrace, error) {
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	trace := converter.TraceFromOTLP(traces)
	if trace == nil {
		return nil, nil
	}
	return &models.TracingSingleTrace{Data: *trace}, nil
}

// emptyResponse is a search answer carrying no trace.
//
// TracingServiceName is set here rather than by the caller because business/tracing.go selects a
// trace's spans by matching it, so a response that leaves it empty yields no spans and no error.
func emptyResponse(serviceName string) *models.TracingResponse {
	return &models.TracingResponse{
		Data:               []jsonModel.Trace{},
		TracingServiceName: serviceName,
	}
}
