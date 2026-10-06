package json

// The remains of a hand-written OTLP/JSON model. Tempo's search answer is TraceQL metadata rather
// than OTLP, so these three types still describe its attributes and span status; everything that
// described OTLP proper is now go.opentelemetry.io/proto/otlp.

type ValueString struct {
	StringValue string `json:"stringValue"`
}

type Attribute struct {
	Key   string      `json:"key"`
	Value ValueString `json:"value"`
}

type Status struct {
	Code string `json:"code"`
}
