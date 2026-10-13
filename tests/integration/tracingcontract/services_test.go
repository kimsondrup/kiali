package tracingcontract

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/kiali/kiali/tracing/jaeger"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
)

// The two routes that answer "which services have traces": api_v3's, which Kiali reads,
// and the pre-api_v3 one, which a configuration pointed at the wrong API would reach.
const (
	apiV3ServicesPath       = "/api/v3/services"
	unversionedServicesPath = "/api/services"
)

// TestServiceListShapes pins the api_v3 service list and the decoder Kiali reads it with.
//
// The service list is Kiali's health probe and the list behind the trace configuration
// page, and it is where a store speaking another API is least likely to say so: the
// pre-api_v3 route answers the same names under a different key, with a 200.
func TestServiceListShapes(t *testing.T) {
	for _, store := range requireJaegerHTTP(t) {
		t.Run(store.version, func(t *testing.T) {
			v3 := get(t, store.addr, apiV3ServicesPath, nil)
			require.Equal(t, http.StatusOK, v3.status, "api_v3 serves its own service list on every version: %s", v3)

			t.Run("api_v3 answers one services key", func(t *testing.T) {
				assert.Equal(t, []string{"services"}, topLevelKeys(t, v3),
					"the api_v3 body is this one key: %s", v3)

				var response apiv3.GetServicesResponse
				require.NoError(t, protojson.Unmarshal(v3.body, &response),
					"the api_v3 body must decode into the generated response type: %s", v3)
				assert.Contains(t, response.GetServices(), service,
					"the seeded service must be in the list, or an empty decode here would be indistinguishable from a wrong shape")
			})

			t.Run("Kiali's decoder reads the names the generated type reads", func(t *testing.T) {
				var response apiv3.GetServicesResponse
				require.NoError(t, protojson.Unmarshal(v3.body, &response))

				decoded, err := jaeger.DecodeServices(v3.body)
				require.NoError(t, err, "%s", v3)
				assert.ElementsMatch(t, response.GetServices(), decoded)
			})

			t.Run("the unversioned route", func(t *testing.T) {
				got := get(t, store.addr, unversionedServicesPath, nil)

				if !store.caps.unversionedServices {
					require.Equal(t, http.StatusNotFound, got.status,
						"Jaeger %s has removed %s: %s", store.version, unversionedServicesPath, got)
					assert.Equal(t, "404 page not found", strings.TrimSpace(string(got.body)),
						"the router answers in plain text and not in the api_v3 error envelope: %s", got)
					return
				}

				require.Equal(t, http.StatusOK, got.status, "%s", got)
				assert.Equal(t, []string{"data", "errors", "limit", "offset", "total"}, topLevelKeys(t, got),
					"the unversioned body wraps the names in its own envelope and shares no key with api_v3's: %s", got)

				// A configuration whose base path reaches this route instead of api_v3's
				// must be refused by name rather than read as a store with no services.
				_, err := jaeger.DecodeServices(got.body)
				require.Error(t, err, "Kiali's decoder must refuse the pre-api_v3 body: %s", got)
				assert.Contains(t, err.Error(), "/api/services")
			})
		})
	}
}

// topLevelKeys reports the object keys of a JSON body, sorted.
func topLevelKeys(t *testing.T, got answer) []string {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got.body, &object), "the body is not a JSON object: %s", got)
	var keys []string
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
