package querypb

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	responseAmbientFile = "../../tracingtest/responseAmbient.json"
	responseFile        = "../../tracingtest/response.json"
	responseTypedErrors = "../../tracingtest/responseTypedErrors.json"
	responseTypedSearch = "../../tracingtest/responseTypedSearch.json"
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

// The numbers are upstream's, verified against Tempo 3.1.0. A renumbered field
// is accepted over the wire as whatever Tempo calls that number, so a search
// answers with plausible nonsense rather than an error.
func TestFieldNumbersMatchUpstream(t *testing.T) {
	assert := assert.New(t)

	assert.Equal(map[string]protoreflect.FieldNumber{
		"Limit":           4,
		"MinDurationMs":   2,
		"Query":           8,
		"SpansPerSpanSet": 9,
		"Tags":            1,
		"end":             6,
		"start":           5,
	}, fieldNumbers(t, &SearchRequest{}))

	assert.Equal(map[string]protoreflect.FieldNumber{
		"metrics": 2,
		"traces":  1,
	}, fieldNumbers(t, &SearchResponse{}))

	assert.Equal(map[string]protoreflect.FieldNumber{
		"durationMs":        5,
		"rootServiceName":   2,
		"rootTraceName":     3,
		"spanSet":           6,
		"spanSets":          7,
		"startTimeUnixNano": 4,
		"traceID":           1,
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
			assert.NotZero(trace.GetStartTimeUnixNano(), "quoted uint64")

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

// A field only a newer Tempo sends must not fail the whole answer.
func TestUnknownFieldIsIgnored(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	body := []byte(`{"metrics":{"inspectedSpans":"12","completedJobs":1}}`)

	var lenient SearchResponse
	require.NoError(unmarshalSearch.Unmarshal(body, &lenient))
	assert.EqualValues(1, lenient.GetMetrics().GetCompletedJobs())

	err := protojson.Unmarshal(body, &SearchResponse{})
	assert.ErrorContains(err, `unknown field "inspectedSpans"`)
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
