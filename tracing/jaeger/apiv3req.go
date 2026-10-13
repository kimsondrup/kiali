package jaeger

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
	"github.com/kiali/kiali/util"
)

// searchPath is the api_v3 HTTP search route, and the prefix a single trace hangs off.
const searchPath = "/api/v3/traces"

// defaultSearchDepth is the depth sent when the caller asks for no limit. GetErrorTraces sends
// Limit 0, and Jaeger 2.17 and 2.20 refuse a 0 or absent depth (gRPC Unknown "search depth must
// be greater than 0", HTTP 500). 100 is what Jaeger itself uses when the parameter is absent and
// it accepts that, so this asks every version for what 2.21 would have answered anyway.
const defaultSearchDepth = 100

// The HTTP query parameters of an api_v3 search. Every one is snake_case: Jaeger 2.17.0's gateway
// refuses the camelCase window parameters (400 "query.start_time_min and query.start_time_max are
// required") and silently ignores camelCase durationMin and operationName. The depth is
// query.num_traces, the only spelling 2.17.0 reads; query.search_depth is never sent, because
// 2.17.0 ignores it and on 2.20 and later it overrides num_traces. Measured on Jaeger 2.17.0,
// 2.20.0 and 2.21.0.
const (
	paramAttributes   = "query.attributes"
	paramDurationMin  = "query.duration_min"
	paramNumTraces    = "query.num_traces"
	paramServiceName  = "query.service_name"
	paramStartTimeMax = "query.start_time_max"
	paramStartTimeMin = "query.start_time_min"
)

// traceQuery is an api_v3 search, built once and rendered for whichever transport carries it.
// Both renderings read the same fields, which is the only thing that keeps the gRPC message and
// the HTTP query string saying the same thing.
type traceQuery struct {
	attributes   map[string]string
	durationMin  time.Duration
	searchDepth  int32
	serviceName  string
	startTimeMax time.Time
	startTimeMin time.Time
}

// newTraceQuery applies the rules api_v3 imposes on a search and reports a query that cannot be
// expressed as one. The three refusals are load-bearing, because the backend answers each of
// them rather than refusing it: a search with no service name answers with every service's
// traces; a zero window, which renders as the year 1, answers every trace in the store on 2.17.0
// over HTTP; and a window whose start is not before its end is a 404 "No traces found" on 2.17.0
// over HTTP, a 400 on later versions, and an OK carrying no messages over gRPC, which reads as a
// store holding no traces.
func newTraceQuery(serviceName string, q models.TracingQuery) (*traceQuery, error) {
	if serviceName == "" {
		return nil, errors.New("api_v3 search: a service name is required, because api_v3 answers a query without one with every service's traces")
	}
	if q.Start.IsZero() || q.End.IsZero() {
		return nil, errors.New("api_v3 search: a start and an end are required, because the two transports read a zero window differently")
	}
	if !q.Start.Before(q.End) {
		return nil, fmt.Errorf("api_v3 search: the start %s must be before the end %s, because the two transports read a window that is empty or inverted differently", formatTime(q.Start), formatTime(q.End))
	}

	tq := &traceQuery{
		searchDepth:  searchDepth(q.Limit),
		serviceName:  serviceName,
		startTimeMax: q.End,
		startTimeMin: q.Start,
	}
	// Copied: the caller keeps its map and adds to it between queries.
	if len(q.Tags) > 0 {
		tq.attributes = util.CopyStringMap(q.Tags)
	}
	if q.MinDuration > 0 {
		tq.durationMin = q.MinDuration
	}
	return tq, nil
}

// searchDepth is always positive, for the reason defaultSearchDepth gives, and never above
// models.MaxTracingLimit, which is also the ceiling Jaeger 2.21's HTTP gateway enforces with a 400.
// The default lives here rather than in the provider-neutral caller, because Tempo reads a zero
// limit its own way.
func searchDepth(limit int) int32 {
	if limit <= 0 {
		return defaultSearchDepth
	}
	return int32(min(limit, models.MaxTracingLimit))
}

// proto renders the search as the gRPC request.
func (tq *traceQuery) proto() *apiv3.FindTracesRequest {
	params := &apiv3.TraceQueryParameters{
		Attributes:   tq.attributes,
		SearchDepth:  tq.searchDepth,
		ServiceName:  tq.serviceName,
		StartTimeMax: timestamppb.New(tq.startTimeMax),
		StartTimeMin: timestamppb.New(tq.startTimeMin),
	}
	// A duration is set only when there is one to filter on, so the request says "no filter"
	// by leaving the field out rather than by a zero that happens to mean the same.
	if tq.durationMin > 0 {
		params.DurationMin = durationpb.New(tq.durationMin)
	}
	return &apiv3.FindTracesRequest{Query: params}
}

// values renders the search as the HTTP query string.
func (tq *traceQuery) values() (url.Values, error) {
	v := url.Values{}
	v.Set(paramServiceName, tq.serviceName)
	v.Set(paramStartTimeMin, formatTime(tq.startTimeMin))
	v.Set(paramStartTimeMax, formatTime(tq.startTimeMax))
	v.Set(paramNumTraces, strconv.Itoa(int(tq.searchDepth)))

	if len(tq.attributes) > 0 {
		// One parameter holding a JSON map; json.Marshal sorts a map's keys, so the URL is the
		// same for the same tags. The bracket spelling query.attributes[key]=value is accepted
		// and ignored by every version, which reads exactly like a backend that cannot filter.
		encoded, err := json.Marshal(tq.attributes)
		if err != nil {
			return nil, fmt.Errorf("api_v3 search: cannot encode the query attributes: %w", err)
		}
		v.Set(paramAttributes, string(encoded))
	}
	if tq.durationMin > 0 {
		// Seconds with nine decimals: Go's ParseDuration, which the gateway uses, and protojson's
		// Duration both read it, and it keeps every nanosecond. A bare number is a 400.
		v.Set(paramDurationMin, fmt.Sprintf("%.9fs", tq.durationMin.Seconds()))
	}
	return v, nil
}

// formatTime renders an instant the way api_v3 reads a Timestamp over HTTP, to the nanosecond.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// getTraceRequest builds the trace detail request. api_v3 takes the ID as hex text.
func getTraceRequest(traceID string) (*apiv3.GetTraceRequest, error) {
	if err := validTraceID(traceID); err != nil {
		return nil, err
	}
	return &apiv3.GetTraceRequest{TraceId: traceID}, nil
}

// tracePath builds the HTTP route of one trace, under the same validation: an ID that is not hex
// is a 400 from the backend, and the path is where it would be pasted unescaped.
func tracePath(traceID string) (string, error) {
	if err := validTraceID(traceID); err != nil {
		return "", err
	}
	return path.Join(searchPath, traceID), nil
}

// validTraceID refuses an ID before it is sent, because the backend answers a malformed one,
// or one longer than 32 characters, with a 400 over HTTP and with a gRPC Unknown that looks like
// a backend failure.
func validTraceID(traceID string) error {
	if !models.ValidTraceIDRe.MatchString(traceID) {
		return fmt.Errorf("api_v3 trace detail: %q is not a trace ID", traceID)
	}
	return nil
}
