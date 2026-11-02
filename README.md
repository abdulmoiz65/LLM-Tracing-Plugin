# llm-tracing

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) plugin that exports every proxied LLM request as an **OpenTelemetry trace** over **OTLP/HTTP (JSON)**. It works with any OTLP backend: Langfuse, Jaeger, Grafana Tempo, Datadog, Honeycomb, or an OpenTelemetry Collector.

- **One trace per inbound request.** A `SERVER` root span covers the client request. Each upstream attempt (including retries and failovers) is a `CLIENT` child span with tokens, latency, time-to-first-token, provider, model and credential index.
- **No added latency.** The host delivers `usage.handle` and `request.complete` asynchronously. The plugin only decodes the event and appends it to an in-memory buffer. Batching, HTTP export and retries run on one background goroutine.
- **Privacy-safe defaults.** It never exports request or response bodies. The client API key is exported only as a SHA-256 fingerprint. Upstream error bodies are redacted and truncated. Upstream URLs lose their credentials and query strings.
- **Bounded memory.** Pending traces, queued spans and attempts per request all have hard caps. When a cap is hit, the oldest data goes first and the drop is counted.

## How it works

CPA calls two plugin capabilities:

| Capability | Method | Fires | Becomes |
|---|---|---|---|
| `request_lifecycle_plugin` | `request.complete` | once per inbound request (success, failure, rejection or cancellation) | root span |
| `usage_plugin` | `usage.handle` | once per upstream attempt, success **or** failure | child span |

Both events carry the same `TraceID`, which is the CPA request ID of the inbound HTTP request, and the plugin joins them on it. Both are delivered asynchronously, the completion from its own goroutine and usage through a queue, so they can arrive in either order:

- **Usage first.** The record waits in a buffer until its completion arrives.
- **Completion first.** The completion waits `completion_grace_ms` (default 2 s) for late usage records, then the trace is exported.
- **Usage after export.** A short-lived index of exported requests lets a late record join the already-exported trace as a child of the same root. OTLP backends merge spans by trace ID.
- **Completion never arrives.** After `orphan_timeout_ms` (default 10 min) the usage records are exported under a synthesized root span (`cpa.root_synthesized=true`, `cpa.outcome=unknown`).
- **No TraceID.** A usage record without a TraceID becomes its own single-span trace.

A long-lived WebSocket connection (OpenAI Responses WebSocket) reuses one TraceID for every turn. Each turn's completion becomes its own OTel trace, and usage records are assigned to the turn whose `[StartedAt, CompletedAt]` window contains them. All turns share `cpa.trace_id`.

Completions with no model and no upstream attempt that succeeded, such as `/v1/models` listings, are not traced.

## Install

From the CPA plugin store, or manually:

1. Download `llm-tracing_<version>_<os>_<arch>.zip` from the releases page and check it against `checksums.txt`.
2. Extract `llm-tracing.dll` / `.so` / `.dylib` into `<plugins.dir>/<goos>/<goarch>/`, for example `plugins/windows/amd64/llm-tracing.dll`. The file name (without extension) is the plugin ID.
3. Configure it under `plugins.configs.llm-tracing` (see below).

## Configuration

Only `endpoint` is required. With an empty endpoint the plugin is a no-op and logs that once.

| Key | Default | Description |
|---|---|---|
| `enabled`, `priority` | host | Standard CPA plugin keys. |
| `endpoint` | — | OTLP/HTTP traces URL (the full path, usually ending in `/v1/traces`). |
| `headers` | `{}` | Extra exporter headers. Either a map, or a list of `"Key: Value"` strings or `{name, value}` items. Values are never logged. |
| `service_name` | `cliproxyapi` | `service.name` resource attribute. |
| `sample_rate` | `1.0` | Fraction of traces kept (0..1). The decision is deterministic per TraceID, so a trace is always kept or dropped whole. |
| `completion_grace_ms` | `2000` | Wait for late usage records after a completion. |
| `orphan_timeout_ms` | `600000` | Export usage records whose completion never arrived. This is also how long exported requests stay joinable by late records. |
| `capture_error_body` | `true` | Attach redacted, truncated (1024 bytes) error text to failed spans. |
| `max_pending_traces` | `10000` | Requests buffered while waiting. When exceeded, the oldest is exported early. |
| `max_queue_spans` | `20000` | Finished spans awaiting export. When exceeded, the oldest are dropped. |
| `flush_interval_ms` | `1000` | Export interval. |
| `max_batch_spans` | `512` | Spans per export request. |
| `export_timeout_ms` | `10000` | HTTP timeout per export attempt. |

Numeric values can be YAML numbers or numeric strings. Out-of-range values are clamped and a warning is logged.

### Jaeger / Grafana Tempo / OpenTelemetry Collector (local OTLP)

```yaml
plugins:
  enabled: true
  configs:
    llm-tracing:
      enabled: true
      endpoint: http://127.0.0.1:4318/v1/traces
      service_name: cliproxyapi
```

Jaeger all-in-one: `docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one` (OTLP is enabled by default in recent versions). Then open <http://localhost:16686>. Tempo and the Collector accept the same endpoint on their OTLP/HTTP receiver. For multi-tenant Tempo or Mimir, add `headers: { X-Scope-OrgID: <tenant> }`.

### Langfuse

Langfuse takes OTLP/HTTP with Basic auth built from your project keys: `base64("<public_key>:<secret_key>")`.

```bash
echo -n "pk-lf-...:sk-lf-..." | base64
```

```yaml
    llm-tracing:
      enabled: true
      endpoint: https://cloud.langfuse.com/api/public/otel/v1/traces   # EU; US: https://us.cloud.langfuse.com/...; self-hosted: https://<host>/api/public/otel/v1/traces
      headers:
        Authorization: Basic cGstbGYtLi4uOnNrLWxmLS4uLg==
```

Attempt spans carry `langfuse.observation.type=generation` and `gen_ai.*` model and usage attributes, so Langfuse shows them as generations with token counts. Root spans set `langfuse.trace.name`, and also `langfuse.session.id` and `langfuse.user.id` (the masked client key) when those are known. The redaction patterns also catch the Langfuse secret key format (`sk-lf-...`) if it shows up in an upstream error.

### Honeycomb

```yaml
    llm-tracing:
      enabled: true
      endpoint: https://api.honeycomb.io/v1/traces    # EU: https://api.eu1.honeycomb.io/v1/traces
      headers:
        x-honeycomb-team: <ingest key>
```

### Datadog

Datadog takes OTLP through the Datadog Agent or the OpenTelemetry Collector. Enable the OTLP HTTP receiver on the Agent (`DD_OTLP_CONFIG_RECEIVER_PROTOCOLS_HTTP_ENDPOINT=0.0.0.0:4318`) and point the plugin at it:

```yaml
    llm-tracing:
      enabled: true
      endpoint: http://127.0.0.1:4318/v1/traces
```

If you use an agentless OTLP intake for your site, pass the key with `headers: ["DD-API-KEY: <key>"]`.

## Span reference

### Resource

`service.name`, `service.version` (plugin version), `telemetry.sdk.name=llm-tracing`, `telemetry.sdk.language=go`, `telemetry.sdk.version`. Instrumentation scope: `llm-tracing`.

### Root span: `cpa.request {model}`, kind SERVER

The span runs from `StartedAt` to `CompletedAt` of the request.

| Attribute | Source |
|---|---|
| `cpa.trace_id` | CPA request ID (join key, matches CPA logs) |
| `cpa.request_id` | lifecycle request ID |
| `cpa.outcome` | `succeeded` / `failed` / `rejected` / `canceled` (`unknown` on synthesized roots) |
| `http.response.status_code` | status returned to the client |
| `gen_ai.request.model` | model requested by the client |
| `cpa.model` | resolved model |
| `cpa.source_format` | client protocol (openai, claude, gemini, …) |
| `cpa.stream` | streaming request |
| `cpa.attempt_count` | number of upstream attempts |
| `cpa.usage.{input,output,total}_tokens` (+ `reasoning`, `cached`, `cache_read`, `cache_creation` when non-zero) | sums over attempts |
| `cpa.client_key`, `langfuse.user.id` | masked client key |
| `session.id`, `langfuse.session.id` | client session ID, when present |
| `langfuse.trace.name` | span name |
| `error.type`, `cpa.error`, `langfuse.observation.level=ERROR` | when the outcome is not `succeeded`; status is ERROR |

Rolled-up tokens go under `cpa.usage.*` rather than `gen_ai.usage.*`, so backends that sum generation usage per trace do not count them twice.

### Attempt span: `cpa.attempt {provider}`, kind CLIENT

The span runs from `RequestedAt` to `RequestedAt + Latency` and is a child of the root.

| Attribute | Source |
|---|---|
| `gen_ai.system`, `gen_ai.provider.name` | CPA provider key (e.g. `claude`, `codex`, `gemini`, `openai-compatible-<name>`) |
| `gen_ai.request.model` / `gen_ai.response.model` | upstream model sent / reported |
| `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | token counts |
| `cpa.usage.total_tokens`, `cpa.usage.{reasoning,cached,cache_read,cache_creation}_tokens` | extra counters (non-zero only) |
| `cpa.attempt` | 1-based ordinal by start time |
| `cpa.request_id`, `cpa.trace_id` | attempt ID / join key |
| `cpa.alias` | model alias the client used |
| `cpa.executor`, `cpa.base_url` (no credentials or query) | upstream route |
| `cpa.auth_index`, `cpa.auth_type` | credential used (index only, never the credential) |
| `cpa.client_key` | `sha256:<12 hex>` fingerprint of the client key |
| `cpa.access_provider`, `cpa.session_id`, `cpa.parent_session_id` | client identity context |
| `cpa.reasoning_effort`, `cpa.service_tier`, `cpa.effective_service_tier`, `cpa.response_service_tier` | request options |
| `cpa.stream`, `cpa.ttft_ms` | streaming and time to first token |
| `langfuse.observation.type=generation` | Langfuse mapping |
| `http.response.status_code`, `error.type`, `cpa.error`, `langfuse.observation.level`, `langfuse.observation.status_message` | failed attempts; status is ERROR |

A `first_token` event is recorded at `RequestedAt + TTFT` when TTFT is known.

Successful spans keep status UNSET, as the OpenTelemetry spec recommends for instrumentation. Only failures set ERROR.

## Privacy

- Prompts, completions, and request and response bodies are **never** exported.
- `UsageRecord.APIKey` contains the **raw client API key** (the authenticated principal). It is exported only as `sha256:` plus the first 12 hex characters of its SHA-256. That is enough to group traffic by client and cannot be reversed for high-entropy keys.
- Credential identity is limited to `auth_index` and `auth_type`. Auth IDs and file names (often e-mail addresses), `Source`, upstream response headers and request metadata are not exported.
- Before export, error text from upstream failure bodies and completion errors is passed through redaction patterns and then truncated to 1024 bytes. Matches are replaced: `Bearer`/`Basic` credentials, `sk-…` keys, `AIza…` keys, JWT-looking tokens, and `api_key=`/`token:`/`password=`-style pairs. Set `capture_error_body: false` to drop error text entirely; status codes are still exported.
- `cpa.base_url` drops userinfo, query string and fragment.
- Exporter header values and endpoint query strings never appear in logs.

## Operational notes

- Export failures and drop counters are logged through the host log (`host.log`), at most once per minute per kind.
- Retries: network errors and HTTP 429/502/503/504 are retried twice with exponential backoff (250 ms, 500 ms). `Retry-After` is honored up to 5 s. After that the batch is dropped and counted. Other 4xx responses are not retried. OTLP `partialSuccess.rejectedSpans` is counted.
- `plugin.quiesce`, `plugin.shutdown` and the native shutdown hook flush all buffered traces and queued spans within 5 s.
- `plugin.reconfigure` applies the new settings without dropping buffered data. Setting an empty endpoint clears the buffers.
- The plugin answers with schema version `min(host, 10)`, so it registers on older hosts too. `request.complete` needs host schema 2 or newer; without it every trace gets a synthesized root after `orphan_timeout_ms`.

## Limitations

- Attributes on an exported root (`cpa.attempt_count`, token roll-ups) do not include usage records that arrive after the trace was exported. Those late records still attach to the root as child spans.
- Requests that CPA rejects before execution produce a root span with no children. Credentials skipped because of cool-down produce no attempt span, because CPA emits no usage record for them.
- When more than one completion with the same TraceID has a window containing a usage record's start time, the most recent of those completions gets the record. Turns on one WebSocket connection do not overlap in practice.
- Only OTLP/HTTP JSON is supported, without gzip compression. OTLP/gRPC and protobuf are not.
- When `max_pending_traces` is reached, the oldest trace is exported early, so a slow late usage record may become a separate late child span.

## Development

```bash
gofmt -l .
go vet ./...
go test -p 2 ./...          # CGO_ENABLED=0 works too; the C ABI glue is behind the cgo build tag
CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags "-s -w" -o dist/windows_amd64/llm-tracing.dll .
```

Tags `v*` trigger `.github/workflows/release.yml`. It builds windows/linux/darwin for amd64 and arm64 and publishes `llm-tracing_<version>_<goos>_<goarch>.zip` (library at the zip root) plus `checksums.txt`.

## License

MIT
