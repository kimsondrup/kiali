package tracing

import (
	"fmt"
	"net/url"

	"github.com/kiali/kiali/config"
)

// ForwardedURL is the internal URL of a tracing backend port-forwarded to localPort. For Jaeger it
// keeps the configured URL's path, because the query service serves api_v3 under its base path:
// Istio's addon sets base_path /jaeger, which is also the path of the default internal_url, and
// answers /api/v3 without it with a plain 404. A Jaeger served at the root is therefore
// configured with an internal_url that has no path. Tempo's URL carries no path, as it never did.
//
// It lives here rather than next to the port-forward in cmd, whose files build only with the
// frontend, so that make test, which builds with exclude_frontend, runs its test.
func ForwardedURL(cfgTracing config.TracingConfig, localPort int) string {
	forwarded := url.URL{Host: fmt.Sprintf("127.0.0.1:%d", localPort), Scheme: "http"}
	if cfgTracing.Provider == "" || cfgTracing.Provider == config.JaegerProvider {
		if configured, err := url.Parse(cfgTracing.InternalURL); err == nil {
			forwarded.Path = configured.Path
		}
	}
	return forwarded.String()
}
