package querypb

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	allowFile           = "../../../hack/tempo-querypb.allow"
	responseAmbientFile = "../../tracingtest/responseAmbient.json"
	responseFile        = "../../tracingtest/response.json"
	responseTypedErrors = "../../tracingtest/responseTypedErrors.json"
	responseTypedSearch = "../../tracingtest/responseTypedSearch.json"
	tempo230Search      = "../../tracingtest/tempo230_search.json"
	tempo310Search      = "../../tracingtest/tempo310_search.json"
)

// Tempo's search answer is the same document over both transports, so the
// generated type carries it whether it arrived as protobuf or as JSON.
// Receivers must tolerate a field a newer Tempo adds, which is what
// opentelemetry-proto's docs/specification.md requires of an OTLP/JSON reader:
// "receivers MUST ignore message fields with unknown names and MUST unmarshal
// the message as if the unknown field was not present in the payload".
var unmarshalSearch = protojson.UnmarshalOptions{DiscardUnknown: true}

func fieldNumbers(t *testing.T, message proto.Message) map[string]protoreflect.FieldNumber {
	t.Helper()

	fields := message.ProtoReflect().Descriptor().Fields()
	numbers := make(map[string]protoreflect.FieldNumber, fields.Len())
	for i := range fields.Len() {
		field := fields.Get(i)
		numbers[string(field.Name())] = field.Number()
	}
	return numbers
}

// The numbers are upstream's. A renumbered field is accepted over the wire as
// whatever Tempo calls that number, so a search answers with plausible nonsense
// rather than an error.
//
// hack/gen-tempo-protos.sh checks them against every Tempo release it names, by
// fetching each one's IDL. This pins them against what was recorded here, so an
// IDL that nobody regenerated fails the suite without a network.
func TestFieldNumbersAreUnchanged(t *testing.T) {
	assert := assert.New(t)

	assert.Equal(map[string]protoreflect.FieldNumber{
		"Limit":           4,
		"MinDurationMs":   2,
		"Query":           8,
		"SpansPerSpanSet": 9,
		"end":             6,
		"start":           5,
	}, fieldNumbers(t, &SearchRequest{}))

	assert.Equal(map[string]protoreflect.FieldNumber{
		"traces": 1,
	}, fieldNumbers(t, &SearchResponse{}))

	assert.Equal(map[string]protoreflect.FieldNumber{
		"rootServiceName": 2,
		"rootTraceName":   3,
		"spanSet":         6,
		"spanSets":        7,
		"traceID":         1,
	}, fieldNumbers(t, &TraceSearchMetadata{}))

	assert.Equal(map[string]protoreflect.FieldNumber{
		"attributes": 3,
		"matched":    2,
		"spans":      1,
	}, fieldNumbers(t, &SpanSet{}))

	assert.Equal(map[string]protoreflect.FieldNumber{
		"attributes":        5,
		"durationNanos":     4,
		"name":              2,
		"spanID":            1,
		"startTimeUnixNano": 3,
	}, fieldNumbers(t, &Span{}))
}

func TestSearchIsTheOnlyRPC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	assert.Equal("/tempopb.StreamingQuerier/Search", StreamingQuerier_Search_FullMethodName)
	assert.Equal("tempopb.StreamingQuerier", StreamingQuerier_ServiceDesc.ServiceName)

	require.Len(StreamingQuerier_ServiceDesc.Streams, 1)
	assert.Equal("Search", StreamingQuerier_ServiceDesc.Streams[0].StreamName)
	assert.True(StreamingQuerier_ServiceDesc.Streams[0].ServerStreams)
	assert.Empty(StreamingQuerier_ServiceDesc.Methods)
}

// hack/tempo-querypb.allow is what decides the IDL's contents, so what the IDL
// declares has to be what that file says. A name added to or taken out of the
// allow-list and not generated fails here, where it would otherwise be noticed
// only by whoever next runs the generator.
func TestDeclarationsAreTheAllowList(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	listed, err := os.ReadFile(allowFile)
	require.NoError(err)

	var allowed []string
	for _, line := range strings.Split(string(listed), "\n") {
		if name, _, _ := strings.Cut(line, "#"); strings.TrimSpace(name) != "" {
			allowed = append(allowed, strings.TrimSpace(name))
		}
	}
	require.NotEmpty(allowed)

	file := (&SearchResponse{}).ProtoReflect().Descriptor().ParentFile()
	var declared []string
	services := file.Services()
	for i := range services.Len() {
		service := services.Get(i)
		methods := service.Methods()
		for j := range methods.Len() {
			declared = append(declared, fmt.Sprintf("%s/%s", service.Name(), methods.Get(j).Name()))
		}
	}
	messages := file.Messages()
	for i := range messages.Len() {
		message := messages.Get(i)
		fields := message.Fields()
		for j := range fields.Len() {
			declared = append(declared, fmt.Sprintf("%s.%s", message.Name(), fields.Get(j).Name()))
		}
	}

	assert.ElementsMatch(allowed, declared)
}

// Span attributes are the published OTLP types, so a typed value arrives as the
// kind Tempo sent rather than as its string spelling.
func TestFixturesDecodeWithTypedAttributes(t *testing.T) {
	cases := []struct {
		attributeKinds map[string]int
		file           string
		spans          int
		traceID        string
	}{
		{
			attributeKinds: map[string]int{"string": 7},
			file:           responseFile,
			spans:          3,
			traceID:        "100cb753c787ed5657c8d88dafc176ed",
		},
		{
			attributeKinds: map[string]int{"string": 31},
			file:           responseAmbientFile,
			spans:          4,
			// Tempo does not zero-pad a trace ID, so a caller still has to.
			traceID: "2e299711ce47710289dc6640727404f",
		},
		{
			attributeKinds: map[string]int{"bool": 1, "int": 1, "string": 11},
			file:           responseTypedSearch,
			spans:          2,
			traceID:        "f0f0f0f0f0f0f0f0f0f0f0f0f0f01",
		},
		{
			attributeKinds: map[string]int{"string": 13},
			file:           responseTypedErrors,
			spans:          2,
			traceID:        "e0e0e0e0e0e0e0e0e0e0e0e0e0e01",
		},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			body, err := os.ReadFile(c.file)
			require.NoError(err)

			var response SearchResponse
			require.NoError(unmarshalSearch.Unmarshal(body, &response))

			require.Len(response.GetTraces(), 1)
			trace := response.GetTraces()[0]
			assert.Equal(c.traceID, trace.GetTraceID())

			// Tempo repeats the deprecated spanSet as the first of spanSets.
			require.Len(trace.GetSpanSets(), 1)
			require.Len(trace.GetSpanSet().GetSpans(), c.spans)
			require.Len(trace.GetSpanSets()[0].GetSpans(), c.spans)

			kinds := map[string]int{}
			for _, span := range trace.GetSpanSet().GetSpans() {
				assert.NotEmpty(span.GetSpanID())
				assert.NotZero(span.GetDurationNanos(), "quoted uint64")
				for _, attribute := range span.GetAttributes() {
					kinds[kindOf(attribute)]++
				}
			}
			assert.Equal(c.attributeKinds, kinds)
		})
	}
}

// A field the IDL does not declare must not fail the whole answer. serviceStats is
// the real case: Tempo 3.x puts it on every matched trace and Kiali reads none of
// it, so a strict reader would get no traces out of a healthy backend.
func TestUnknownFieldIsIgnored(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	body := []byte(`{"traces":[{"traceID":"abc","serviceStats":{"svc":{"spanCount":2}}}]}`)

	var lenient SearchResponse
	require.NoError(unmarshalSearch.Unmarshal(body, &lenient))
	require.Len(lenient.GetTraces(), 1)
	assert.Equal("abc", lenient.GetTraces()[0].GetTraceID())

	err := protojson.Unmarshal(body, &SearchResponse{})
	assert.ErrorContains(err, `unknown field "serviceStats"`)
}

// Both fixtures are one Tempo answering Kiali's own search, captured from the two
// versions nine releases apart that the generator's range spans, over the same
// spans and cut to the three traces both answered. One set of stubs reads them
// alike: the same traces, the same spans, the same matched counts. That is what
// the cut is for, and what a cut too deep or a renumbered field would break.
//
// Neither body decodes strictly, and both fail on the same key, because every
// Tempo answers with more of the trace than Kiali declares. The leniency is not
// insurance against a release that has not shipped; it is what reads the releases
// that have.
func TestOneIDLReadsBothTempoVersions(t *testing.T) {
	traceIDs := []string{
		// 31 characters: Tempo does not zero-pad a trace ID.
		"dbaf0dc0b41c28514f68e02f3c9f156",
		"1ef2487efdf2b3c410a0996de9e7009b",
		"154b7e552e97f1d529f04168ac4017e9",
	}
	spans := []int{3, 2, 3}

	cases := []struct {
		// The select list reaches more attributes on 3.1.0, which is a backend
		// difference and not a decoding one.
		attributes map[string]int
		file       string
	}{
		{attributes: map[string]int{"string": 50}, file: tempo230Search},
		{attributes: map[string]int{"string": 55}, file: tempo310Search},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			body, err := os.ReadFile(c.file)
			require.NoError(err)

			var response SearchResponse
			require.NoError(unmarshalSearch.Unmarshal(body, &response))
			require.Len(response.GetTraces(), len(traceIDs))

			kinds := map[string]int{}
			for i, trace := range response.GetTraces() {
				assert.Equal(traceIDs[i], trace.GetTraceID())
				assert.Equal("waypoint.tp-idl-svgkbx", trace.GetRootServiceName())
				assert.Len(trace.GetSpanSet().GetSpans(), spans[i])
				assert.EqualValues(spans[i], trace.GetSpanSet().GetMatched())

				// Tempo repeats the deprecated spanSet as the first of spanSets.
				require.Len(trace.GetSpanSets(), 1)
				assert.Len(trace.GetSpanSets()[0].GetSpans(), spans[i])

				for _, span := range trace.GetSpanSet().GetSpans() {
					assert.NotEmpty(span.GetSpanID())
					assert.NotZero(span.GetStartTimeUnixNano(), "quoted uint64")
					assert.NotZero(span.GetDurationNanos(), "quoted uint64")
					for _, attribute := range span.GetAttributes() {
						kinds[kindOf(attribute)]++
					}
				}
			}
			assert.Equal(c.attributes, kinds)

			err = protojson.Unmarshal(body, &SearchResponse{})
			assert.ErrorContains(err, `unknown field "startTimeUnixNano"`)
		})
	}
}

func kindOf(attribute *commonv1.KeyValue) string {
	switch attribute.GetValue().GetValue().(type) {
	case *commonv1.AnyValue_BoolValue:
		return "bool"
	case *commonv1.AnyValue_IntValue:
		return "int"
	case *commonv1.AnyValue_StringValue:
		return "string"
	default:
		return "other"
	}
}
