package jaeger

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The two ways api_v3 says it has nothing. They are separate sentinels because the answers
// differ: an unknown trace is a 404 to the browser, while an unmatched search is an empty list.
var (
	errEmptyResult = errors.New("no trace matched the query")
	errNotFound    = errors.New("the trace is not in the store")
)

// apiv3Error is the envelope api_v3 answers a failed HTTP request with.
type apiv3Error struct {
	Error struct {
		HTTPCode int    `json:"httpCode"`
		Message  string `json:"message"`
	} `json:"error"`
}

// bodySnippet bounds how much of an unreadable body reaches a log line.
const bodySnippet = 256

// classifyHTTPStatus maps an api_v3 HTTP response onto the error policy. nothing is the sentinel
// standing for the backend's own "No traces found": a search and a detail are answered with the
// same status and the same body, so only the caller knows whether nothing means an unknown trace
// or an unmatched search.
//
// A 404 means that only when it carries the api_v3 error envelope. A mistyped path answers 404
// with the plain text "404 page not found", and reading that as a result would turn a
// misconfigured URL into a backend that holds no traces. The content type is never consulted:
// Jaeger 2.17.0 labels its api_v3 successes text/plain, and every version labels its errors so.
func classifyHTTPStatus(code int, body []byte, nothing error) error {
	if code == http.StatusOK {
		return nil
	}

	var envelope apiv3Error
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		if code == http.StatusNotFound {
			return nothing
		}
		return fmt.Errorf("Jaeger api_v3 answered %d: %s", code, envelope.Error.Message)
	}

	if len(body) > bodySnippet {
		body = body[:bodySnippet]
	}
	return fmt.Errorf("Jaeger api_v3 answered %d: %s", code, body)
}

// classifyGRPCError maps an api_v3 gRPC error onto the same policy. Jaeger 2.21 answers
// codes.NotFound for a trace ID the store does not hold; 2.17 and 2.20 answer it, and every
// version answers an unmatched search, with OK and no message at all, so those never reach here.
// Everything else is wrapped with %w so the caller can still read the status code.
func classifyGRPCError(err error, nothing error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.NotFound {
		return nothing
	}
	return fmt.Errorf("Jaeger api_v3 gRPC error: %w", err)
}

// notAPIv3Error reports an endpoint that answered, but not as api_v3: over HTTP a plain-text 404
// on an api_v3 route, a 200 whose body is not JSON (the Jaeger UI's HTML under a wrong base
// path), or a JSON body without the expected key; over gRPC codes.Unimplemented. Each of those
// would otherwise reach the user as an empty trace list or as a bare status code, where what they
// need to hear is that the backend is too old or the URL points somewhere else. where names the
// endpoint, so the message says what was probed.
func notAPIv3Error(where string, cause error) error {
	return fmt.Errorf("%s does not serve Jaeger api_v3 (Jaeger 1.54 or newer is required, or the URL/base path is wrong): %w", where, cause)
}

// wrongTransportError reports an endpoint that speaks the other protocol: gRPC dialled at an
// HTTP port fails reading the server preface ("frame too large"), and HTTP sent to a gRPC port
// reads a "malformed HTTP response". Neither error text names the cause, which is use_grpc not
// matching the port in the URL.
func wrongTransportError(where string, cause error) error {
	return fmt.Errorf("%s answered in another protocol, which means the wrong port or transport (check that use_grpc matches the port in the URL): %w", where, cause)
}

// HTTPWrongTransportError is wrongTransportError for an HTTP request that reached a gRPC port,
// and nil for a request that failed in any other way. Discovery words its HTTP probes through it,
// so that its log names the cause the client's own error names.
func HTTPWrongTransportError(where string, err error) error {
	if err == nil || !strings.Contains(err.Error(), "malformed HTTP response") {
		return nil
	}
	return wrongTransportError(where, err)
}
