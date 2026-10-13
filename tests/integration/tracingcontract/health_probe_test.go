package tracingcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/kiali/kiali/tracing/jaeger"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
)

// wrongBasePath is the api_v3 service list behind a base path these Jaegers do
// not have. It is not a hypothetical: Istio's add-on sets base_path /jaeger, so
// a configuration that carries the path to a Jaeger serving at the root — or
// omits it for one that does not — produces exactly this request.
const wrongBasePath = "/jaeger" + apiV3ServicesPath

// mistypedRoute is a path the router knows the shape of and does not have.
const mistypedRoute = "/api/v3/service"

// TestHealthProbeMustRefuse pins the three answers a probe has to tell apart,
// none of which the status code distinguishes on its own.
//
// Kiali's HTTP health probe decides whether to show a trace store as available.
// The wrong-base-path answer is the dangerous one: it is a 200, so a probe that
// reads the status reports a completely misconfigured store as healthy and
// every search then fails somewhere the user cannot see.
func TestHealthProbeMustRefuse(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			t.Run("a wrong base path is a 200 carrying the UI", func(t *testing.T) {
				got := get(t, store.addr, wrongBasePath, nil)
				require.Equal(t, http.StatusOK, got.status,
					"the Jaeger UI is a single-page app and catches every path it does not route, so a wrong base path answers 200: %s", got)
				assert.Contains(t, got.contentType, "text/html", "%s", got)
				assert.True(t, bytes.HasPrefix(got.body, []byte("<!doctype html>\n<html lang=\"en\">")),
					"the body is the UI's document and not a service list: %s", got)

				// What the probe must do instead of reading the status, and
				// the reason it is enough: no decoder can be talked into a
				// result by a document.
				var response apiv3.GetServicesResponse
				assert.Error(t, protojson.Unmarshal(got.body, &response),
					"the api_v3 decoder must refuse the UI document, which is the only thing that separates this from a healthy store")
				_, err := jaeger.DecodeServices(got.body)
				assert.Error(t, err, "so must the decoder Kiali's probe reads the service list with")
			})

			t.Run("a mistyped route is a 404 with no envelope", func(t *testing.T) {
				got := get(t, store.addr, mistypedRoute, nil)
				require.Equal(t, http.StatusNotFound, got.status, "%s", got)
				assert.Contains(t, got.contentType, "text/plain", "%s", got)
				assert.Equal(t, "404 page not found", strings.TrimSpace(string(got.body)), "%s", got)

				var envelope map[string]json.RawMessage
				assert.Error(t, json.Unmarshal(got.body, &envelope),
					"the router's 404 is not JSON at all, so it cannot be mistaken for the api_v3 404 that means the store holds nothing")
			})

			t.Run("a real api_v3 404 carries the envelope", func(t *testing.T) {
				// The same status as the row above and the opposite meaning.
				// The envelope is the whole of the difference, which is why a
				// classifier has to read the body of a 404 and not only its
				// status.
				params := searchParams()
				params.Set("query.service_name", absentService)
				params.Set("query.num_traces", seedDepth)
				got := get(t, store.addr, searchPath, params)
				require.Equal(t, http.StatusNotFound, got.status, "%s", got)
				assertEnvelope(t, got, http.StatusNotFound, "No traces found")
			})

			t.Run("api_v3 does not label its answers application/json everywhere", func(t *testing.T) {
				// The row nothing else in the suite covers. A probe or a
				// decoder that gates on the content type would reject the
				// Jaeger Istio ships while accepting the two newer ones, which
				// is the worst possible shape for a version check to have.
				services := get(t, store.addr, apiV3ServicesPath, nil)
				require.Equal(t, http.StatusOK, services.status, "%s", services)
				assert.Equal(t, store.caps.apiV3ContentType, services.contentType,
					"Jaeger %s labels its api_v3 service list %q: %s", store.version, store.caps.apiV3ContentType, services)

				search := search(t, store, map[string][]string{"query.num_traces": {seedDepth}})
				require.Equal(t, http.StatusOK, search.status, "%s", search)
				assert.Equal(t, store.caps.apiV3ContentType, search.contentType,
					"and labels its search the same way, so the type is the version's and not the route's: %s", search)
			})

			t.Run("a search answer is one JSON object however it is framed", func(t *testing.T) {
				// api_v3's streaming wrapper may send several objects in one
				// chunked body, and otlp.Decode reads a body as a sequence for
				// that reason. Every version in the lab sends one, so that
				// loop runs once; this row is what reports the day one of them
				// starts streaming and the second iteration begins to matter.
				got := search(t, store, map[string][]string{"query.num_traces": {seedDepth}})
				require.Equal(t, http.StatusOK, got.status, "%s", got)
				assert.Equal(t, []string{"chunked"}, got.transferEncoding,
					"the body is streamed, so its length is not known up front and the framing is all a reader has: %s", got)
				assert.Equal(t, 1, topLevelObjects(t, got),
					"the whole answer arrives as one top-level object: %s", got)
				assert.Equal(t, []string{"result"}, topLevelKeys(t, got),
					"wrapped under the one root key api_v3's gateway puts over the payload: %s", got)
			})
		})
	}
}

// topLevelObjects counts the JSON values a body holds end to end, which is what
// tells a single object from a stream of them.
func topLevelObjects(t *testing.T, got answer) int {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(got.body))
	count := 0
	for {
		var value json.RawMessage
		err := dec.Decode(&value)
		if errors.Is(err, io.EOF) {
			return count
		}
		require.NoError(t, err, "the body is not a sequence of JSON values: %s", got)
		count++
	}
}
