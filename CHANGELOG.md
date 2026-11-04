# Changelog

## 0.2.0

### Added

- Built-in **Traces page** in the CPA Management Center (plugin page **LLM Tracing**, served from `/v0/resource/plugins/llm-tracing/dashboard`). It shows last-hour summary cards (requests, error rate, average and p95 latency, tokens, retried requests), a filterable list of recent requests, and per-request detail with an attempt waterfall and per-attempt attributes. It asks for the management key, keeps it in memory (optionally `sessionStorage` for the tab), and stops auto-refresh after a 401 or 403.
- Management API routes `GET /v0/management/plugins/llm-tracing/summary`, `/traces` and `/trace` (authenticated by CPA with the management key). See the README API reference.
- Config keys `ui_enabled` (default `true`) and `ui_max_traces` (default `500`, 10..5000).
- `management_api` plugin capability.
- Cache-aware token accounting: per trace and per attempt `total_input_tokens` (the real prompt size: for Claude input + cache reads + cache writes; for OpenAI-style and Gemini providers, whose input already includes cached tokens, the input itself), `cache_read_tokens`, `cache_creation_tokens` and `cache_hit_rate`. Exported spans get `cpa.usage.total_input_tokens` (root and attempts). The page's "Tokens in" now shows total input, a **Cache** column shows reads and writes, the summary has a cache hit rate card, and the detail panel shows a per-attempt breakdown (uncached, cache read, cache write, output, reasoning).
- **Provider** and **Account** columns (of the serving attempt) and per-attempt provider and account in the detail panel, plus a provider filter (`provider=` on `/traces`, `providers` on `/summary`). Account labels come from `host.auth.list` (e-mail, label or auth file name, cached for 60 s), falling back to `<auth type> · <short auth index>`; API-key credentials from `config.yaml` always use the fallback.
- Config keys `ui_show_account_labels` (default `true`) and `export_account_labels` (default `false`; adds `cpa.account` to exported attempt spans).

### Changed

- The tracer now runs whenever the plugin is enabled, even with an empty `endpoint`. The OTLP exporter runs only when an endpoint is set. With an empty endpoint and `ui_enabled: false` the plugin is a no-op as before.
- `sample_rate` applies to export only. The Traces page shows every request; with the page off, sampled-out traces are still dropped before buffering.
- Usage records that arrive after a trace was finalized also update the stored trace.
- `gen_ai.usage.input_tokens` and the API's `input_tokens` keep the provider-reported value (for Claude, uncached tokens only); use `total_input_tokens` for the prompt size.
- Page layout: the header keeps its title and connection controls (exporter status, **Disconnect**) on the left and reserves the top-right corner, where the Management Center draws its toolbar. Filter and refresh controls flow from the left; low-priority columns hide on narrow windows and the table scrolls inside its card.

### Security

- Stored traces are built from the same sanitized spans as the export (masked client key, redacted and truncated errors, no Auth IDs, `Source`, response headers, prompts or responses).
- Account labels are resolved only for the management-authenticated page and are not stored with traces or exported unless `export_account_labels: true`. `UsageRecord.AuthID`, `Source` and the host's `account` field (raw key for API-key credentials) are never read; label candidates that look like secrets are skipped.
- Page assets are static, embedded with `go:embed`, and served with a strict Content-Security-Policy (no inline script or style, `connect-src 'self'`, `frame-ancestors 'self'`), `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`. The page builds the DOM with `textContent` only.

## 0.1.0

- Initial release: OTLP/HTTP JSON exporter joining `request.complete` (root span) and `usage.handle` (attempt spans) by TraceID.
