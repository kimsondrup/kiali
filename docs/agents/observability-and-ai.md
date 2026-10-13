---
scribe:
  title: "Observability Integrations and AI"
  description: "Distributed tracing (Jaeger/Tempo/OTel), Grafana, Perses, Kiali self-instrumentation, and the AI Chat integration"
  watch_paths: [tracing/, grafana/, perses/, observability/, ai/]
  scan: "f9d619df93bb21a45a15076ec025938f8e79f856"
  freshness: 92
  human_input: 0
  completeness: 90
  inferred_sections:
    - {id: "overview", heading: "Overview"}
    - {id: "tracing-backends", heading: "Tracing: Supported Backends"}
    - {id: "tracing-client", heading: "Tracing Client Interfaces and Factory"}
    - {id: "jaeger", heading: "Jaeger Integration"}
    - {id: "tempo", heading: "Tempo Integration"}
    - {id: "grafana", heading: "Grafana Integration"}
    - {id: "perses", heading: "Perses Integration"}
    - {id: "observability-package", heading: "Observability Package: Kiali Self-Instrumentation"}
    - {id: "ai-overview", heading: "AI Chat Integration Overview"}
    - {id: "ai-providers", heading: "AI Providers"}
    - {id: "mcp-tools", heading: "MCP Tool System"}
    - {id: "ai-store", heading: "AI Conversation Store"}
  stale_flags: []
  review_notes:
    - finding: "Claim ai-providers:5: StopReasonPauseTurn block is at lines 76-83 (not 72-76), and at the final iteration the code returns an error rather than continuing — this edge case is not documented."
      severity: minor
      tag: WRONG_CLAIM
      confidence: 0.9
      date: "2026-05-14"
    - finding: "purgeInactiveSessions evicts based on session-level LastAccessed (AIChatConversation.LastAccessed), not per-conversation timestamps — all conversations in a stale session are purged together. This granularity detail is absent from the ai-store section."
      severity: minor
      tag: MISSING_SECTION
      confidence: 0.95
      date: "2026-05-14"
    - finding: "Several claim line numbers are off by 1-3 lines (ai-providers:4 cites line 52, actual is 53; ai-store:2 cites 346-375, actual function is 349-378)."
      severity: minor
      tag: STALE_REFERENCE
      confidence: 0.85
      date: "2026-05-14"
---

# Observability Integrations and AI

> TL;DR: Kiali proxies distributed traces from Jaeger or Tempo (via HTTP or gRPC), generates deep-links to Grafana and Perses dashboards, exports its own spans via OTel, and provides an AI Chat feature backed by OpenAI-compatible, Anthropic (Claude), or Google Gemini providers that call a set of MCP tools to query live mesh data.

## Overview

Kiali integrates with four categories of external observability tools:

1. **Distributed tracing backends** (Jaeger, Tempo) — proxied through a `ClientInterface` so the rest of the codebase is backend-agnostic.
2. **Dashboard/visualization tools** (Grafana, Perses) — URL discovery and deep-link generation for embedding into Kiali detail pages.
3. **Kiali self-instrumentation** (`observability/`) — the server emits its own OTel spans and Prometheus metrics.
4. **AI Chat** (`ai/`) — an LLM-backed chat interface that can query live mesh state via MCP tools.

## Tracing: Supported Backends

The `config.ExternalServices.Tracing.Provider` field controls which backend is active. Two values are recognized:

- `"jaeger"` (default) — Jaeger's api_v3 query API, on either transport. The documented minimum is Jaeger 1.54; there is no client for the older unversioned HTTP API (`/api/traces`, `/api/services`) or for the api_v2 gRPC service, and no fallback to them.
- `"tempo"` (`config.TempoProvider`) — Grafana Tempo via its HTTP search and gRPC stream APIs.

Both backends are accessed through the unified `tracing.ClientInterface`. The backend-specific transport is selected at client construction time in `tracing/client.go:newClient`.

Configuration fields relevant to tracing:

| Field | Purpose |
|---|---|
| `ExternalServices.Tracing.Enabled` | Feature flag |
| `ExternalServices.Tracing.InternalURL` | In-cluster URL (preferred); used for actual API calls |
| `ExternalServices.Tracing.ExternalURL` | Public URL (shown to users in the UI for direct links) |
| `ExternalServices.Tracing.UseGRPC` | Jaeger: `false` reads the api_v3 HTTP gateway at `<URL path>/api/v3/...`; `true` calls the gRPC service `jaeger.api_v3.QueryService` at the URL's host:port (the path is ignored; the shipped default is port 16685, `use_grpc: true`). Tempo: gRPC stream for search only |
| `ExternalServices.Tracing.GrpcPort` | Tempo gRPC port (separate from HTTP port); Jaeger ignores it |
| `ExternalServices.Tracing.Provider` | `"jaeger"` or `"tempo"` |
| `ExternalServices.Tracing.Auth` | Auth config (type, token, UseKialiToken) |
| `ExternalServices.Tracing.CustomHeaders` | Extra HTTP/gRPC metadata headers |
| `ExternalServices.Tracing.QueryScope` | Additional tags always added to trace queries |
| `ExternalServices.Tracing.TempoConfig.CacheEnabled` | Enable trace-detail caching (Tempo only) |
| `ExternalServices.Tracing.TempoConfig.CacheCapacity` | FIFO cache capacity |

## Tracing Client Interfaces and Factory

`tracing/client.go` defines three interfaces:

```go
// Unified top-level interface used by business layer
type ClientInterface interface {
    GetAppTraces(ctx, ns, app string, query TracingQuery) (*TracingResponse, error)
    GetTraceDetail(ctx, traceId string) (*TracingSingleTrace, error)
    GetErrorTraces(ctx, ns, app string, duration) (int, error)
    GetServiceStatus(ctx) (bool, error)
    GetServices(ctx) ([]string, error)
}

// HTTP transport (Jaeger HTTP or Tempo HTTP)
type HTTPClientInterface interface {
    GetAppTracesHTTP(ctx, client, baseURL, serviceName, query) (*TracingResponse, error)
    GetTraceDetailHTTP(ctx, client, baseURL, traceID) (*TracingSingleTrace, error)
    GetServiceStatusHTTP(ctx, client, baseURL) (bool, error)
    GetServices(ctx, client, baseURL) ([]string, error)
}

// gRPC transport (Jaeger gRPC or Tempo gRPC stream)
type GRPCClientInterface interface {
    FindTraces(ctx, app, query) (*TracingResponse, error)
    GetTrace(ctx, traceID) (*TracingSingleTrace, error)
    GetServices(ctx) (bool, error)
    GetServicesList(ctx) ([]string, error)
}
```

The concrete `Client` struct holds an `httpTracingClient HTTPClientInterface`, a `grpcClient GRPCClientInterface`, and the `provider` and `transport` it was built for. `newClient` records both explicitly (an empty provider is read as `jaeger`; any other unknown provider is an error), and each `Get*` method dispatches on `transport`, not on which clients happen to be set and not on `config.Get()`:

- Jaeger: every call goes over the configured transport.
- Tempo with `use_grpc`: search, services and the health probe go over gRPC, but `GetTraceDetail` goes over HTTP, because Tempo's gRPC streaming API only searches. A Tempo gRPC client therefore always carries an HTTP client too.

There is no fallback between transports or APIs: a failure is reported, not retried another way.

`NewClient(ctx, conf, token, retry bool)` constructs the client. If `retry = true` (production path), it calls `wait.PollUntilContextCancel` with a 30-second retry interval — the tracing backend is considered non-essential, and startup should not block waiting for it. In `cmd/server.go`, the tracing client is initialized in a goroutine and exposed via a loader closure:

```go
var tracingClient tracing.ClientInterface
tracingLoader := func() tracing.ClientInterface { return tracingClient }
go func() {
    client, err := tracing.NewClient(ctx, conf, kialiToken, true)
    tracingClient = client
}()
```

Handlers receive `traceClientLoader func() tracing.ClientInterface` and call it lazily.

Custom headers are propagated to downstream tracing API calls via gRPC metadata (`metadata.NewOutgoingContext`) using `prepareContextForClient`.

`tracing/discovery_client.go` provides `DiagnoseTracingConfig` — used by the `/api/tracing/diagnose` endpoint — which validates the configuration, probes connectivity, and returns a structured `TracingDiagnose` with a log of test steps. Its Jaeger probes ask api_v3 (`/api/v3/services` over HTTP, `GetServices` over gRPC), also against Tempo hosts, so it never suggests an endpoint the client cannot read. It sets `Code` 200 and a `Message` when it finds a valid configuration, and otherwise `Code` 404 with a message naming api_v3, Jaeger 1.54+ and the probed endpoints; the Discovery tab colours the message by that code.

## Jaeger Integration

`provider: jaeger` reads Jaeger's api_v3 on both transports. Both transports build their requests from one place and convert their answers through the same OTLP pipeline that Tempo uses, so a trace looks the same whichever transport fetched it.

`tracing/jaeger/` contains:

- `apiv3_http.go` — `HTTPClient` implementing `HTTPClientInterface`. Reads the api_v3 HTTP gateway: `GET <base path>/api/v3/traces` (search), `/api/v3/traces/{traceID}` (detail) and `/api/v3/services`. The URL's path is the query service's base path and is joined in front of `/api/v3`. Answers are decoded with `otlp.Decode` (hex IDs, `result` root key). `DecodeServices` reads the services answer and is shared with discovery.
- `apiv3_grpc.go` — `GRPCClient` implementing `GRPCClientInterface`. Calls `jaeger.api_v3.QueryService` (`FindTraces`, `GetTrace`, `GetServices`) at the URL's host:port, which Jaeger serves on the same port as the api_v2 service it replaced (16685). The server-streamed answers are read with `otlp.ReadStream`, which accumulates every message and treats only `io.EOF` as a clean end. Every call is bounded by `query_timeout` when it is positive, as well as by the caller's context.
- `apiv3req.go` — the request builder both transports share. It refuses an empty service name, answers a window nothing can match (empty, inverted, or with no end) as an empty result without sending anything, sends a missing start as the Unix epoch (gRPC refuses the year 1 a zero time renders as), keeps the window at nanosecond precision, and always sends a positive search depth: a limit of 0 or less (as `GetErrorTraces` sends) becomes 100, Jaeger's own default, and anything above `models.MaxTracingLimit` (10000) is clamped. HTTP parameters are snake_case (`query.service_name`, `query.start_time_min`, `query.num_traces`, `query.attributes`, ...), because Jaeger 2.17 refuses or ignores the camelCase spellings.
- `result.go` and `errors.go` — turn answers into `models.TracingResponse` / `models.TracingSingleTrace` with one meaning for "nothing": an unknown trace ID is `(nil, nil)` on both transports (the handler answers 404), and an unmatched search is an empty list. Errors that mean "this is not api_v3" (a plain-text 404, a non-JSON or `services`-less body, gRPC `Unimplemented`) name api_v3 and the 1.54 minimum; HTTP sent to a gRPC port, or gRPC to an HTTP port, is reported as a wrong port or transport.
- `filter.go` — `keepTracesWithTags`, Kiali's own tag filter for an HTTP gateway that ignores `query.attributes` (Jaeger 2.17 and older). A tagged HTTP search is one request at the requested depth that still sends `query.attributes`, so Jaeger 2.20 and later filter on the server; the converted answer is then filtered again, which keeps what such a server kept (keys with a `resource.`, `scope.` or `span.` prefix are left to the server, since the converted trace does not carry them under that name). On a gateway that ignores the attributes the depth applies before the filter, so a tag search, such as the errors-only count, can return fewer matches than the window holds; gRPC, where every version filters and this is never called, and HTTP on Jaeger 2.20 and later do not have this limit.
- The gRPC dial options come from `jaeger.DialOptions()`, which raises the receive limit to 64 MiB: api_v3 sends a whole trace in one message, and gRPC's default 4 MiB refuses a trace of about 10000 spans.
- `apiv3/` — the trimmed api_v3 IDL and its generated stubs (`hack/gen-apiv3-protos.sh` regenerates them and checks the fields against upstream jaeger-idl).
- `logging.go` — logger helpers.

The shared pieces live outside the package: `tracing/otel/otlp` decodes OTLP JSON and reads gRPC streams, and `tracing/otel/model/converter` (`TracesFromOTLP`, `TraceFromOTLP`) converts OTLP into Kiali's provider-neutral trace model in `models/tracing/json`. HTTP requests go through `otel.MakeRequest`, which handles context propagation.

The health probe (`GetServiceStatus`, also used by `/api/tracing/check`) is api_v3 `GetServices`, so a backend that does not serve api_v3 fails it with a clear error instead of looking empty.

The Jaeger version shown in the UI is scraped from the `JAEGER_VERSION` constant in the query service's UI page (`status/versions.go`); the scrape is independent of api_v3 and of the client. With `use_grpc` the scrape strips the port from the URL and tries the default HTTP(S) port, because the shipped default URL is the gRPC port 16685.

## Tempo Integration

`tracing/tempo/` contains:

- `http_client.go` — `OtelHTTPClient` implementing `HTTPClientInterface`. Queries Tempo's `/api/search` endpoint using TraceQL (built by `traceQLBuilder.go`). Optionally caches single-trace responses in a `store.Store[string, *TracingSingleTrace]` with a 5-minute TTL (`store.NewExpirationStore` + `store.NewFIFOStore`).
- `grpc_client.go` — `TempoGRPCClient` implementing `GRPCClientInterface`. Uses Tempo's protobuf streaming API (`tempopb`).
- `util.go` — `ConstructTempoTenantURL` adjusts the base URL when a multi-tenant Tempo setup is configured.
- `model/` — Tempo-specific response models (OTel format).

For Tempo, `newClient` selects:
- HTTP only: `UseGRPC=false` — creates `OtelHTTPClient`.
- gRPC + HTTP: `UseGRPC=true` — creates both an `OtelHTTPClient` (for `GetTraceDetail`) and a gRPC stream client (for `FindTraces`).

The Tempo HTTP client decodes with `otlp.Decode` (base64 IDs, `batches` root key) and converts through the same `tracing/otel/model/converter/` functions as Jaeger, into the provider-neutral model in `models/tracing/json`. Tempo's gRPC stream keeps its own reader rather than `otlp.ReadStream`.

## Grafana Integration

`grafana/grafana.go` provides `Service`, a thin wrapper around Grafana's REST API used to:

1. **Discover the Grafana URL** — if `ExternalServices.Grafana.InternalURL` is set, `ParseUrl` parses the hostname and calls `discoverServiceURL` to look up an OpenShift Route in the target namespace. The discovered URL is cached in `routeURL *string` under a `sync.RWMutex`.
2. **Generate dashboard deep-links** — `Info(ctx, dashboardSupplier)` iterates `conf.ExternalServices.Grafana.Dashboards`, calls Grafana's `/api/search?query=<dashboardName>` API, and constructs full URLs by joining the external base URL with the returned dashboard path.
3. **Return connection info** for the business layer — `Links(ctx, linksSpec)` processes per-resource dashboard link specifications from monitoring dashboard definitions.

Dashboard links support query-string parameters (e.g. `orgId=1`) appended from the configured external URL.

`VersionURL(ctx)` returns the Grafana `/api/frontend/settings` URL used to probe Grafana's version at health-check time.

The `DashboardSupplierFunc` type (`func(string, string, *config.Auth) ([]byte, int, error)`) is a seam for testing — `DashboardSupplier` is a package-level variable defaulting to `findDashboard` and can be overridden in tests.

Grafana auth credentials are loaded from the override-secrets directory (`SecretFileGrafanaUsername`, `SecretFileGrafanaToken`, etc.).

## Perses Integration

`perses/perses.go` provides `Service`, structurally parallel to the Grafana service. It:

1. **Discovers the Perses URL** — same OpenShift Route discovery pattern as Grafana, cached under a `sync.RWMutex`.
2. **Authenticates with Perses** — performs a login POST (`/api/auth/providers/native`) using configured credentials and caches the `AccessToken` / `RefreshToken` in `sessionData`.
3. **Generates dashboard deep-links** — fetches dashboard metadata from Perses REST API and constructs deep-links for embedding in Kiali detail pages.

Perses is configured under `conf.ExternalServices.Perses`. It is disabled by default and must be explicitly enabled and configured. The `/api/perses` endpoint returns `204 No Content` when Perses is disabled.

## Observability Package: Kiali Self-Instrumentation

`observability/tracing.go` configures Kiali's **own** OTel tracing — distinct from the external Jaeger/Tempo backends used for mesh traces:

```go
const TracingService = "kiali"

func InitTracer(collectorURL string) *sdktrace.TracerProvider
```

`InitTracer` creates an OTel `TracerProvider` that exports spans to an OTLP collector (HTTP or gRPC, inferred from the URL). It configures:
- `ParentBased(TraceIDRatioBased(conf.Server.Observability.Tracing.SamplingRate))` as the sampler.
- Resource attributes: `service.name`, `service.namespace`, `hostname` (set to `"kiali.<namespace>"`), `instance_name`.

The tracer is enabled when `conf.Server.Observability.Tracing.Enabled = true`. The server middleware (`otelmux.Middleware`) automatically creates spans for every HTTP request.

The `observability` package also exports the constant `TracingClusterTag = "istio.cluster_id"` used when annotating spans with the mesh cluster.

`server/metrics_server.go` runs a **separate HTTP server** (on `conf.Server.Observability.Metrics.Port`) exposing `promhttp.Handler()` for Prometheus scraping of Kiali's own internal metrics (`prometheus/internalmetrics/`). This server uses the same TLS policy as the main server.

## AI Chat Integration Overview

The `ai/` package implements an LLM-backed chat feature exposed at:
- `POST /api/chat/{provider}/{model}/ai` — send a user query; get an AI response.
- `POST /api/chat/mcp/{tool_name}` — call a single MCP tool directly (used by Kiali's own UI).
- `GET /api/chat/conversations` — list conversation IDs for the current session.
- `DELETE /api/chat/conversations?conversationIDs=<id1,id2,...>` — delete specific conversations. Returns HTTP 400 if the `conversationIDs` query parameter is absent.

The feature is gated by `conf.ChatAI.Enabled`. Providers and models are configured under `conf.ChatAI.Providers`.

## AI Providers

`ai/provider.go:NewAIProvider(conf, providerName, modelName)` constructs the right backend:

```go
switch provider.Type {
case config.OpenAIProvider:
    return openaiProvider.NewOpenAIProvider(conf, provider, model)
case config.AnthropicProvider:
    return anthropicProvider.NewAnthropicProvider(conf, provider, model)
case config.GoogleProvider:
    return googleProvider.NewGoogleAIProvider(conf, provider, model)
}
```

Three provider types are implemented:

### OpenAI Provider (`ai/providers/openai/`)

`openai_provider.go` uses the `github.com/openai/openai-go/v3` SDK. `NewOpenAIProvider` calls `getProviderOptions` which resolves the API key (from config secrets) and constructs `option.RequestOption` values for the SDK client. Three backend configs are supported via `provider.Config`:

| `provider.Config` | Behaviour |
|---|---|
| `"gemini"` | Routes to Google's OpenAI-compatible endpoint (`https://generativelanguage.googleapis.com/v1beta/openai`) |
| `""` (default) | Standard OpenAI endpoint |
| `"azure"` | Azure OpenAI endpoint with `azureAPIVersion = "2024-06-01"` and `azure.NewTokenCredential` |

### Anthropic Provider (`ai/providers/anthropic/`)

`anthropic_provider.go` uses the `github.com/anthropics/anthropic-sdk-go` SDK. `NewAnthropicProvider` resolves the API key via `providers.ResolveProviderKey` and constructs an `anthropic.Client` with `option.WithAPIKey`. An optional `model.Endpoint` overrides the base URL (for custom Anthropic-compatible endpoints).

Only `provider.Config = ""` (default) is supported; other config type values return an error.

The provider uses an internal `anthropicConversation` struct that separates system messages (`[]anthropic.TextBlockParam`) from dialogue messages (`[]anthropic.MessageParam`), reflecting Anthropic's API shape where system prompts are passed as a top-level parameter rather than inline with the conversation.

`SendChat` iterates up to **5 tool-call rounds** (`maxToolIterations = 5`). Within each round:
1. Calls `p.client.Messages.New(ctx, ...)` with the current conversation and tool definitions.
2. If `resp.StopReason == StopReasonPauseTurn`, appends the response and continues without executing tools.
3. If tool-use blocks are present (`anthropicHasToolUse`), extracts them via `TransformToolCallToToolsProcessor`, executes them in parallel, and appends `ToolResult` blocks in a new user message.
4. If no tool use is present, extracts the text answer and exits the loop.

`TransformToolCallToToolsProcessor` returns `([]mcp.ToolsProcessor, []string, error)`, propagating JSON unmarshalling failures from tool argument payloads.

Tool schemas are normalized for Anthropic API compatibility by `normalizeAnthropicInputSchema`:
- `allOf` entries are merged into the top-level schema via `mergeTopLevelAllOf`.
- `anyOf` / `oneOf` combinators are stripped and their semantics approximated as human-readable constraint notes appended to the tool description and individual property descriptions.
- Object schemas without `additionalProperties` have it defaulted to `false`.

`ReduceConversation` uses the shared `providers.SplitConversationForReduction` helper (see below) and summarizes the middle portion via a dedicated `p.client.Messages.New` call with `reduceConversationMaxTokens = 1024`.

Token limits: `defaultMaxTokens = 4096` for chat, `reduceConversationMaxTokens = 1024` for summarization.

The provider implements `providers.AIProvider` (defined in `ai/providers/provider.go`):

```go
type AIProvider interface {
    InitializeConversation(conversation *[]ConversationMessage, req AIRequest)
    ReduceConversation(ctx, conversation, reduceThreshold) []ConversationMessage
    GetToolDefinitions() interface{}
    TransformToolCallToToolsProcessor(toolCall any) ([]mcp.ToolsProcessor, []string, error)
    ConversationToProvider(conversation []ConversationMessage) interface{}
    ProviderToConversation(providerMessage interface{}) ConversationMessage
    SendChat(kialiInterface *mcputil.KialiInterface, req AIRequest, aiStore AIStore) (*AIResponse, int)
}
```

All three provider implementations (OpenAI, Anthropic, Google) propagate errors from `TransformToolCallToToolsProcessor`.

### Google Provider (`ai/providers/google/`)

`google_provider.go` uses the Google Generative AI Go SDK. The `GoogleAIProvider` implements the same `AIProvider` interface.

### Shared helpers (`ai/providers/helpers.go`)

`SplitConversationForReduction(conversation, reduceThreshold, keepCount)` splits a conversation into a system-instruction anchor, the summarizable middle portion, and the recent tail. Returns `(instructions, toSummarize, recentMessages, ok bool)` where `ok = false` if the conversation is below the threshold or too short to split meaningfully. The anchor is determined by counting leading messages whose `Role == "system"` (up to the first 2).

### Key credential resolution

`providers.ResolveProviderKey(conf, provider, model)` resolves API keys at runtime from the config's secret files. Provider-level keys take precedence over model-level keys. Secret file names follow the pattern `chat-ai-provider-<sanitized-name>` and `chat-ai-model-<sanitized-provider>-<sanitized-model>`.

## MCP Tool System

The `ai/mcp/` package defines a declarative tool system. Tool definitions live in YAML files under `ai/mcp/tools/` (embedded via `//go:embed tools`).

Each YAML file defines a `ToolDef`:

```go
type ToolDef struct {
    Name        string                 // tool name, e.g. "get_mesh_traffic_graph"
    Description string                 // human-readable description sent to the LLM
    InputSchema map[string]interface{} // JSON Schema for tool arguments
    Toolset     []string               // "default" | "mcp" | both
}
```

On first use, `LoadTools()` reads all YAML files and populates two maps:
- `MCPToolHandlers` — tools in the `"mcp"` toolset, used when Kiali acts as an MCP server.
- `DefaultToolHandlers` — tools in the `"default"` toolset, used for the built-in chat UI.

The tools implemented (each with its own subdirectory containing the handler `go` file):

| Tool name | Purpose |
|---|---|
| `get_mesh_traffic_graph` | Returns the traffic graph for given namespaces |
| `get_metrics` | Returns Prometheus metrics for a workload/service/app |
| `get_pod_performance` | Returns CPU/memory usage metrics |
| `get_logs` | Fetches pod logs |
| `get_mesh_status` | Returns overall mesh health and component status |
| `get_trace_details` | Fetches a specific trace by ID |
| `list_traces` | Lists traces for an app/service |
| `list_or_get_resources` | Lists or gets Kubernetes/Istio resources |
| `manage_istio_config` | Creates/updates/deletes Istio config objects |
| `manage_istio_config_read` | Read-only Istio config operations |
| `get_referenced_docs` | Returns relevant Kiali documentation (UI-only) |
| `get_action_ui` | Returns UI action suggestions (UI-only) |

Two special exclusions are maintained in `ExcludedToolNames` — `get_referenced_docs` and `get_action_ui` are excluded from the MCP server tool listing since they are UI-specific.

Tools that require Prometheus are tracked in `MetricToolNames`; tools that require a tracing backend in `TraceToolNames`. The handler logic checks `config.Get().ExternalServices.Prometheus.Enabled` / `Tracing.Enabled` before executing these tools and returns a helpful error message when the dependency is disabled.

`HeaderKialiUI = "Kiali-UI"` is an HTTP header the Kiali browser sends on `POST /api/chat/mcp/{tool}` to signal it is the internal UI client. When present, `ChatMCP` uses `DefaultToolHandlers` instead of `MCPToolHandlers`.

## AI Conversation Store

`ai/store.go` implements `types.AIStore`:

```go
type AIStore interface {
    Enabled() bool
    ReduceWithAI() bool
    ReduceThreshold() int
    GetConversation(sessionID, conversationID string) (*Conversation, bool)
    SetConversation(sessionID, conversationID string, conversation *Conversation) error
    GetConversationIDs(sessionID string) []string
    DeleteConversations(sessionID string, conversationIDs []string) error
}
```

The concrete `AIStoreImpl` stores conversations in a `map[sessionID]*AIChatConversation` where each value holds a `map[conversationID]*Conversation`. Thread-safety is maintained at two levels: a top-level `sync.RWMutex` on the store, and per-session and per-conversation mutexes.

**Memory management**: `SetConversation` calls `EstimateConversationMemory` (character count / 1024 / 1024 → MB) before storing. If the projected total memory would exceed `MaxCacheMemoryMB` (default 1024 MB), `evictLRUConversations` evicts the least-recently-accessed conversations until enough space is freed. A single conversation that alone exceeds `MaxCacheMemoryMB` is rejected immediately with an error rather than triggering eviction. After eviction, if the post-eviction projected memory still exceeds the cap, `SetConversation` returns an error. `internalmetrics.GetAIStoreEvictionsTotalMetric().Inc()` is called for each evicted conversation. When the last conversation in a session is evicted, the session entry itself is deleted.

**Inactivity-based cleanup**: `NewAIStore` launches a background goroutine (`startInactivityCleanupLoop`) when `Enabled = true` and `InactivityTimeout > 0`. The goroutine ticks at half the timeout duration (clamped between 50 ms and 1 minute) and calls `purgeInactiveSessions(now)`. Any session whose `LastAccessed` is older than `InactivityTimeout` is deleted wholesale. `GetConversationIDs` and `DeleteConversations` both update `AIChatConversation.LastAccessed` (previously only `SetConversation` / `GetConversation` updated it). Empty sessions (zero conversations) are also removed immediately after `DeleteConversations` empties them.

**Configuration** (loaded by `LoadAIStoreConfig`):

| Field | Default | Purpose |
|---|---|---|
| `InactivityTimeout` | 30m | Purge sessions not accessed within this duration |
| `MaxCacheMemoryMB` | 1024 | Memory cap across all sessions |
| `ReduceWithAI` | false | Use the AI to summarize long conversations before they hit the threshold |
| `ReduceThreshold` | 15 | Message count threshold that triggers AI-based reduction |

`InactivityTimeout` is loaded from `cfg.ChatAI.StoreConfig.InactivityTimeout` (parsed as a duration string). Invalid or zero values fall back to 30 minutes with a warning log.

A `Conversation` contains:
```go
type Conversation struct {
    Conversation []ConversationMessage
    LastAccessed time.Time
    EstimatedMB  float64
    Mu           sync.RWMutex
}
```

Each `ConversationMessage` has `Role`, `Content`, `Name`, and `Param` fields — the role/content model is provider-agnostic; providers convert to/from their SDK-specific message types via `ConversationToProvider` / `ProviderToConversation`.

**System prompt security**: The system prompt (`ai/types/prompt.go`) includes a `TOOL OUTPUT HANDLING` section that instructs the LLM to treat data returned by tools as raw, untrusted data — never as instructions — preventing prompt injection attacks via tool results that might contain directive text from the cluster.

The AI store is initialized in `routing/router.go` via `ai.NewAIStore(ctx, aiStoreConfig)` and passed to the AI handler functions as `aiStore types.AIStore`.
