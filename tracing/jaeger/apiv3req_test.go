package jaeger

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/models"
)

// Both renderings of a search are asserted against one expectation, because the failure this
// guards against is the two drifting apart: a gRPC query that filters and an HTTP query that
// does not look alike in review and answer differently.
type requestCase struct {
	attributes  map[string]string
	durationMin time.Duration // zero when the parameter must be omitted
	// durationParam is the HTTP rendering of durationMin.
	durationParam string
	name          string
	query         models.TracingQuery
	searchDepth   int32
	serviceName   string
	startTimeMax  string
	startTimeMin  string
}

var (
	windowStart = time.Date(2026, 10, 6, 14, 31, 8, 922775237, time.UTC)
	windowEnd   = time.Date(2026, 10, 6, 15, 31, 8, 0, time.UTC)
)

func requestCases() []requestCase {
	return []requestCase{
		{
			name:        "every field a Kiali query can carry",
			serviceName: "waypoint.tp-abc",
			query: models.TracingQuery{
				Start:       windowStart,
				End:         windowEnd,
				Tags:        map[string]string{models.IstioClusterTag: "cluster1", "error": "true"},
				MinDuration: 1500 * time.Microsecond,
				Limit:       100,
			},
			attributes:    map[string]string{models.IstioClusterTag: "cluster1", "error": "true"},
			startTimeMin:  "2026-10-06T14:31:08.922775237Z",
			startTimeMax:  "2026-10-06T15:31:08Z",
			durationMin:   1500 * time.Microsecond,
			durationParam: "0.001500000s",
			searchDepth:   100,
		},
		{
			// GetErrorTraces sends Limit 0, and Jaeger 2.17 and 2.20 refuse a depth of 0 or
			// none at all, so "no limit" is sent as the depth Jaeger itself defaults to.
			name:         "no tags, no duration, no limit",
			serviceName:  "waypoint.tp-abc",
			query:        models.TracingQuery{Start: windowStart, End: windowEnd},
			startTimeMin: "2026-10-06T14:31:08.922775237Z",
			startTimeMax: "2026-10-06T15:31:08Z",
			searchDepth:  defaultSearchDepth,
		},
		{
			name:         "a negative limit is no limit",
			serviceName:  "waypoint.tp-abc",
			query:        models.TracingQuery{Start: windowStart, End: windowEnd, Limit: -1},
			startTimeMin: "2026-10-06T14:31:08.922775237Z",
			startTimeMax: "2026-10-06T15:31:08Z",
			searchDepth:  100,
		},
		{
			name:        "a zero duration is not a filter",
			serviceName: "waypoint.tp-abc",
			query: models.TracingQuery{
				Start:       windowStart,
				End:         windowEnd,
				MinDuration: 0,
				Limit:       20,
			},
			startTimeMin: "2026-10-06T14:31:08.922775237Z",
			startTimeMax: "2026-10-06T15:31:08Z",
			searchDepth:  20,
		},
		{
			// The rendering keeps every nanosecond of a duration that has them.
			name:        "a duration with a nanosecond part",
			serviceName: "waypoint.tp-abc",
			query: models.TracingQuery{
				Start:       windowStart,
				End:         windowEnd,
				MinDuration: 2*time.Second + 1,
			},
			startTimeMin:  "2026-10-06T14:31:08.922775237Z",
			startTimeMax:  "2026-10-06T15:31:08Z",
			durationMin:   2*time.Second + 1,
			durationParam: "2.000000001s",
			searchDepth:   100,
		},
		{
			// Jaeger 2.21's HTTP gateway refuses a depth above 10000 with a 400, which is the
			// ceiling models.MaxTracingLimit names as well, so a caller that asks for more is
			// clamped rather than refused.
			name:         "a limit beyond what api_v3 accepts",
			serviceName:  "waypoint.tp-abc",
			query:        models.TracingQuery{Start: windowStart, End: windowEnd, Limit: 20000},
			startTimeMin: "2026-10-06T14:31:08.922775237Z",
			startTimeMax: "2026-10-06T15:31:08Z",
			searchDepth:  models.MaxTracingLimit,
		},
		{
			name:         "a window with no sub-second part",
			serviceName:  "waypoint.tp-abc",
			query:        models.TracingQuery{Start: windowEnd, End: windowEnd.Add(time.Hour), Limit: 5},
			startTimeMin: "2026-10-06T15:31:08Z",
			startTimeMax: "2026-10-06T16:31:08Z",
			searchDepth:  5,
		},
		{
			// A window outside UTC is sent in UTC, to the same instant.
			name:         "a window in another zone",
			serviceName:  "waypoint.tp-abc",
			query:        models.TracingQuery{Start: windowStart.In(time.FixedZone("CEST", 2*60*60)), End: windowEnd, Limit: 5},
			startTimeMin: "2026-10-06T14:31:08.922775237Z",
			startTimeMax: "2026-10-06T15:31:08Z",
			searchDepth:  5,
		},
		{
			// The narrowest window api_v3 accepts, which is what keeps the refusal of an
			// empty one from refusing a real search as well.
			name:         "a window one nanosecond wide",
			serviceName:  "waypoint.tp-abc",
			query:        models.TracingQuery{Start: windowStart, End: windowStart.Add(time.Nanosecond), Limit: 5},
			startTimeMin: "2026-10-06T14:31:08.922775237Z",
			startTimeMax: "2026-10-06T14:31:08.922775238Z",
			searchDepth:  5,
		},
	}
}

func TestFindTracesProtoRequest(t *testing.T) {
	for _, tc := range requestCases() {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			tq, err := newTraceQuery(tc.serviceName, tc.query)
			require.NoError(err)
			params := tq.proto().GetQuery()
			require.NotNil(params)

			assert.Equal(tc.serviceName, params.GetServiceName())
			assert.Equal(tc.attributes, params.GetAttributes())
			assert.Equal(tc.startTimeMin, params.GetStartTimeMin().AsTime().Format(time.RFC3339Nano))
			assert.Equal(tc.startTimeMax, params.GetStartTimeMax().AsTime().Format(time.RFC3339Nano))
			assert.Equal(tc.searchDepth, params.GetSearchDepth())

			if tc.durationMin == 0 {
				assert.Nil(params.GetDurationMin())
			} else {
				require.NotNil(params.GetDurationMin())
				assert.Equal(tc.durationMin, params.GetDurationMin().AsDuration())
			}
		})
	}
}

func TestFindTracesHTTPQuery(t *testing.T) {
	for _, tc := range requestCases() {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			tq, err := newTraceQuery(tc.serviceName, tc.query)
			require.NoError(err)
			values, err := tq.values()
			require.NoError(err)

			assert.Equal(tc.serviceName, values.Get("query.service_name"))
			assert.Equal(tc.startTimeMin, values.Get("query.start_time_min"))
			assert.Equal(tc.startTimeMax, values.Get("query.start_time_max"))
			assert.Equal(strconv.Itoa(int(tc.searchDepth)), values.Get("query.num_traces"))
			assert.Equal(tc.durationParam, values.Get("query.duration_min"))
			if tc.durationParam != "" {
				// The gateway parses it with Go's ParseDuration.
				parsed, err := time.ParseDuration(tc.durationParam)
				require.NoError(err)
				assert.Equal(tc.durationMin, parsed)
			}

			if tc.attributes == nil {
				assert.NotContains(values, "query.attributes")
				return
			}
			var attributes map[string]string
			require.NoError(json.Unmarshal([]byte(values.Get("query.attributes")), &attributes))
			assert.Equal(tc.attributes, attributes)
		})
	}
}

// Jaeger 2.17.0 refuses the camelCase window parameters with a 400 and silently ignores the
// camelCase duration and query.search_depth, and on 2.20 and later query.search_depth overrides
// query.num_traces. So the query string carries exactly the snake_case keys and nothing else.
func TestFindTracesHTTPQuerySendsOnlySnakeCaseKeys(t *testing.T) {
	for _, tc := range requestCases() {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			tq, err := newTraceQuery(tc.serviceName, tc.query)
			require.NoError(err)
			values, err := tq.values()
			require.NoError(err)

			allowed := map[string]bool{
				"query.attributes":     true,
				"query.duration_min":   true,
				"query.num_traces":     true,
				"query.service_name":   true,
				"query.start_time_max": true,
				"query.start_time_min": true,
			}
			for key, sent := range values {
				assert.True(allowed[key], "unexpected parameter %q", key)
				assert.Len(sent, 1, "parameter %q sent more than once", key)
				assert.Equal(strings.ToLower(key), key, "parameter %q is not snake_case", key)
			}
			assert.NotContains(values, "query.search_depth")
			assert.NotContains(values, "query.searchDepth")
		})
	}
}

// The attributes key order is fixed, so the same tags always make the same URL.
func TestAttributesRenderWithSortedKeys(t *testing.T) {
	require := require.New(t)

	tq, err := newTraceQuery("waypoint.tp-abc", models.TracingQuery{
		Start: windowStart,
		End:   windowEnd,
		Tags:  map[string]string{"z": "1", models.IstioClusterTag: "Kubernetes", "error": "true", "a": "2"},
	})
	require.NoError(err)
	for range 20 {
		values, err := tq.values()
		require.NoError(err)
		require.Equal(`{"a":"2","error":"true","istio.cluster_id":"Kubernetes","z":"1"}`, values.Get("query.attributes"))
	}
}

// The attributes go out as one parameter holding a JSON map. The bracket spelling
// query.attributes[key]=value is accepted and ignored by Jaeger 2.17.0, 2.20.0 and 2.21.0, so a
// search filtered that way answers with every trace of the service, where the one-parameter form
// filters on 2.20.0 and later.
func TestAttributesAreOneJSONParameter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tq, err := newTraceQuery("waypoint.tp-abc", models.TracingQuery{
		Start: windowStart,
		End:   windowEnd,
		Tags:  map[string]string{models.IstioClusterTag: "cluster1"},
	})
	require.NoError(err)
	values, err := tq.values()
	require.NoError(err)

	require.Len(values["query.attributes"], 1)
	assert.Equal(`{"istio.cluster_id":"cluster1"}`, values.Get("query.attributes"))

	encoded := values.Encode()
	assert.NotContains(encoded, "query.attributes%5B")
	assert.Contains(encoded, "query.attributes="+url.QueryEscape(`{"istio.cluster_id":"cluster1"}`))
}

// Every tag reaches both renderings whole, whatever is in it: a query that drops them is not
// narrower but answers with every trace of the service, from every cluster.
//
// json.Marshal of a string map cannot fail, so values() returning the encoding error rather
// than logging it is a shape the test cannot trigger; what it can pin is that the encoding is
// never hand-built, which is where a value carrying a quote or a brace would break it.
func TestEveryTagReachesBothRenderings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tags := map[string]string{
		models.IstioClusterTag: "cluster1",
		"error":                "true",
		"http.route":           `/api/{id}?q="x"`,
		"peer.address":         "10.244.0.1:15008",
		"k8s.pod.name":         "podinfo-7d8f9c-ünïcode",
	}
	tq, err := newTraceQuery("waypoint.tp-abc", models.TracingQuery{Start: windowStart, End: windowEnd, Tags: tags})
	require.NoError(err)

	assert.Equal(tags, tq.proto().GetQuery().GetAttributes())

	values, err := tq.values()
	require.NoError(err)
	var decoded map[string]string
	require.NoError(json.Unmarshal([]byte(values.Get("query.attributes")), &decoded))
	assert.Equal(tags, decoded)
}

// The tags belong to the caller, which keeps adding to them between queries.
func TestAttributesAreCopiedFromTheQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tags := map[string]string{models.IstioClusterTag: "cluster1"}
	tq, err := newTraceQuery("waypoint.tp-abc", models.TracingQuery{Start: windowStart, End: windowEnd, Tags: tags})
	require.NoError(err)

	tags["error"] = "true"
	assert.Equal(map[string]string{models.IstioClusterTag: "cluster1"}, tq.proto().GetQuery().GetAttributes())
}

// A query api_v3 cannot express is refused here rather than sent, because no backend refuses all
// three and no two of them refuse one alike. Each case carries what the three lab Jaegers
// measurably answer it, and the error has to name the field that was wrong: a guard that refuses
// every malformed query with one message leaves the caller nothing to act on, and the three
// faults have nothing in common beyond being unsearchable.
func TestFindTracesRefusesAQueryItCannotExpress(t *testing.T) {
	cases := map[string]struct {
		// because is the measured backend answer the guard stands in for.
		because string
		// names is what the refusal has to say, so the caller learns which field to fix.
		names       string
		query       models.TracingQuery
		serviceName string
	}{
		"no service name": {
			serviceName: "",
			query:       models.TracingQuery{Start: windowStart, End: windowEnd},
			names:       "a service name is required",
			because:     "every version answers it with every service's traces, on both transports",
		},
		"no start": {
			serviceName: "waypoint.tp-abc",
			query:       models.TracingQuery{End: windowEnd},
			names:       "a start and an end are required",
			because:     "a missing bound renders as the year 1, which is the zero-width window below",
		},
		"no end": {
			serviceName: "waypoint.tp-abc",
			query:       models.TracingQuery{Start: windowStart},
			names:       "a start and an end are required",
			because:     "a missing bound renders as the year 1, which is the zero-width window below",
		},
		"no window": {
			serviceName: "waypoint.tp-abc",
			query:       models.TracingQuery{},
			names:       "a start and an end are required",
			because:     "both bounds render as the year 1: 2.17.0 over HTTP answers 200 with every trace in the store, while gRPC answers Unknown on 2.17.0 and InvalidArgument later",
		},
		"an empty window": {
			serviceName: "waypoint.tp-abc",
			query:       models.TracingQuery{Start: windowEnd, End: windowEnd},
			names:       "must be before",
			because:     "2.17.0 answers 404 \"No traces found\" over HTTP, 2.20.0 and 2.21.0 answer 400, and every version answers OK with no message over gRPC",
		},
		"an inverted window": {
			serviceName: "waypoint.tp-abc",
			query:       models.TracingQuery{Start: windowEnd, End: windowStart},
			names:       "must be before",
			because:     "2.17.0 answers 404 carrying the api_v3 empty-result envelope, so the user is told there are no traces, and every version answers OK with no message over gRPC",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tq, err := newTraceQuery(tc.serviceName, tc.query)
			require.Error(t, err, "this query has to be refused here: %s", tc.because)
			assert.Nil(t, tq)
			assert.ErrorContains(t, err, tc.names,
				"and the refusal has to name the field, because the three faults share no fix")
		})
	}
}

// api_v3 takes a trace ID as hex text on both transports, where api_v2 took 16 bytes.
func TestGetTraceRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const traceID = "d7be8dde0afcd12af9ab0e6a87cd2acb"
	request, err := getTraceRequest(traceID)
	require.NoError(err)
	assert.Equal(traceID, request.GetTraceId())

	route, err := tracePath(traceID)
	require.NoError(err)
	assert.Equal("/api/v3/traces/"+traceID, route)

	// A 64-bit ID is as legal as a 128-bit one.
	short, err := getTraceRequest("f9ab0e6a87cd2acb")
	require.NoError(err)
	assert.Equal("f9ab0e6a87cd2acb", short.GetTraceId())
}

func TestGetTraceRefusesSomethingThatIsNotATraceID(t *testing.T) {
	for _, traceID := range []string{"", "zzzz", "d7be8dde0afcd12af9ab0e6a87cd2acbd7", "../services", "d7be8dde/../.."} {
		t.Run(traceID, func(t *testing.T) {
			_, err := getTraceRequest(traceID)
			assert.Error(t, err)

			route, err := tracePath(traceID)
			assert.Error(t, err)
			assert.Empty(t, route)
		})
	}
}
