package tracingcontract

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/tracing/jaeger/apiv3"
	"github.com/kiali/kiali/tracing/otel/otlp"
)

// streamDepth is the N the search asks for. It is well below the seeded trace count,
// so the answer is bounded by the depth rather than by how much traffic the lab has
// seen.
const streamDepth = 20

// TestStreamShape pins how many messages an api_v3 gRPC search answer arrives in, the
// one property of the stream a reader cannot discover at runtime.
//
// api_v3 FindTraces chunks: each message carries part of the answer, so a reader
// accumulates and the last message alone is a fraction of the result. That is why
// Kiali's Jaeger gRPC transport reads it with otlp.ReadStream, which keeps every
// message, and not with the diff-based reader Tempo's own search stream needs.
func TestStreamShape(t *testing.T) {
	t.Run("api_v3 gRPC chunks one trace per message", func(t *testing.T) {
		for _, store := range requireJaegerGRPC(t) {
			t.Run(store.version, func(t *testing.T) {
				client := queryClient(t, store)

				query := seededQuery()
				query.SearchDepth = streamDepth
				perMessage, err := findTraceMessages(t, client, query)
				require.NoError(t, err, "Jaeger %s refused the seeded search at depth %d", store.version, streamDepth)

				distinct := map[string]bool{}
				for _, ids := range perMessage {
					for _, id := range ids {
						distinct[id] = true
					}
				}
				require.Greater(t, len(distinct), 1,
					"the seeded window holds one trace at most, so a chunked stream is indistinguishable from a single answer here: drive traffic through the lab")

				assert.Len(t, perMessage, len(distinct),
					"api_v3 chunks by trace: %d traces arrived in %d messages, where each message carries one, so accumulating the whole stream is the only way to read it and the last message holds one trace of %d",
					len(distinct), len(perMessage), len(distinct))

				seen := map[string]bool{}
				for i, ids := range perMessage {
					assert.Len(t, ids, 1,
						"message %d of %d carried %d trace IDs, where every message in the lab carries exactly one",
						i+1, len(perMessage), len(ids))
					for _, id := range ids {
						assert.False(t, seen[id],
							"trace %s arrived in more than one message: no trace straddles a chunk boundary in the lab, so a reader may not rely on one message being one whole trace either", id)
						seen[id] = true
					}
				}

				// The shipped reader, over the live stream, so the assertion
				// above is about Kiali and not only about Jaeger.
				recovered := readWithKialisReader(t, client, query)
				assert.Equal(t, sortedKeys(distinct), recovered,
					"otlp.ReadStream must recover every trace the stream carried, and it recovered %d of %d", len(recovered), len(distinct))
			})
		}
	})
}

// findTraceMessages runs an api_v3 gRPC search and reports the distinct trace
// IDs of each stream message separately.
//
// The per-message split is the whole point: findTraces in the harness unions
// them, which is the right way to read the stream and therefore cannot show how
// many messages it took.
func findTraceMessages(t *testing.T, client apiv3.QueryServiceClient, query *apiv3.TraceQueryParameters) ([][]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.FindTraces(ctx, &apiv3.FindTracesRequest{Query: query})
	if err != nil {
		return nil, err
	}
	var perMessage [][]string
	for {
		message, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return perMessage, nil
		}
		if recvErr != nil {
			return perMessage, recvErr
		}
		seen := map[string]bool{}
		for _, rs := range message.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, span := range ss.GetSpans() {
					seen[hex.EncodeToString(span.GetTraceId())] = true
				}
			}
		}
		perMessage = append(perMessage, sortedKeys(seen))
	}
}

// readWithKialisReader drives the shipped otlp.ReadStream over a live api_v3
// stream and reports the distinct trace IDs it recovered.
func readWithKialisReader(t *testing.T, client apiv3.QueryServiceClient, query *apiv3.TraceQueryParameters) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.FindTraces(ctx, &apiv3.FindTracesRequest{Query: query})
	require.NoError(t, err, "the search the shipped reader is driven over was refused")

	traces, err := otlp.ReadStream(ctx, stream)
	require.NoError(t, err, "otlp.ReadStream failed on a live api_v3 stream")

	seen := map[string]bool{}
	for _, rs := range traces.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				seen[hex.EncodeToString(span.GetTraceId())] = true
			}
		}
	}
	return sortedKeys(seen)
}

func sortedKeys(set map[string]bool) []string {
	var keys []string
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
