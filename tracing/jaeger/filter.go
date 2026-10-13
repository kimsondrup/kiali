package jaeger

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	jsonModel "github.com/kiali/kiali/models/tracing/json"
)

// keepTracesWithTags keeps the traces in which at least one span carries every requested tag,
// counting the span's own tags and its process's.
//
// It exists because Jaeger 2.17.0's HTTP gateway ignores query.attributes in every spelling,
// measured: a search for {"error":"true"} answers every trace in the window, and one for an
// istio.cluster_id no span carries answers every trace rather than none. Jaeger 1.54's gateway
// never read the parameter either. Filtering again inside the one API that was asked is not a
// fallback to another. The gRPC transport, where every version filters, never calls this.
//
// GetAppTracesHTTP calls it on every answer, so on Jaeger 2.20 and later, which filter on the
// server, it has to keep what the server kept, while it sees only what the converter made of a
// span. So it follows Jaeger 2.20's own matching where the converter changed the span:
//   - error is read as a truth value, as the server reads it, and true matches the error tag the
//     converter folds a failed status, an error flag and status="error" into; false matches a span
//     without it or with an OK status, which is what the server matches it against.
//   - status=error matches that error tag too, since the converter folds the attribute into it.
//   - service.name matches the process's service name, where the converter moves the attribute.
//   - span.kind matches the converted kind whatever its case, as the server does.
//   - every other value is compared as the server renders it: a number in decimal, a boolean as
//     true or false, bytes in base64.
//
// The converter keeps no record of a folded error flag or status attribute, so error=false and
// status=error match more here than on the server: on Jaeger 2.17, where this is the only filter,
// error=false keeps every span that did not fail and status=error every failed span. Matching too
// much there is preferred to dropping what a server that filters kept.
//
// The keys the server reads as something other than an attribute, a resource. prefix and the
// exact keys scope.name, scope.version and span.status, are left to it, because the converted
// trace carries them under another name or not at all; on a gateway that ignores them they narrow
// nothing. Any other scope. or span. key is an ordinary attribute to the server and is matched
// here. What the converted trace does not carry still drops over HTTP a trace 2.20 answered: a tag
// that only a span event, a span link or the instrumentation scope's attributes carry.
func keepTracesWithTags(traces []jsonModel.Trace, tags map[string]string) []jsonModel.Trace {
	checked := make(map[string]string, len(tags))
	for key, value := range tags {
		if !serverOnlyKey(key) {
			checked[key] = value
		}
	}
	if len(checked) == 0 {
		return traces
	}

	// Never nil: the handlers marshal Data straight to the browser, where null and [] differ.
	kept := make([]jsonModel.Trace, 0, len(traces))
	for _, trace := range traces {
		for _, span := range trace.Spans {
			process := trace.Processes[span.ProcessID]
			if spanCarriesTags(span, process, checked) {
				kept = append(kept, trace)
				break
			}
		}
	}
	return kept
}

// serverOnlyKey reports whether Jaeger 2.20 and later read a tag key as naming something other
// than an attribute, which the converted trace does not carry under that key: a resource.
// prefix, which names a resource attribute only, or one of the scope's or the status's fields.
func serverOnlyKey(key string) bool {
	switch key {
	case "scope.name", "scope.version", "span.status":
		return true
	}
	return strings.HasPrefix(key, "resource.")
}

// spanCarriesTags reports whether every requested tag is on the span or on its process.
func spanCarriesTags(span jsonModel.Span, process jsonModel.Process, tags map[string]string) bool {
	for key, want := range tags {
		if !spanCarriesTag(span, process, key, want) {
			return false
		}
	}
	return true
}

// spanCarriesTag reports whether one requested tag is on the span or on its process, reading the
// keys the converter rewrites the way keepTracesWithTags describes.
func spanCarriesTag(span jsonModel.Span, process jsonModel.Process, key, want string) bool {
	switch key {
	case "error":
		flag, err := strconv.ParseBool(want)
		if err != nil {
			break
		}
		failed := isError(span.Tags) || isError(process.Tags)
		if flag {
			return failed
		}
		return !failed || hasTag(span.Tags, "otel.status_code", "OK")
	case "service.name":
		return process.ServiceName == want
	case "span.kind":
		for _, kv := range span.Tags {
			if kv.Key == key && strings.EqualFold(tagText(kv.Value), want) {
				return true
			}
		}
		return false
	case "status":
		if want == "error" && (isError(span.Tags) || isError(process.Tags)) {
			return true
		}
	}
	return hasTag(span.Tags, key, want) || hasTag(process.Tags, key, want)
}

// isError reports whether the list holds an error tag whose value reads as true: the Boolean the
// converter sets, or the string a proxy sets on a process.
func isError(kvs []jsonModel.KeyValue) bool {
	for _, kv := range kvs {
		if kv.Key != "error" {
			continue
		}
		if flag, err := strconv.ParseBool(tagText(kv.Value)); err == nil && flag {
			return true
		}
	}
	return false
}

// hasTag reports whether the list holds the key with the value, compared as text.
func hasTag(kvs []jsonModel.KeyValue, key, want string) bool {
	for _, kv := range kvs {
		if kv.Key == key && tagText(kv.Value) == want {
			return true
		}
	}
	return false
}

// tagText renders a converted tag value the way Jaeger's pdata AsString renders the attribute it
// came from, which is what the server compares a requested value with: a double as JSON writes it,
// an integer in decimal and bytes in base64.
func tagText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		// encoding/json writes a finite double exactly as AsString does; the converter has
		// already turned a non-finite one into text.
		if text, err := json.Marshal(v); err == nil {
			return string(text)
		}
	case []byte:
		return base64.StdEncoding.EncodeToString(v)
	}
	return fmt.Sprint(value)
}
