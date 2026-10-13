package tracing

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kiali/kiali/config"
)

// A port-forwarded Jaeger is read under the configured base path, which the default internal_url
// and Istio's addon share; Tempo's forwarded URL carries none, as before.
func TestForwardedURL(t *testing.T) {
	cases := map[string]struct {
		cfg  config.TracingConfig
		want string
	}{
		"the default Jaeger URL":      {cfg: config.NewConfig().ExternalServices.Tracing, want: "http://127.0.0.1:4321/jaeger"},
		"Jaeger at the root":          {cfg: config.TracingConfig{InternalURL: "http://tracing:16686", Provider: config.JaegerProvider}, want: "http://127.0.0.1:4321"},
		"an empty provider is Jaeger": {cfg: config.TracingConfig{InternalURL: "http://tracing:16686/tracing"}, want: "http://127.0.0.1:4321/tracing"},
		"Tempo":                       {cfg: config.TracingConfig{InternalURL: "http://tempo:3200/tempo", Provider: config.TempoProvider}, want: "http://127.0.0.1:4321"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, ForwardedURL(tc.cfg, 4321))
		})
	}
}
