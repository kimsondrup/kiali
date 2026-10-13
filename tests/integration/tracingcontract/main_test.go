// Package tracingcontract pins the backend contract Kiali's tracing clients
// depend on, asserted against live trace stores rather than fixtures.
//
// A fixture records what a backend answered once; these tests fail the moment a
// Jaeger or Tempo upgrade changes what Kiali relies on, which is the failure
// that is otherwise found by a user whose trace list went empty.
//
// Every test is gated on an environment variable naming a reachable store and
// skips with a named reason when it is unset, so `go test ./...` with no trace
// store is green and silent.
//
// The package must not import tests/integration/utils/kiali: that package
// builds its client in a package-level var and calls log.Fatalf when $URL is
// unset, so merely linking it aborts the binary before any test runs.
package tracingcontract

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
)

// The environment the suite reads. The two Jaeger variables carry version=address
// pairs, because every assertion here is version-specific: a contract test that
// asserts the lowest common denominator passes on the backend Istio ships while
// Kiali is broken against it.
const (
	envClusterID  = "KIALI_CONTRACT_CLUSTER_ID"
	envJaegerHTTP = "KIALI_CONTRACT_JAEGER_HTTP"
	envJaegerGRPC = "KIALI_CONTRACT_JAEGER_GRPC"
	envTempoHTTP  = "KIALI_CONTRACT_TEMPO_HTTP"
	envTempoGRPC  = "KIALI_CONTRACT_TEMPO_GRPC"
	envService    = "KIALI_CONTRACT_SERVICE"
)

// defaultClusterID is the istio.cluster_id istiod stamps on spans when the mesh sets no
// cluster name, which is what the tracing lab runs. CI meshes set their own, so the
// value is a parameter: a cluster filter carrying a value no span has empties the trace
// list on every version except 2.17.0 over HTTP, which ignores the filter, so a wrong
// default would make the attribute rows assert the opposite of what they mean.
const defaultClusterID = "Kubernetes"

// searchPath is the api_v3 HTTP search route, repeated here rather than imported:
// this suite pins what the backend serves, so it must not move when Kiali does.
const searchPath = "/api/v3/traces"

// seedDepth bounds every search that does not set its own depth. It is above the
// number of traces the lab seeds, so a bounded search and an unbounded one
// answer alike and a count assertion is about the filter under test rather than
// about the depth.
//
// The margin is wide on purpose. A depth the seeded window reaches truncates
// the baseline, and then a filtered search can answer a trace the baseline does
// not carry, so a set comparison between the two fails for no reason but the
// depth. It stays below every bound a store imposes: the lowest ceiling any
// version validates is models.MaxTracingLimit, and the smallest store in the
// lab holds 5000 traces.
const seedDepth = "1000"

// capabilities carries the divergences between Jaeger versions as booleans, so a
// row of the test table states what a version does instead of describing it.
// Every field is measured, and an unmeasured version has no row at all rather
// than a guessed one.
type capabilities struct {
	// apiV3ContentType is what the version labels its own api_v3 answers with,
	// search and service list alike. It is not application/json everywhere, so
	// a reader that gates on that type rejects the Jaeger Istio ships while
	// accepting the newer ones.
	apiV3ContentType string

	// camelCaseDurationMin is whether the HTTP gateway reads
	// query.durationMin. Where it does not the parameter is inert: the search
	// answers 200 with every trace in the window, so a minimum-duration filter
	// silently widens instead of failing.
	camelCaseDurationMin bool

	// camelCaseSearchParams is whether the HTTP gateway reads
	// query.serviceName / query.startTimeMin / query.startTimeMax. Where it does
	// not, the window parameters are simply absent and the search is a 400.
	camelCaseSearchParams bool

	// depthHardCeiling is the highest query.num_traces the version validates
	// against before the store is consulted, and 0 where the version imposes no
	// ceiling of its own.
	//
	// The depth has two independent bounds and both are refusals, never clamps.
	// Where the version has a ceiling, exceeding it is a 400 naming the
	// parameter. Where it has none, the store's own trace capacity bounds the
	// depth instead and exceeding that is a 500 naming the depth, so the bound
	// is a property of the deployment and not of the version: a Jaeger 2.20.0
	// configured with memory max_traces 5000 refuses a depth of 8000 that the
	// same version with a larger store accepts. Kiali's clamp to
	// models.MaxTracingLimit is therefore necessary and not sufficient.
	depthHardCeiling int

	// depthOptionalHTTP is whether the HTTP gateway accepts a search carrying no
	// depth at all. Where it does, the answer is the whole window.
	depthOptionalHTTP bool

	// grpcDepthOptional is whether api_v3 gRPC FindTraces accepts a request
	// carrying no search depth. Where it does not, the RPC is refused with
	// codes.Unknown, so the depth is mandatory over gRPC on backends whose HTTP
	// gateway happily omits it, and the default has to be set where both
	// renderings read it.
	grpcDepthOptional bool

	// grpcUnknownTraceNotFound is whether api_v3 gRPC GetTrace answers
	// codes.NotFound for an ID the store does not hold. Where it does not, the
	// answer is OK with no message at all, so no error reaches the classifier
	// and the empty stream is the only thing saying the trace is absent.
	grpcUnknownTraceNotFound bool

	// grpcZeroWindowCode is the status the gRPC service refuses a window of
	// zero width with. Every version refuses it, so the divergence is only the
	// code: 2.17.0 reports codes.Unknown, which a client cannot tell from a
	// backend failure, where the newer two name the argument.
	grpcZeroWindowCode codes.Code

	// httpAttributeFilter is whether the HTTP gateway honours query.attributes.
	// Where it does not, every spelling answers 200 with the unfiltered body.
	httpAttributeFilter bool

	// searchDepthParam is whether the HTTP gateway reads the depth as
	// query.search_depth. Where it does not, the depth is absent and the search
	// is a 500, because api_v3 requires a positive depth.
	searchDepthParam bool

	// unversionedServices is whether the version still serves the pre-api_v3
	// service list, /api/services. Where it does not, the router answers a
	// plain-text 404 with no api_v3 error envelope. 2.21.0 removed that route
	// but still answers the pre-api_v3 trace route, /api/traces/{id}, so this
	// is about the service list alone and says nothing about the other routes.
	unversionedServices bool

	// windowValidatedHTTP is whether the HTTP gateway refuses a window that is
	// inverted or empty. Where it does not, neither shape is reported: an
	// inverted window is a 404 carrying the api_v3 "No traces found" envelope,
	// which a classifier reads as a store holding nothing, and a window of zero
	// width is a 200 carrying every trace of the service with the window
	// ignored.
	windowValidatedHTTP bool
}

// jaegerCapabilities is the measured table. Keyed by the version string the
// address is labelled with, so a store under test declares which row applies.
var jaegerCapabilities = map[string]capabilities{
	// The version Istio 1.31's Jaeger add-on ships, and so the version CI lands
	// on. It is the strict outlier on every HTTP row below.
	"2.17.0": {
		apiV3ContentType:         "text/plain; charset=utf-8",
		camelCaseDurationMin:     false,
		camelCaseSearchParams:    false,
		depthHardCeiling:         0,
		depthOptionalHTTP:        false,
		grpcDepthOptional:        false,
		grpcUnknownTraceNotFound: false,
		grpcZeroWindowCode:       codes.Unknown,
		httpAttributeFilter:      false,
		searchDepthParam:         false,
		unversionedServices:      true,
		windowValidatedHTTP:      false,
	},
	"2.20.0": {
		apiV3ContentType:         "application/json",
		camelCaseDurationMin:     true,
		camelCaseSearchParams:    true,
		depthHardCeiling:         0,
		depthOptionalHTTP:        true,
		grpcDepthOptional:        false,
		grpcUnknownTraceNotFound: false,
		grpcZeroWindowCode:       codes.InvalidArgument,
		httpAttributeFilter:      true,
		searchDepthParam:         true,
		unversionedServices:      true,
		windowValidatedHTTP:      true,
	},
	"2.21.0": {
		apiV3ContentType:         "application/json",
		camelCaseDurationMin:     true,
		camelCaseSearchParams:    true,
		depthHardCeiling:         models.MaxTracingLimit,
		depthOptionalHTTP:        true,
		grpcDepthOptional:        true,
		grpcUnknownTraceNotFound: true,
		grpcZeroWindowCode:       codes.InvalidArgument,
		httpAttributeFilter:      true,
		searchDepthParam:         true,
		unversionedServices:      false,
		windowValidatedHTTP:      true,
	},
}

// jaegerStore is one configured Jaeger, with the capability row its version
// declares.
type jaegerStore struct {
	addr    string
	caps    capabilities
	version string
}

var (
	// clusterID is the istio.cluster_id value the seeded spans carry, from
	// KIALI_CONTRACT_CLUSTER_ID and defaulting to defaultClusterID.
	clusterID string

	// service is the span service.name every search narrows to. Required
	// whenever a store is configured: with the wrong name every Jaeger answers
	// 404 "No traces found" and Tempo answers 200 {"traces":[]}, so a suite that
	// defaulted it would pass against an empty answer.
	service string

	jaegerHTTP []jaegerStore
	jaegerGRPC []jaegerStore
	tempoHTTP  string
	tempoGRPC  string

	// windowMin and windowMax bound every search, fixed once so two stores are
	// asked the same question. The end is in the future because spans batch
	// through the collector after the request that produced them.
	windowMin time.Time
	windowMax time.Time

	// grpcConns holds the connection per Jaeger address, dialled once up front.
	// A missing entry means the endpoint did not answer, which is a skip and not
	// a failure: a kubectl port-forward to a gRPC port dies permanently on a
	// connection reset, and that is a lab fault, not a contract change.
	grpcConns map[string]*grpc.ClientConn
	tempoConn *grpc.ClientConn
)

func TestMain(m *testing.M) {
	var err error
	if jaegerHTTP, err = parseStores(os.Getenv(envJaegerHTTP)); err != nil {
		fail("%s: %v", envJaegerHTTP, err)
	}
	if jaegerGRPC, err = parseStores(os.Getenv(envJaegerGRPC)); err != nil {
		fail("%s: %v", envJaegerGRPC, err)
	}
	tempoHTTP = os.Getenv(envTempoHTTP)
	tempoGRPC = os.Getenv(envTempoGRPC)
	service = os.Getenv(envService)
	if clusterID = strings.TrimSpace(os.Getenv(envClusterID)); clusterID == "" {
		clusterID = defaultClusterID
	}

	configured := len(jaegerHTTP) > 0 || len(jaegerGRPC) > 0 || tempoHTTP != "" || tempoGRPC != ""
	if configured && service == "" {
		// Not a skip. A store with no service name cannot be asked a question
		// whose empty answer is distinguishable from a broken one.
		fail("%s is set but %s is not: every search here narrows to one service.name, and a search without it cannot tell an empty store from a wrong name", envJaegerHTTP, envService)
	}

	now := time.Now()
	windowMin = now.Add(-1 * time.Hour)
	windowMax = now.Add(1 * time.Minute)

	grpcConns = map[string]*grpc.ClientConn{}
	for _, store := range jaegerGRPC {
		if conn, dialErr := dialJaeger(store.addr); dialErr == nil {
			grpcConns[store.addr] = conn
		} else {
			fmt.Fprintf(os.Stderr, "tracingcontract: Jaeger %s gRPC at %s did not answer, its subtests will skip: %v\n", store.version, store.addr, dialErr)
		}
	}
	if tempoGRPC != "" {
		if conn, dialErr := grpc.NewClient(tempoGRPC, grpc.WithTransportCredentials(insecure.NewCredentials())); dialErr == nil {
			tempoConn = conn
		} else {
			fmt.Fprintf(os.Stderr, "tracingcontract: Tempo gRPC at %s could not be dialled, its subtests will skip: %v\n", tempoGRPC, dialErr)
		}
	}

	code := m.Run()

	for _, conn := range grpcConns {
		_ = conn.Close()
	}
	if tempoConn != nil {
		_ = tempoConn.Close()
	}
	os.Exit(code)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "tracingcontract: "+format+"\n", args...)
	os.Exit(1)
}

// parseStores reads a comma-separated list of version=address pairs. A version
// with no capability row is refused here rather than at the first assertion, so
// the message says what is missing.
func parseStores(value string) ([]jaegerStore, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var stores []jaegerStore
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		version, addr, found := strings.Cut(entry, "=")
		if !found || version == "" || addr == "" {
			return nil, fmt.Errorf("%q is not a version=address pair", entry)
		}
		caps, known := jaegerCapabilities[version]
		if !known {
			return nil, fmt.Errorf("no capability row for Jaeger %q: measure it and add one, because every assertion here is version-specific", version)
		}
		stores = append(stores, jaegerStore{addr: addr, caps: caps, version: version})
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i].version < stores[j].version })
	return stores, nil
}

// dialJaeger proves the endpoint speaks api_v3 gRPC before any test depends on
// it, with the one unary RPC the service has.
func dialJaeger(addr string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := apiv3.NewQueryServiceClient(conn).GetServices(ctx, &apiv3.GetServicesRequest{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// requireJaegerHTTP returns the configured Jaegers, or skips with the reason.
func requireJaegerHTTP(t *testing.T) []jaegerStore {
	t.Helper()
	if len(jaegerHTTP) == 0 {
		t.Skipf("%s is unset: no live Jaeger to pin the api_v3 HTTP contract against", envJaegerHTTP)
	}
	return jaegerHTTP
}

// requireJaegerGRPC returns the configured Jaegers, or skips with the reason.
func requireJaegerGRPC(t *testing.T) []jaegerStore {
	t.Helper()
	if len(jaegerGRPC) == 0 {
		t.Skipf("%s is unset: no live Jaeger to pin the api_v3 gRPC contract against", envJaegerGRPC)
	}
	return jaegerGRPC
}

// queryClient returns the api_v3 gRPC client for a store, or skips this subtest
// alone, so a dead port-forward on one transport does not weaken the other.
func queryClient(t *testing.T, store jaegerStore) apiv3.QueryServiceClient {
	t.Helper()
	conn, reachable := grpcConns[store.addr]
	if !reachable {
		t.Skipf("Jaeger %s gRPC endpoint %s not reachable: dialled once at startup and it did not answer GetServices", store.version, store.addr)
	}
	return apiv3.NewQueryServiceClient(conn)
}

// searchParams is the window every search carries, in the spelling every
// supported Jaeger reads.
func searchParams() url.Values {
	v := url.Values{}
	v.Set("query.service_name", service)
	v.Set("query.start_time_min", formatTime(windowMin))
	v.Set("query.start_time_max", formatTime(windowMax))
	return v
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// answer is one HTTP response, kept whole: these tests assert on the body size
// and the trace count as often as on the status, because a broken parameter
// answers 200 with the unfiltered body and a status-only assertion passes on it.
type answer struct {
	body        []byte
	contentType string
	status      int

	// transferEncoding is what the response declared, so a test can tell a
	// streamed body from one whose length was known up front.
	transferEncoding []string
}

func (a answer) String() string {
	body := a.body
	if len(body) > 160 {
		body = body[:160]
	}
	return fmt.Sprintf("status %d, %d bytes, content-type %q, body %s", a.status, len(a.body), a.contentType, body)
}

// traceIDs reports the distinct trace IDs an api_v3 body carries. The count is
// what proves a filter narrowed: the body size alone cannot tell a narrower
// answer from a differently ordered one.
func (a answer) traceIDs(t *testing.T) []string {
	t.Helper()
	var envelope struct {
		Result struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						TraceID string `json:"traceId"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"result"`
	}
	if err := json.Unmarshal(a.body, &envelope); err != nil {
		t.Fatalf("api_v3 body is not readable as a search answer: %v (%s)", err, a)
	}
	seen := map[string]bool{}
	var ids []string
	for _, rs := range envelope.Result.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				if span.TraceID != "" && !seen[span.TraceID] {
					seen[span.TraceID] = true
					ids = append(ids, span.TraceID)
				}
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// get asks one store one question. Any transport fault is a failure and not a
// skip: the endpoint was reachable when the suite started.
func get(t *testing.T, addr, path string, params url.Values) answer {
	t.Helper()
	target := "http://" + addr + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("cannot build a request for %s: %v", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s did not answer: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s answered %d and the body could not be read: %v", target, resp.StatusCode, err)
	}
	return answer{
		body:             body,
		contentType:      resp.Header.Get("Content-Type"),
		status:           resp.StatusCode,
		transferEncoding: resp.TransferEncoding,
	}
}

// search runs an api_v3 HTTP search carrying the window plus whatever the caller
// adds.
func search(t *testing.T, store jaegerStore, extra url.Values) answer {
	t.Helper()
	params := searchParams()
	for key, values := range extra {
		params[key] = values
	}
	return get(t, store.addr, searchPath, params)
}

// seededQuery is the api_v3 gRPC search every test here starts from: the seeded
// service over the fixed window, at a depth above the seeded trace count.
func seededQuery() *apiv3.TraceQueryParameters {
	depth, _ := strconv.Atoi(seedDepth)
	return &apiv3.TraceQueryParameters{
		ServiceName:  service,
		StartTimeMin: timestamppb.New(windowMin),
		StartTimeMax: timestamppb.New(windowMax),
		SearchDepth:  int32(depth),
	}
}

// findTraces runs an api_v3 gRPC search and reports the distinct trace IDs and
// the number of stream messages that carried them.
//
// The IDs accumulate across the stream rather than being taken from the last
// message: FindTraces answers chunks, so a reader that keeps only the latest
// recovers one trace of however many were found, which is the framing
// stream_shape_test.go pins.
//
// The error is returned rather than asserted, because several tests here pin a
// refusal.
func findTraces(t *testing.T, client apiv3.QueryServiceClient, query *apiv3.TraceQueryParameters) (ids []string, messages int, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.FindTraces(ctx, &apiv3.FindTracesRequest{Query: query})
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	for {
		message, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			sort.Strings(ids)
			return ids, messages, nil
		}
		if recvErr != nil {
			return ids, messages, recvErr
		}
		messages++
		for _, rs := range message.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, span := range ss.GetSpans() {
					// api_v3 gRPC carries an ID as proto bytes, where the HTTP
					// gateway renders it as hex. Both are rendered hex here so
					// the two transports are comparable.
					id := hex.EncodeToString(span.GetTraceId())
					if id != "" && !seen[id] {
						seen[id] = true
						ids = append(ids, id)
					}
				}
			}
		}
	}
}

// jsonMap renders an attribute filter the way api_v3's HTTP gateway reads it: one parameter
// holding a URL-encoded JSON map.
func jsonMap(t *testing.T, attributes map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(attributes)
	if err != nil {
		t.Fatalf("cannot encode the attribute filter %v: %v", attributes, err)
	}
	return string(encoded)
}
