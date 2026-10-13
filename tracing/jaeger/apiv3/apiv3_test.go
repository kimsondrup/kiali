package apiv3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// These guard the generated stubs rather than any behaviour of them: a method
// name or a field number that drifts from the IDL still compiles, and shows up
// only as a backend that answers Unimplemented or ignores a predicate.

func TestMethodNames(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	assert.Equal("/jaeger.api_v3.QueryService/GetTrace", QueryService_GetTrace_FullMethodName)
	assert.Equal("/jaeger.api_v3.QueryService/FindTraces", QueryService_FindTraces_FullMethodName)
	assert.Equal("/jaeger.api_v3.QueryService/GetServices", QueryService_GetServices_FullMethodName)

	assert.Equal("jaeger.api_v3.QueryService", QueryService_ServiceDesc.ServiceName)

	// Both searches stream their traces in chunks; the service list does not.
	require.Len(QueryService_ServiceDesc.Streams, 2)
	assert.Equal("GetTrace", QueryService_ServiceDesc.Streams[0].StreamName)
	assert.True(QueryService_ServiceDesc.Streams[0].ServerStreams)
	assert.Equal("FindTraces", QueryService_ServiceDesc.Streams[1].StreamName)
	assert.True(QueryService_ServiceDesc.Streams[1].ServerStreams)
	require.Len(QueryService_ServiceDesc.Methods, 1)
	assert.Equal("GetServices", QueryService_ServiceDesc.Methods[0].MethodName)
}

func TestGetTraceRequestTakesAHexTraceID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	traceID := (&GetTraceRequest{}).ProtoReflect().Descriptor().Fields().ByName("trace_id")
	require.NotNil(traceID)

	assert.Equal(protoreflect.FieldNumber(1), traceID.Number())
	// api_v3 asks for the hex encoding of the ID, where api_v2 asked for the
	// 16 raw bytes, so the ID Kiali already validates goes straight in.
	assert.Equal(protoreflect.StringKind, traceID.Kind())
}

func TestTraceQueryParametersFieldNumbers(t *testing.T) {
	assert := assert.New(t)

	fields := (&TraceQueryParameters{}).ProtoReflect().Descriptor().Fields()
	numbers := make(map[string]protoreflect.FieldNumber, fields.Len())
	for i := range fields.Len() {
		field := fields.Get(i)
		numbers[string(field.Name())] = field.Number()
	}

	// The numbers are upstream's. A renumbered field is accepted over the wire
	// as whatever upstream calls that number, so a search silently answers with
	// traces nobody asked for.
	assert.Equal(map[string]protoreflect.FieldNumber{
		"service_name":   1,
		"attributes":     3,
		"start_time_min": 4,
		"start_time_max": 5,
		"duration_min":   6,
		"search_depth":   8,
	}, numbers)

	query := (&FindTracesRequest{}).ProtoReflect().Descriptor().Fields().ByName("query")
	require.NotNil(t, query)
	assert.Equal(protoreflect.FieldNumber(1), query.Number())
}

func TestGetServicesResponseReadsTheAPIV3Key(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const body = `{"services":["a","b"]}`

	var response GetServicesResponse
	require.NoError(protojson.Unmarshal([]byte(body), &response))
	assert.Equal([]string{"a", "b"}, response.GetServices())

	marshalled, err := protojson.Marshal(&response)
	require.NoError(err)
	assert.JSONEq(body, string(marshalled))

	// The key the unversioned Jaeger API used. Reading it as a service list is
	// what made an api_v3 backend look like one with no services at all.
	assert.Error(protojson.Unmarshal([]byte(`{"data":["a","b"]}`), &GetServicesResponse{}))
}
