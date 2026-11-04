# llm-tracing

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) plugin that turns every proxied LLM request into an **OpenTelemetry trace**. It exports traces over **OTLP/HTTP (JSON)** to any OTLP backend (Langfuse, Jaeger, Grafana Tempo, Datadog, Honeycomb, or an OpenTelemetry Collector) and shows the most recent requests on a **built-in Traces page** in the CPA Management Center, so you can inspect them without running a tracing backend.

- **One trace per inbound request.** A `SERVER` root span covers the client request. Each upstream attempt (including retries and failovers) is a `CLIENT` child span with tokens, latency, time-to-first-token, provider, model and credential index.
- **No added latency.** The host delivers `usage.handle` and `request.complete` asynchronously. The plugin only decodes the event and appends it to an in-memory buffer. Batching, HTTP export and retries run on one background goroutine.
- **Privacy-safe defaults.** It never exports request or response bodies. The client API key is exported only as a SHA-256 fingerprint. Upstream error bodies are redacted and truncated. Upstream URLs lose their credentials and query strings.
- **Bounded memory.** Pending traces, queued spans, attempts per request and the Traces page buffer all have hard caps. When a cap is hit, the oldest data goes first and the drop is counted.
- **Built-in Traces page.** Management Center > **LLM Tracing** lists recent requests with status, latency, attempts, tokens and an attempt waterfall. It works with or without an OTLP endpoint.

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

No key is required. With an empty `endpoint` the OTLP exporter is off and the plugin only keeps recent traces for the Traces page. With an empty `endpoint` and `ui_enabled: false` the plugin is a no-op and logs that once.

| Key | Default | Description |
|---|---|---|
| `enabled`, `priority` | host | Standard CPA plugin keys. |
| `endpoint` | — | OTLP/HTTP traces URL (the full path, usually ending in `/v1/traces`). Empty disables export only. |
| `headers` | `{}` | Extra exporter headers. Either a map, or a list of `"Key: Value"` strings or `{name, value}` items. Values are never logged. |
| `service_name` | `cliproxyapi` | `service.name` resource attribute. |
| `sample_rate` | `1.0` | Fraction of traces exported (0..1). The decision is deterministic per TraceID, so a trace is always kept or dropped whole. It applies to the exporter only: the Traces page shows every request. |
| `completion_grace_ms` | `2000` | Wait for late usage records after a completion. |
| `orphan_timeout_ms` | `600000` | Export usage records whose completion never arrived. This is also how long exported requests stay joinable by late records. |
| `capture_error_body` | `true` | Attach redacted, truncated (1024 bytes) error text to failed spans. |
| `max_pending_traces` | `10000` | Requests buffered while waiting. When exceeded, the oldest is exported early. |
| `max_queue_spans` | `20000` | Finished spans awaiting export. When exceeded, the oldest are dropped. |
| `flush_interval_ms` | `1000` | Export interval. |
| `max_batch_spans` | `512` | Spans per export request. |
| `export_timeout_ms` | `10000` | HTTP timeout per export attempt. |
| `ui_enabled` | `true` | Keep recent traces in memory for the built-in Traces page. |
| `ui_max_traces` | `500` | Traces kept for the page (10..5000). The oldest is dropped first. |
| `ui_show_account_labels` | `true` | Show a readable account label (e-mail, label or auth file name) for each credential on the Traces page. Off: the page shows only `<auth type> · <short auth index>`. See [Account labels](#account-labels). |
| `export_account_labels` | `false` | Also export the account label as `cpa.account` on attempt spans. Labels are often e-mail addresses, so this is off by default. |

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

## Built-in Traces page

Open the CPA Management Center and pick **LLM Tracing** from the plugin pages. The Management Center loads the page in a same-origin iframe from `/v0/resource/plugins/llm-tracing/dashboard`. You can also open that URL directly.

1. Enter the CPA management key and click **Connect**. The page cannot read the Management Center's login, so it asks for the key itself. The key stays in the page's memory. Tick **Remember for this tab** to keep it in `sessionStorage` until the tab closes. It is never put in a URL, a cookie or `localStorage`.
2. The cards show the last hour: requests, error rate, average and p95 latency, tokens in/out (input includes cache reads and writes), cache hit rate and retried requests.
3. Filter by status (All / OK / Errors), by model (substring of the requested model), by provider (the serving attempt's provider), or to slow requests only (over 10 s). Auto-refresh reloads every 5 s and pauses while the tab is hidden.
4. Click a row to see the attempt waterfall (failed attempts in red, first-token markers) and, for every attempt, its provider and account, model, response model, status code, token breakdown (total input, uncached, cache read, cache write, output, reasoning), cache hit rate, TTFT, auth type and index, and error text. A 429 on one account followed by a success on another shows as two attempts with different accounts.

Table columns:

| Column | Content |
|---|---|
| Time, Model, Status, Duration, Attempts | as before; Attempts shows `n (k failed)` |
| Provider | provider of the serving attempt (the last successful attempt, or the last attempt when all failed), e.g. `claude`, `codex`, `openai-compatible-<name>` |
| Account | account label of the serving attempt's credential (see [Account labels](#account-labels)) |
| Tokens in / out | total input (uncached + cache read + cache write) / output, summed over attempts. The tooltip shows the breakdown, e.g. `145 uncached + 12,288 cache read + 1,024 cache write = 13,457 input tokens` |
| Cache | `12.3k read · 1.1k write`, or `-` without cache use; the tooltip shows the hit rate |
| Client key | `sha256:` fingerprint of the client key |

On narrower windows the Client key column is hidden first (below 1180 px), then Provider and Cache (below 900 px). The table scrolls horizontally inside its card when it still does not fit; the page itself never scrolls sideways.

Layout: the Management Center draws its own toolbar (refresh, language, theme, logout) over the top-right corner of the plugin page. The page keeps that corner free: the title, the exporter status and the **Disconnect** button sit on the left of the header, the header reserves 256 px on the right and is tall enough that the first card starts below the toolbar, and below 720 px width the whole header moves under the toolbar. Filters and buttons flow from the left, and the trace detail scrolls into view with a top margin so its buttons are not hidden.

What the page shows:

- **Every request, regardless of `sample_rate`.** Sampling limits what is sent to the OTLP backend, not what you see locally. Requests with no model and no upstream attempt (for example `/v1/models` listings) are skipped, the same as for export.
- **The same sanitized data as the exported spans.** Each stored trace is built from the spans handed to the exporter: masked client key, redacted and truncated error text, no Auth IDs, `Source` or response headers, and never prompts or responses. Stored string attributes are capped at 512 bytes, and the list's `error` column at 256 bytes. Langfuse mapping attributes (`langfuse.*`) duplicate other keys and are not stored.
- **Trace and span IDs match the OTLP export**, so a trace ID copied from the page finds the same trace in Jaeger or Langfuse (when it was sampled in).
- Usage records that arrive after a trace was finalized update the stored trace (attempt count, tokens, waterfall).
- Requests without a `request.complete` event (synthesized roots, usage records without TraceID) have outcome `unknown`.

### Token semantics

CPA passes the plugin the raw per-attempt counters (`UsageDetail`), and providers count cached tokens differently:

| Provider family | `InputTokens` from CPA | Total input (prompt size) |
|---|---|---|
| Claude / Anthropic (`claude`, anything matching `claude`/`anthropic`) | uncached tokens only; `cache_read_input_tokens` and `cache_creation_input_tokens` are separate | `input + cache_read + cache_creation` |
| OpenAI chat/responses, Codex, OpenAI-compatible (`openai-compatible-<name>`), xAI, Kimi, Qwen, DeepSeek, OpenRouter | `prompt_tokens` / `input_tokens`, which already include `cached_tokens` | `input` (cached tokens are not added again) |
| Gemini, AI Studio, Vertex, Antigravity | `promptTokenCount`, which includes `cachedContentTokenCount` | `input` |
| other / unknown | unknown | the largest of `input` and `cache_read + cache_creation` (a lower bound) |

The table mirrors CPA's own token accounting (`tokenAccountingSemanticsFor` in `sdk/cliproxy/usage/accounting.go`), decided from the attempt's provider and executor. When an attempt reports only `cached_tokens`, it counts as cache read.

Fields (API, per trace and per attempt):

- `input_tokens`: the provider-reported value, unchanged from v0.1.0. For Claude it excludes the cache, so a large cached prompt can show as a few hundred tokens.
- `total_input_tokens`: the true prompt size computed as above. The page shows this as "Tokens in".
- `cache_read_tokens`, `cache_creation_tokens`: cache reads and writes (`uncached_input_tokens = total_input_tokens - cache_read - cache_creation` per attempt).
- `cache_hit_rate`: `cache_read_tokens / total_input_tokens`, rounded to 4 decimals, 0 when there is no input.
- `cached_tokens`: kept for compatibility: per attempt the larger of CPA's `CachedTokens` and `CacheReadTokens`. For Claude, CPA sets `CachedTokens` to the cache reads, or to the cache writes when there are no reads, so prefer the two explicit fields.

### Account labels

Each attempt records the credential's `auth_index` (a stable hash) and `auth_type` (`oauth` or `apikey`). The plugin never uses `UsageRecord.AuthID` or `Source`: `AuthID` is often an auth file name containing an e-mail address, and for API-key credentials `Source` is the raw upstream API key.

With `ui_show_account_labels: true` (default), the management handler resolves `auth_index` to a label with the host callback `host.auth.list`, the same list the Management Center's Auth Files page shows to the same administrator. The result is cached for 60 s; a failed lookup keeps the previous labels and is retried after 60 s. The callback is never made on the request path, only while answering a page request (and, with `export_account_labels: true`, from the plugin's background loop).

- **OAuth and other file-based credentials:** the entry's e-mail, else its label, else the auth file name without `.json`.
- **API keys from `config.yaml`** (for example `openai-compatibility`, `claude-api-key`): CPA does not return them from `host.auth.list`, so the label is `<auth type> · <first 8 characters of the auth index>`, for example `apikey · a72c126e`. The provider column already names the `openai-compatibility` entry (`openai-compatible-<name>`). The raw key is never shown: the host's `account` field (which holds the raw key for API-key credentials) is not read, and candidate labels that look like secrets (`sk-…`, `AIza…`, bearer tokens, `key=value` pairs, or a run of 24+ letters and digits) are skipped.
- **Callback unavailable or failing:** the same `<auth type> · <short index>` fallback. `/summary` reports `account_labels` (`enabled`, `known_credentials`, `last_refresh_at`, `last_error`).

Labels are computed when the page asks and are not stored with the traces. With `ui_show_account_labels: false` the page shows only the fallback form and `host.auth.list` is not called for the page.

Labels are not exported by default. With `export_account_labels: true` each exported attempt span gets `cpa.account`, read from the cached list (the background loop refreshes it every 60 s while exporting); attempts recorded before the first refresh get the fallback form. The root span never carries it.

Memory: traces live in a ring of `ui_max_traces` entries (newest kept). Each stored trace keeps at most 16 attempt spans; counters still include the rest. A typical trace takes a few KB, so the default 500 traces use a few MB. Data is lost on restart.

Wrong keys: CPA bans an IP for 30 minutes after 5 failed management-key attempts. The page stops auto-refresh after a 401 or 403 and asks for the key again instead of retrying.

The page and its API are unavailable when CPA runs in Home mode (`home.enabled`), because CPA disables management routes and plugin resources there.

### API reference

The page uses three plugin Management API routes. The host authenticates them with the management key (`Authorization: Bearer <key>` or `X-Management-Key`) before the plugin sees the request; a missing or wrong key gets 401 from CPA. CPA only allows plugin management routes under `/v0/management/`; they are not mirrored under `/v8/management/`. All responses are JSON with `Content-Type: application/json; charset=utf-8` and `Cache-Control: no-store`. Only GET is supported.

`P = /v0/management/plugins/llm-tracing`

| Route | Response |
|---|---|
| `GET P/summary` | `{"version","window_seconds":3600,"requests","errors","error_rate","avg_latency_ms","p95_latency_ms","input_tokens","total_input_tokens","cache_read_tokens","cache_creation_tokens","cache_hit_rate","output_tokens","retried_requests","stored_traces","capacity","providers":[...],"exporter":{"enabled","endpoint_host","exported_spans","dropped_spans","last_error","last_export_at"},"account_labels":{"enabled","known_credentials","last_refresh_at","last_error"}}` |
| `GET P/traces?limit=50&status=all\|ok\|error&model=<substring>&provider=<name>&min_ms=<n>` | `{"traces":[item...]}`, newest start time first |
| `GET P/trace?id=<32 hex>` | `{"trace": item + "spans":[span...]}` |

- **Summary** covers stored traces that started in the last hour (so at most `ui_max_traces`). `error_rate` is `errors / requests` (0 with no requests). `p95_latency_ms` uses the nearest-rank method. `retried_requests` counts traces with more than one attempt, whatever the outcome. Token fields sum the traces in the window (see [Token semantics](#token-semantics)); `cache_hit_rate` is computed from those sums. `providers` lists the serving providers of all stored traces (sorted, at most 50) for the provider filter. `account_labels` describes the label cache. `exporter.enabled` is false when no endpoint is set; `endpoint_host` is `host:port` only; `dropped_spans` sums queue-full, failed-export and backend-rejected spans; `last_error` is the redacted error of the last failed export and is cleared by the next successful one; `last_export_at` is the RFC 3339 time of the last successful export.
- **Item:** `trace_id` (OTel trace ID), `cpa_trace_id`, `started_at` (RFC 3339 nano, UTC), `duration_ms`, `requested_model`, `source_format`, `stream`, `outcome` (`succeeded`, `failed`, `rejected`, `canceled`, `unknown`), `status_code`, `attempt_count`, `failed_attempts`, `input_tokens` (provider-reported), `total_input_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `cache_hit_rate`, `output_tokens`, `reasoning_tokens`, `cached_tokens` (compatibility; per attempt the larger of cached and cache-read tokens), `provider` and `account` (of the serving attempt), `client_key` (`sha256:...`), `error` (the request error, or for a failed request without one the last failed attempt's error; empty for succeeded requests).
- **Span:** `span_id`, `parent_span_id` (empty for the root), `name`, `kind` (`server` or `client`), `start_offset_ms` (from the root start), `duration_ms`, `status` (`error`, or `unset` for successes, following the OpenTelemetry convention); attempt (client) spans also have `provider`, `account` and `tokens` (`{"input_tokens","total_input_tokens","uncached_input_tokens","cache_read_tokens","cache_creation_tokens","cache_hit_rate","output_tokens","reasoning_tokens"}`); `attributes` (the exported attributes except `cpa.account`; values are strings, numbers or booleans), `events` (`[{"name","offset_ms"}]`, offset from the root start). The root comes first, then attempts by start time.
- **Query parameters:** `limit` defaults to 50; values below 1 become 1 and values above 500 become 500. `min_ms` below 0 becomes 0. `status` is `all` (default), `ok` or `error`; `error` means outcome not `succeeded` or status code 400 or higher. `model` is a case-insensitive substring of `requested_model`. `provider` matches the serving attempt's provider exactly, ignoring case. A `limit` or `min_ms` that is not a number, or an unknown `status`, gets 400. A malformed `id` gets 400; an unknown one gets 404 `{"error":"trace not found"}`.
- **Errors** are `{"error":"message"}`. With `ui_enabled: false` every route answers 404 with an error that names `ui_enabled`.

The page assets are `/v0/resource/plugins/llm-tracing/dashboard`, `/dashboard.css` and `/dashboard.js`. Resource routes are not authenticated by CPA, so they serve only the static page embedded in the plugin and never trace data.

### Security notes

- The page treats every value from the API as untrusted (model names, error text and attribute values can come from clients or upstreams). It builds the DOM with `createElement` and `textContent` only, never `innerHTML`, `eval` or HTML strings. JSON responses escape `<`, `>` and `&`.
- The page, stylesheet and script are served with `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and `Cache-Control: no-store`. CPA adds no security headers to plugin resources, so the plugin sets them. There is no inline script or style, and nothing is loaded from another origin.
- API calls go only to the page's own origin and use `credentials: 'omit'` and `cache: 'no-store'`. The API base is derived from the page URL, so CPA behind a path prefix works.

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
| `cpa.usage.total_input_tokens` | sum of the attempts' total input (cache included once) |
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
| `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | provider-reported token counts (see below) |
| `cpa.usage.total_input_tokens` | total input: uncached + cache read + cache write, cache counted once ([Token semantics](#token-semantics)) |
| `cpa.usage.total_tokens`, `cpa.usage.{reasoning,cached,cache_read,cache_creation}_tokens` | extra counters (non-zero only) |
| `cpa.attempt` | 1-based ordinal by start time |
| `cpa.request_id`, `cpa.trace_id` | attempt ID / join key |
| `cpa.alias` | model alias the client used |
| `cpa.executor`, `cpa.base_url` (no credentials or query) | upstream route |
| `cpa.auth_index`, `cpa.auth_type` | credential used (index only, never the credential) |
| `cpa.account` | account label, only with `export_account_labels: true` |
| `cpa.client_key` | `sha256:<12 hex>` fingerprint of the client key |
| `cpa.access_provider`, `cpa.session_id`, `cpa.parent_session_id` | client identity context |
| `cpa.reasoning_effort`, `cpa.service_tier`, `cpa.effective_service_tier`, `cpa.response_service_tier` | request options |
| `cpa.stream`, `cpa.ttft_ms` | streaming and time to first token |
| `langfuse.observation.type=generation` | Langfuse mapping |
| `http.response.status_code`, `error.type`, `cpa.error`, `langfuse.observation.level`, `langfuse.observation.status_message` | failed attempts; status is ERROR |

A `first_token` event is recorded at `RequestedAt + TTFT` when TTFT is known.

`gen_ai.usage.input_tokens` keeps the value the provider reported, as in v0.1.0. The OpenTelemetry GenAI conventions recommend that it include cached tokens; for OpenAI-style and Gemini providers it does, but for Claude it does not (Anthropic reports cache reads and writes separately). It is left unchanged because Langfuse and other backends read `gen_ai.usage.*` for token and cost accounting, and changing it would silently change their numbers for existing users. The convention-style total is in `cpa.usage.total_input_tokens`, which those backends ignore. For the same reason the plugin does not emit `gen_ai.usage.cache_read.input_tokens` / `gen_ai.usage.cache_creation.input_tokens`; the cache counts are in `cpa.usage.cache_read_tokens` and `cpa.usage.cache_creation_tokens`.

Successful spans keep status UNSET, as the OpenTelemetry spec recommends for instrumentation. Only failures set ERROR.

## Privacy

- Prompts, completions, and request and response bodies are **never** exported.
- `UsageRecord.APIKey` contains the **raw client API key** (the authenticated principal). It is exported only as `sha256:` plus the first 12 hex characters of its SHA-256. That is enough to group traffic by client and cannot be reversed for high-entropy keys.
- Credential identity is limited to `auth_index` and `auth_type`. Auth IDs and file names (often e-mail addresses), `Source` (the e-mail, or the raw upstream key for API-key credentials), upstream response headers and request metadata are not exported.
- Account labels (e-mail, label or auth file name from `host.auth.list`) are shown only on the management-authenticated Traces page, are not stored with traces, and are exported (`cpa.account`) only when `export_account_labels: true`. A raw API key is never used as a label.
- Before export, error text from upstream failure bodies and completion errors is passed through redaction patterns and then truncated to 1024 bytes. Matches are replaced: `Bearer`/`Basic` credentials, `sk-…` keys, `AIza…` keys, JWT-looking tokens, and `api_key=`/`token:`/`password=`-style pairs. Set `capture_error_body: false` to drop error text entirely; status codes are still exported.
- `cpa.base_url` drops userinfo, query string and fragment.
- Exporter header values and endpoint query strings never appear in logs.

## Operational notes

- Export failures and drop counters are logged through the host log (`host.log`), at most once per minute per kind.
- Retries: network errors and HTTP 429/502/503/504 are retried twice with exponential backoff (250 ms, 500 ms). `Retry-After` is honored up to 5 s. After that the batch is dropped and counted. Other 4xx responses are not retried. OTLP `partialSuccess.rejectedSpans` is counted.
- `plugin.quiesce`, `plugin.shutdown` and the native shutdown hook flush all buffered traces and queued spans within 5 s.
- `plugin.reconfigure` applies the new settings without dropping buffered data. Setting an empty endpoint clears the export queue; setting `ui_enabled: false` clears the Traces page buffer; doing both clears everything.
- Page requests never block request handling: usage and completion events are delivered asynchronously, and the page store has its own lock.
- The plugin answers with schema version `min(host, 10)`, so it registers on older hosts too. `request.complete` needs host schema 2 or newer; without it every trace gets a synthesized root after `orphan_timeout_ms`.

## Limitations

- Attributes on an exported root (`cpa.attempt_count`, token roll-ups) do not include usage records that arrive after the trace was exported. Those late records still attach to the root as child spans. The Traces page counters do include them.
- The Traces page keeps data in memory only, per CPA process, and loses it on restart.
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
