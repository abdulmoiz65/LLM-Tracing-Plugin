# Changelog

## 0.4.0

### Added

- **Insights** card on the Traces page, for the selected time range:
  - **Cache rewrites**: requests that had to rewrite most of a conversation's prompt cache instead of reading it, with the extra cost (write price minus read price). Rewrites on a different account than the previous request are counted as "moved to another account" (caches are per account, so round-robin across accounts rebuilds them). The rest are split honestly into rewrites within 5 minutes of the previous request (the cached prefix changed, for example a timestamp or a changed tool list) and rewrites after an idle gap (the cache expired or the prefix changed; the plugin cannot tell). Click one to open the request.
  - **Large context**: what re-reading conversation history above 200K tokens cost, per session. Click one to show its requests.
  - **Retries**: requests that needed more than one attempt, the waiting time retries added, and failures per account. Failed attempts are usually not billed, so this reports time, not money.
  - **What if**: the same recorded tokens re-priced on another model, with the caveats (tokenizers and quality differ; switching models mid-conversation rebuilds the cache).
- API: `GET /v0/management/plugins/llm-tracing/insights?range=` and `GET .../whatif?range=&model=`.
- Every Insights box has a "What is this?" explanation. Amounts say "extra"; the retries box is now "Failures & retries" ("2 failed · 0 retried"); long account names wrap onto two lines.

Insights are computed on request from the existing history in one pass (about 2 to 4 ms and 1 MB of short-lived memory at 20,000 requests) and keep no state of their own. The page refreshes them every 30 s.

### Changed


- Plugin logo and author byline (a 3 KB avatar served by the plugin; the Plugins page and the store show the logo from the repository).
- Cleaner cost card: one headline number (the upper estimate) with "min $x · saved $y" underneath, instead of a two-number range with a "+". Table rows show the plain value; a `*` marks a partial total. Details stay in the tooltips.

## 0.3.1

### Changed

- **Costs are fixed when a request is recorded.** Each attempt keeps the price that applied then (saved with the history), so a later price update, a new plugin release or a `pricing` change only affects new requests and never rewrites past costs. Attempts without a price stay open and are priced once a price exists (the source then says "priced later"). History saved by 0.3.0 is priced once on load and saved again with fixed prices.
- **Cost range for Claude subscription cache writes.** The proxy does not tell plugins whether a cache write used the 5-minute or the 1-hour cache, so with `cache_write_ttl: auto` the cost card shows a range (5-minute to 1-hour price), and rows show the upper value marked `≤`. New `cost_min_usd` on `/summary`, `/traces` and `/sessions`.
- **Canceled requests without usage show "unknown"** instead of $0: the provider may still have billed the part it generated. They are counted in `unknown_cost_requests` and marked `+` on totals.
- The cost card is now labelled **Est. API cost**; its tooltip says it is what the tokens would cost through the API, not a bill.
- **Outdated price warning**: `/summary` reports `prices_age_days` and `prices_stale`; after 30 days the cost card asks to update the plugin or set `pricing`.
- Price sources include the price date (for example `litellm 2026-11-07: openai`), shown in the tooltip and the detail panel.
- Price table refreshed from LiteLLM (2026-11-07, 1,813 models).

## 0.3.0

### Added

- **Time range** picker on the Traces page (15 minutes to 7 days, or all traces kept in memory; default all). The summary cards, sessions and request list follow it, and a line under the picker says which requests the numbers cover. In 0.2.0 the cards always showed the last hour while the list showed everything, which looked like a mismatch.
- **Estimated cost** at API list prices: a cost card with the prompt-cache savings, an Est. cost column, per-request and per-attempt cost in the detail panel, and cost per session. Prices for about 1,700 models: a verified built-in Claude table plus a table generated from LiteLLM's price list (OpenAI/Codex, Gemini, Vertex, Antigravity, DeepSeek, xAI, Kimi, GLM, MiniMax, Mistral, Meta and more), with long-context tiers. The cost tooltip names the price source. Override or add prices with the new `pricing` config key; `tools/genprices` refreshes the generated table. Cache writes are priced at the 1-hour rate for Claude OAuth accounts and the 5-minute rate otherwise (`cache_write_ttl`).
- **Sessions**: requests are grouped into estimated conversations (same client key and model, prompt keeps growing, no long pause). The sessions table shows context used against the model's context window (warning from 70%, red from 90%), growth per request, an estimate of how many requests still fit, and cost. Click a session to filter the request list.
- **Context** column on requests (serving prompt against the context window). Built-in context windows for current Claude models; others via `context_windows`.
- API: `range` on `/summary`, `/traces` and `/sessions`; `session` filter on `/traces`; new `GET /sessions` route; cost, context and session fields on items. Config keys `pricing`, `context_windows`, `cache_write_ttl` and `session_gap_minutes`.

- **Saved history**: the Traces page history is written to `data_dir` (default `plugins/llm-tracing`, one file per day) and loaded again at start, so proxy restarts and plugin updates no longer reset cost and sessions. New keys `save_history` (default `true`), `data_dir`, `history_days` (default 7) and `history_max_requests` (default 20000). `/summary` reports `history` (folder, days, loaded, written and dropped records, last error) and `detail_traces`; items have `has_detail`.
- **Canceled** requests (the client stopped them) are counted separately (`canceled` on `/summary` and `/sessions`, `status=canceled` filter) and no longer count as errors.

### Changed

- The history now covers every request of the last `history_days`, not just the newest `ui_max_traces`. Totals, cost, sessions and the 24-hour and 7-day ranges use the whole history; `ui_max_traces` (default 500) now bounds only the requests that keep full attempt details. `capacity` on `/summary` is the history limit.
- `/summary` without `range` still covers the last hour, so existing API clients see the same numbers.

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
