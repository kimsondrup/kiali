package jaeger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiali/kiali/tracing/otel/otlp"
)

// Captured live from Jaeger 2.21.0: a search answering three traces, and one trace of three spans.
// tracing/tracingtest/README.adoc records how.
const (
	apiv3SearchHTTP = "../tracingtest/apiv3_search_http.json"
	apiv3TraceHTTP  = "../tracingtest/apiv3_trace_http.json"
)

// The body api_v3 answers an unmatched search and an unknown trace with, over HTTP. The two are
// indistinguishable, which is why the sentinel is the caller's to choose.
const noTracesBody = `{"error":{"httpCode":404,"message":"No traces found"}}`

func decodeFixture(t *testing.T, file string) *tracepb.TracesData {
	t.Helper()
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	traces, err := otlp.Decode(body, otlp.Options{RootKey: otlp.RootKeyResult, IDEncoding: otlp.Hex})
	require.NoError(t, err)
	return traces
}

func TestSearchResponseNamesTheService(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	response, err := searchResponse("waypoint.tp-otlp-sf2ed7", decodeFixture(t, apiv3SearchHTTP), nil)
	require.NoError(err)
	require.NotNil(response)

	// business/tracing.go matches a trace's processes against this name, so a response that
	// leaves it empty yields no spans and no error.
	assert.Equal("waypoint.tp-otlp-sf2ed7", response.TracingServiceName)
	assert.Len(response.Data, 3)
}

// A payload with no resource spans is an empty list, never a nil dereference and never nil Data:
// the handlers marshal Data straight to the browser, where null and [] are not the same thing.
func TestSearchResponseWithNoSpans(t *testing.T) {
	for name, traces := range map[string]*tracepb.TracesData{
		"an empty payload": {},
		"no payload":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			response, err := searchResponse("waypoint.tp-abc", traces, nil)
			require.NoError(t, err)
			require.NotNil(t, response)
			assert.NotNil(t, response.Data)
			assert.Empty(t, response.Data)
			assert.Equal(t, "waypoint.tp-abc", response.TracingServiceName)
		})
	}
}

func TestSearchResponseEmptyResultIsAnAnswer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	response, err := searchResponse("waypoint.tp-abc", nil, errEmptyResult)
	require.NoError(err)
	require.NotNil(response)
	assert.NotNil(response.Data)
	assert.Empty(response.Data)
	assert.Equal("waypoint.tp-abc", response.TracingServiceName)
}

func TestSearchResponseReportsAFailure(t *testing.T) {
	failure := errors.New("Jaeger api_v3 answered 500: something broke")
	response, err := searchResponse("waypoint.tp-abc", decodeFixture(t, apiv3SearchHTTP), failure)

	// Not an empty list: a search that could not run is reported, so the UI does not show
	// "no traces" for a backend that is down.
	assert.Nil(t, response)
	assert.ErrorIs(t, err, failure)
}

func TestTraceResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	single, err := traceResponse(decodeFixture(t, apiv3TraceHTTP), nil)
	require.NoError(err)
	require.NotNil(single)
	assert.Len(single.Data.TraceID, 32)
	assert.Len(single.Data.Spans, 3)
}

// A trace the backend does not hold is (nil, nil), which handlers/tracing.go reports as a 404.
func TestTraceResponseNotFound(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		traces *tracepb.TracesData
	}{
		"the backend said so":    {err: errNotFound},
		"a payload with no span": {traces: &tracepb.TracesData{}},
	} {
		t.Run(name, func(t *testing.T) {
			single, err := traceResponse(tc.traces, tc.err)
			assert.NoError(t, err)
			assert.Nil(t, single)
		})
	}
}

func TestTraceResponseReportsAFailure(t *testing.T) {
	failure := errors.New("Jaeger api_v3 answered 400: malformed parameter trace_id")
	single, err := traceResponse(nil, failure)

	assert.Nil(t, single)
	assert.ErrorIs(t, err, failure)
}

// The HTTP half of the policy. Measured against Jaeger 2.17.0, 2.20.0 and 2.21.0: both an
// unmatched search and an unknown trace ID answer 404 with the api_v3 error envelope, while a
// mistyped path answers 404 with plain text, and a malformed parameter answers 400 or 500 with
// the envelope. Every one of those error bodies is labelled text/plain, so nothing here reads
// the content type.
func TestHTTPStatusPolicy(t *testing.T) {
	cases := []struct {
		body    string
		code    int
		message string
		name    string
		nothing error
		want    error // nil, or one of the sentinels
	}{
		{name: "a search that answered", code: http.StatusOK, body: `{"result":{"resourceSpans":[]}}`, nothing: errEmptyResult},
		{name: "a search that matched nothing", code: http.StatusNotFound, body: noTracesBody, nothing: errEmptyResult, want: errEmptyResult},
		{name: "a trace the store does not hold", code: http.StatusNotFound, body: noTracesBody, nothing: errNotFound, want: errNotFound},
		{
			name: "a mistyped path", code: http.StatusNotFound, body: "404 page not found\n",
			nothing: errEmptyResult, message: "404 page not found",
		},
		{
			name: "a malformed parameter", code: http.StatusBadRequest,
			body:    `{"error":{"httpCode":400,"message":"malformed parameter query.duration_min: time: missing unit in duration \"5\""}}`,
			nothing: errEmptyResult, message: "missing unit in duration",
		},
		{
			name: "a search depth of zero", code: http.StatusInternalServerError,
			body:    `{"error":{"httpCode":500,"message":"search depth must be greater than 0 and less than max traces"}}`,
			nothing: errEmptyResult, message: "search depth must be greater than 0",
		},
		{
			name: "a proxy in the way", code: http.StatusServiceUnavailable, body: "<html><body>503</body></html>",
			nothing: errEmptyResult, message: "503",
		},
		{
			name: "no body at all", code: http.StatusNotFound, body: "",
			nothing: errNotFound, message: "answered 404",
		},
		{
			// The envelope's shape with nothing in it is not the backend saying "No traces
			// found", so it is not read as having nothing.
			name: "an envelope with no message", code: http.StatusNotFound, body: `{"error":{"httpCode":404}}`,
			nothing: errEmptyResult, message: "answered 404",
		},
		{
			name: "a trace ID longer than 32 characters", code: http.StatusBadRequest,
			body:    `{"error":{"httpCode":400,"message":"TraceID cannot be longer than 32 hex characters"}}`,
			nothing: errNotFound, message: "cannot be longer than 32",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyHTTPStatus(tc.code, []byte(tc.body), tc.nothing)

			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
				return
			}
			if tc.message == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			// A failure is neither of the two ways of having nothing.
			assert.NotErrorIs(t, err, errEmptyResult)
			assert.NotErrorIs(t, err, errNotFound)
			assert.Contains(t, err.Error(), tc.message)
		})
	}
}

// The gRPC half. Jaeger 2.21 answers GetTrace with codes.NotFound for an ID the store does not
// hold; 2.17 and 2.20 answer it, and every version an unmatched FindTraces, with OK and no message
// at all, so those never reach here (TestEmptyGRPCStreamIsNoTrace covers them).
func TestGRPCErrorPolicy(t *testing.T) {
	cases := []struct {
		err     error
		message string
		name    string
		nothing error
		want    error
	}{
		{name: "a call that answered", nothing: errNotFound},
		{name: "an unknown trace ID", err: status.Error(codes.NotFound, "trace not found"), nothing: errNotFound, want: errNotFound},
		{
			name: "a query that timed out", err: status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			nothing: errNotFound, message: "context deadline exceeded",
		},
		{
			name: "a backend that is not there", err: status.Error(codes.Unavailable, "connection refused"),
			nothing: errEmptyResult, message: "connection refused",
		},
		{
			name: "a request api_v3 refused", err: status.Error(codes.InvalidArgument, "start time min and max are required parameters"),
			nothing: errEmptyResult, message: "start time min and max are required",
		},
		{
			// Every version answers a malformed ID with Unknown, not InvalidArgument, and it
			// is a fault of the request, not a trace that is missing.
			name: "a malformed trace ID", err: status.Error(codes.Unknown, "malform trace ID"),
			nothing: errNotFound, message: "malform trace ID",
		},
		{
			name:    "an unknown trace ID wrapped by a caller",
			err:     fmt.Errorf("GetTrace: %w", status.Error(codes.NotFound, "trace not found")),
			nothing: errNotFound, want: errNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyGRPCError(tc.err, tc.nothing)

			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
				return
			}
			if tc.message == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.NotErrorIs(t, err, errEmptyResult)
			assert.NotErrorIs(t, err, errNotFound)
			assert.Contains(t, err.Error(), tc.message)
		})
	}
}

// receiver replays a fixed stream, ending with err.
type receiver struct {
	err      error
	messages []*tracepb.TracesData
}

func (r *receiver) Recv() (*tracepb.TracesData, error) {
	if len(r.messages) == 0 {
		return nil, r.err
	}
	message := r.messages[0]
	r.messages = r.messages[1:]
	return message, nil
}

// The gRPC transport reads its stream through otlp.ReadStream, which wraps what Recv reports. The
// NotFound Jaeger 2.21 answers an unknown trace with has to survive that wrap, or the trace detail
// turns from a 404 into a 500 on exactly the version that says "not found" most plainly.
func TestNotFoundSurvivesTheStreamReader(t *testing.T) {
	assert := assert.New(t)

	traces, err := otlp.ReadStream(context.Background(), &receiver{err: status.Error(codes.NotFound, "trace not found")})
	assert.Nil(traces)
	assert.Equal(codes.NotFound, status.Code(err))

	single, err := traceResponse(traces, classifyGRPCError(err, errNotFound))
	assert.NoError(err)
	assert.Nil(single)
}

// Jaeger 2.17 and 2.20 answer an unknown trace ID over gRPC with OK and an empty stream, where
// 2.21 answers NotFound and HTTP answers 404. All of them have to reach the handler as the same
// (nil, nil), so the browser sees one answer for one question whichever transport carried it.
func TestEmptyGRPCStreamIsNoTrace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	traces, err := otlp.ReadStream(context.Background(), &receiver{err: io.EOF})
	require.NoError(err)

	single, err := traceResponse(traces, classifyGRPCError(err, errNotFound))
	assert.NoError(err)
	assert.Nil(single)

	// The same empty stream answering a search is an empty list naming the service.
	response, err := searchResponse("waypoint.tracegen", traces, classifyGRPCError(err, errEmptyResult))
	require.NoError(err)
	require.NotNil(response)
	assert.NotNil(response.Data)
	assert.Empty(response.Data)
	assert.Equal("waypoint.tracegen", response.TracingServiceName)
}

// The HTTP 404 envelope for an unknown trace reaches the handler as the same (nil, nil).
func TestHTTPNotFoundIsNoTrace(t *testing.T) {
	single, err := traceResponse(nil, classifyHTTPStatus(http.StatusNotFound, []byte(noTracesBody), errNotFound))
	assert.NoError(t, err)
	assert.Nil(t, single)
}

// A backend that is not api_v3 has to be named as such, with the minimum version, rather than
// reaching the user as a bare status or as an empty list. The cause stays reachable, so a gRPC
// status code can still be read through it.
func TestNotAPIv3Error(t *testing.T) {
	assert := assert.New(t)

	cause := status.Error(codes.Unimplemented, "unknown service jaeger.api_v3.QueryService")
	err := notAPIv3Error("tracing.istio-system:16685", cause)

	assert.Equal("tracing.istio-system:16685 does not serve Jaeger api_v3 (Jaeger 1.54 or newer is required, or the URL/base path is wrong): rpc error: code = Unimplemented desc = unknown service jaeger.api_v3.QueryService", err.Error())
	assert.ErrorIs(err, cause)
	assert.Equal(codes.Unimplemented, status.Code(err))
}

func TestWrongTransportError(t *testing.T) {
	assert := assert.New(t)

	cause := errors.New(`net/http: HTTP/1.x transport connection broken: malformed HTTP response "\x00\x00\x06\x04"`)
	err := wrongTransportError("http://tracing.istio-system:16685/api/v3/services", cause)

	assert.Contains(err.Error(), "http://tracing.istio-system:16685/api/v3/services")
	assert.Contains(err.Error(), "wrong port or transport")
	assert.Contains(err.Error(), "use_grpc")
	assert.ErrorIs(err, cause)
}
