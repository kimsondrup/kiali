package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tracing settings CI installs Kiali with decide which Jaeger transport every e2e
// run exercises, and nothing follows them afterwards: a run that silently installed the
// other transport, or pointed one transport at the other's port, would still go green
// on whichever half happened to work. These tests read the install script, so they need
// no cluster, no Kiali and no trace store.

const setupKindScript = "hack/setup-kind-in-ci.sh"

func repoFile(t *testing.T, rel string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", "..", rel))
	require.NoError(t, err, "cannot read %s", rel)

	return string(body)
}

// shellFunc returns the text of a shell function declared at the start of a line, so a
// test can run the shipped derivation instead of restating it.
func shellFunc(t *testing.T, script, name string) string {
	t.Helper()

	start := strings.Index(script, "\n"+name+"() {\n")
	require.NotEqual(t, -1, start, "%s does not declare %s()", setupKindScript, name)

	body := script[start+1:]
	end := strings.Index(body, "\n}\n")
	require.NotEqual(t, -1, end, "%s() is not closed by a } at the start of a line", name)

	return body[:end+3]
}

// runShell runs a script and returns its output lines, so an empty output is an empty
// slice rather than one empty argument.
func runShell(t *testing.T, script string) []string {
	t.Helper()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not on PATH, so the shell derivation in %s cannot be driven: %v", setupKindScript, err)
	}

	cmd := exec.Command(bash, "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash failed: %s", out)

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return []string{}
	}
	return strings.Split(trimmed, "\n")
}

// TestCITracingJaegerTransportArgs drives the script's own derivation of the Jaeger
// transport arguments for each --use-grpc value.
//
// The two addresses are not interchangeable: Istio's Jaeger add-on Service exposes the
// query gRPC API on port 16685 and the HTTP API on port 80 (targetPort 16686) under the
// base path /jaeger, so an HTTP client pointed at 16685 cannot connect at all. An empty
// flag must set nothing, so that the suites that do not choose keep exercising Kiali's
// shipped tracing defaults.
func TestCITracingJaegerTransportArgs(t *testing.T) {
	fn := shellFunc(t, repoFile(t, setupKindScript), "jaeger_tracing_transport_args")

	cases := map[string][]string{
		"": {},
		"true": {
			"--set", "external_services.tracing.internal_url=http://tracing.istio-system:16685/jaeger",
			"--set", "external_services.tracing.use_grpc=true",
		},
		"false": {
			"--set", "external_services.tracing.internal_url=http://tracing.istio-system:80/jaeger",
			"--set", "external_services.tracing.use_grpc=false",
		},
	}

	for useGRPC, want := range cases {
		t.Run("use_grpc="+useGRPC, func(t *testing.T) {
			got := runShell(t, fn+"\njaeger_tracing_transport_args '"+useGRPC+"'\n")
			assert.Equal(t, want, got)
		})
	}
}

// TestCITracingInstallUsesTheTransportArgs pins that the single-cluster install passes
// the derived arguments to helm, that an unset flag stays empty, and that Tempo's values
// are the ones it installed before the flag applied to Jaeger too.
func TestCITracingInstallUsesTheTransportArgs(t *testing.T) {
	script := repoFile(t, setupKindScript)

	// Not assert.Contains: it would print the whole script as the haystack.
	for _, want := range []string{
		`USE_GRPC="${USE_GRPC:-}"`,
		`mapfile -t jaeger_tracing_args < <(jaeger_tracing_transport_args "${USE_GRPC}")`,
		`"${jaeger_tracing_args[@]}" \`,
		// The browser link is left as it was installed before.
		`--set external_services.tracing.external_url="http://tracing.istio-system:16685/jaeger" \`,
		// Tempo: an empty flag still means HTTP, as it did when the flag defaulted to false.
		`--set external_services.tracing.provider="tempo" \`,
		`--set external_services.tracing.internal_url="http://tempo-cr-query-frontend.tempo:3200" \`,
		`--set external_services.tracing.use_grpc="${USE_GRPC:-false}" \`,
	} {
		assert.True(t, strings.Contains(script, want), "%s does not contain %s", setupKindScript, want)
	}

	// The Jaeger install must not set the transport outside the derived arguments, or an
	// empty flag would no longer mean the shipped defaults.
	assert.Equal(t, 1, strings.Count(script, `--set external_services.tracing.use_grpc=`),
		"%s sets use_grpc outside the Tempo install", setupKindScript)
}
