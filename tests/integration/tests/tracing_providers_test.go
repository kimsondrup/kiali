package tests

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/tests/integration/utils/kiali"
	"github.com/kiali/kiali/tests/integration/utils/kube"
)

// tracingProviderRow is one provider and transport pair to install and then exercise
// against the trace store the suite's Istio brought with it.
//
// The expected provider and transport are test data and are never read back from GET
// /api/tracing, so a row names what it meant to exercise instead of trusting the server
// under test to report it.
type tracingProviderRow struct {
	// internalURL belongs to the row because the port decides the transport. Istio's
	// add-on Service exposes the query gRPC API on 16685 and port 80 onto the query
	// UI's 16686, which serves HTTP under the base path /jaeger. The gRPC client
	// ignores the path; the HTTP client joins it in front of /api/v3.
	internalURL string
	provider    config.TracingProvider
	transport   string
	useGRPC     bool
}

// tracingProviderRows are the two transports of provider jaeger, and both read
// Jaeger's api_v3: use_grpc true dials jaeger.api_v3.QueryService, which Jaeger serves
// on the same 16685 port as its older gRPC API, and use_grpc false reads the api_v3
// HTTP gateway under the base path.
var tracingProviderRows = []tracingProviderRow{
	{
		internalURL: "http://tracing.istio-system:16685/jaeger",
		provider:    config.JaegerProvider,
		transport:   "api_v3 gRPC",
		useGRPC:     true,
	},
	{
		internalURL: "http://tracing.istio-system:80/jaeger",
		provider:    config.JaegerProvider,
		transport:   "api_v3 HTTP",
		useGRPC:     false,
	},
}

// TestTracingProviders installs each provider and transport pair in turn and runs the
// trace, detail, unknown-trace and error-trace assertions against it.
//
// The restore in the defer is not optional: every later test in this package talks to
// the same pod. GetConfig forces tracing enabled, so the restored config has it enabled
// whatever the suite installed, which is also why no row here covers tracing switched
// off.
func TestTracingProviders(t *testing.T) {
	require := require.New(t)
	kubeClientSet := kube.NewKubeClient(t)
	dynamicClient := kube.NewDynamicClient(t)

	ctx := contextWithTestingDeadline(t)

	instance, err := kiali.NewInstance(ctx, kubeClientSet, dynamicClient)
	require.NoError(err)

	cfg, err := instance.GetConfig(ctx)
	require.NoError(err)

	installed := cfg.ExternalServices.Tracing
	defer func() {
		cfg.ExternalServices.Tracing = installed
		require.NoError(instance.UpdateConfig(ctx, cfg))
		require.NoError(instance.Restart(ctx))
	}()

	for _, row := range tracingProviderRows {
		cfg.ExternalServices.Tracing.InternalURL = row.internalURL
		cfg.ExternalServices.Tracing.Provider = row.provider
		cfg.ExternalServices.Tracing.UseGRPC = row.useGRPC
		require.NoError(instance.UpdateConfig(ctx, cfg))
		require.NoError(instance.Restart(ctx))

		t.Run(string(row.provider)+" over "+row.transport, func(t *testing.T) {
			assertTracingProviderRow(t, row, cfg.ExternalServices.Tracing)
		})
	}
}

// assertTracingProviderRow reads traces, a trace detail, an unknown trace and an
// error-trace count through whichever transport the installed row dispatches to.
//
// Nothing here is conditional on the answer being non-empty, which is what makes a row
// mean anything: a client that reached an API it cannot read, or sent a query the
// backend refused, answers 200 with an empty array, and a row that tolerated that would
// report every transport as working.
func assertTracingProviderRow(t *testing.T, row tracingProviderRow, tracingConfig config.TracingConfig) {
	t.Helper()
	require := require.New(t)
	name := "details"

	validation, statusCode, err := kiali.TracingConfigCheck(tracingConfig)
	require.NoError(err)
	require.Equal(200, statusCode)
	require.Empty(validation.Error, "%s over %s failed its own health probe: %s",
		row.provider, row.transport, validation.Error)

	traces, statusCode, err := kiali.Traces("apps", name, kiali.BOOKINFO)
	require.Equal(200, statusCode, "%s over %s answered the trace query %d: %s",
		row.provider, row.transport, statusCode, err)
	assertTraces(traces, statusCode, err, require)

	t.Run("trace detail", assertTraceDetail)
	t.Run("unknown trace detail", assertUnknownTraceDetail)

	// Over gRPC the store applies Kiali's error=true tag itself. Over HTTP, Jaeger 2.17.0's
	// api_v3 gateway ignores query.attributes, so Kiali re-applies the tags to what came
	// back. Either way the count cannot exceed the window's own trace list, which is what
	// catches an error tag that narrowed nothing.
	t.Run("error traces", func(t *testing.T) {
		assertErrorTraces(t, name, 10*time.Minute)
	})
}
