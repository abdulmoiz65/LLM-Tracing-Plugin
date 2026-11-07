// Command llm-tracing is a CLIProxyAPI native plugin that turns every
// proxied LLM request into an OpenTelemetry trace. Traces are exported over
// OTLP/HTTP JSON and the most recent ones are shown on a built-in Traces page
// in the Management Center.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	pluginID      = "llm-tracing"
	pluginName    = "LLM Tracing"
	pluginVersion = "0.4.1"
	pluginAuthor  = "abdulmoiz65"
	pluginRepo    = "https://github.com/abdulmoiz65/LLM-Tracing-Plugin"
	// pluginLogo is shown by the Management Center's Plugins page and the
	// Plugin Store (an image URL, like other store plugins use).
	pluginLogo = "https://raw.githubusercontent.com/abdulmoiz65/LLM-Tracing-Plugin/main/assets/logo.png"

	// pluginSchemaVersion is the newest RPC schema this plugin was written
	// against (sdk/pluginabi.SchemaVersion). The plugin only needs schema 2+
	// (request.complete), so it answers with the host's version when older.
	pluginSchemaVersion uint32 = 10

	shutdownTimeout = 5 * time.Second
	maxTick         = 250 * time.Millisecond
)

func main() {}

// pluginState owns the tracer and the single background loop.
type pluginState struct {
	mu       sync.Mutex
	tracer   *tracer
	autoLoop bool
	running  bool
	stop     chan struct{}
	done     chan struct{}
	loopCtx  context.Context
	cancel   context.CancelFunc
	modeLog  string
	// history saves the Traces page history to disk. Only the real plugin
	// (autoLoop) writes files; unit tests use historyFile directly.
	history *historyFile
}

func newPluginState(now func() time.Time, logger func(level, message string), autoLoop bool) *pluginState {
	cfg := defaultConfig()
	t := newTracer(cfg, now)
	if logger != nil {
		t.logger = logger
	}
	return &pluginState{tracer: t, autoLoop: autoLoop}
}

// plug is the process-wide plugin instance used by the C ABI.
var plug = newPluginState(time.Now, func(level, message string) { hostLog(level, message) }, true)

func (p *pluginState) configure(cfg config, warnings []string) {
	p.tracer.reconfigure(cfg)
	for _, w := range warnings {
		p.tracer.logger("warn", "llm-tracing config: "+w)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mode := ""
	switch {
	case !cfg.recording():
		mode = "llm-tracing: disabled, or no endpoint and ui_enabled: false; tracing is a no-op"
	case !cfg.active():
		mode = "llm-tracing: no endpoint configured; OTLP export is a no-op, recent traces are kept for the Traces page"
	}
	if mode != "" && mode != p.modeLog {
		p.tracer.logger("info", mode)
	}
	p.modeLog = mode
	if p.autoLoop {
		p.syncHistoryLocked(cfg)
	}
	if cfg.recording() && p.autoLoop && !p.running {
		p.startLocked()
	}
}

// syncHistoryLocked starts, restarts or stops the history file for cfg. A
// new history file is loaded in the background and merged into the store.
func (p *pluginState) syncHistoryLocked(cfg config) {
	want := cfg.uiActive() && cfg.SaveHistory
	dir := cfg.DataDir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if p.history != nil && (!want || p.history.dir != dir || p.history.days != cfg.HistoryDays) {
		p.tracer.store.setPersist(nil)
		old := p.history
		p.history = nil
		go old.close()
	}
	if !want || p.history != nil {
		return
	}
	h := newHistoryFile(dir, cfg.HistoryDays, p.tracer.now)
	h.loading = true
	p.history = h
	p.tracer.store.setPersist(h.enqueue)
	go func() {
		traces, err := h.load()
		n := p.tracer.store.importHistory(traces)
		h.statMu.Lock()
		h.loading, h.loaded, h.imported = false, len(traces), n
		h.statMu.Unlock()
		if err != nil {
			h.setErr(fmt.Errorf("load history: %w", err))
			p.tracer.logger("warn", "llm-tracing: could not load the saved history: "+err.Error())
		} else if n > 0 {
			p.tracer.logger("info", fmt.Sprintf("llm-tracing: loaded %d saved requests from %s (%d new)", len(traces), dir, n))
		}
	}()
}

func (p *pluginState) currentHistory() *historyFile {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.history
}

func (p *pluginState) startLocked() {
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	p.loopCtx, p.cancel = context.WithCancel(context.Background())
	p.running = true
	go p.run(p.loopCtx, p.stop, p.done)
}

// run is the single background loop: it sweeps grace/orphan deadlines, flushes
// batches and reports drop counters. No per-trace goroutines or timers exist.
func (p *pluginState) run(ctx context.Context, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := p.tracer
	lastFlush := t.now()
	for {
		cfg := t.config()
		timer := time.NewTimer(min(cfg.FlushInterval, maxTick))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		now := t.now()
		if cfg.ExportAccountLabels && cfg.active() {
			// Keep the label cache warm for exported spans; the request path
			// only reads the cached snapshot.
			t.accounts.refresh(now)
		}
		t.sweep(now)
		if t.queueLen() >= cfg.MaxBatchSpans || now.Sub(lastFlush) >= cfg.FlushInterval {
			lastFlush = now
			// Drain in batches; stop at the first failure and retry next interval.
			for ctx.Err() == nil {
				n, err := t.flushOnce(ctx)
				if n == 0 || err != nil {
					break
				}
			}
		}
		t.reportStats(now)
		if h := p.currentHistory(); h != nil {
			h.flush()
		}
	}
}

// drain exports all pending traces and queued spans within timeout.
func (p *pluginState) drain(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	p.tracer.flushPending()
	p.tracer.flushAll(ctx)
	if h := p.currentHistory(); h != nil {
		h.flush()
	}
}

func (p *pluginState) quiesce() { p.drain(shutdownTimeout) }

// shutdown flushes with a bounded deadline, then stops the background loop.
func (p *pluginState) shutdown() {
	p.drain(shutdownTimeout)
	p.mu.Lock()
	running, stop, done, cancel := p.running, p.stop, p.done, p.cancel
	p.running = false
	history := p.history
	p.history = nil
	p.mu.Unlock()
	if history != nil {
		p.tracer.store.setPersist(nil)
		history.close()
	}
	if !running {
		return
	}
	cancel()
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

type configField struct {
	Name        string
	Type        string
	Default     any `json:",omitempty"`
	Description string
}

func registration(hostSchema uint32) map[string]any {
	schema := pluginSchemaVersion
	if hostSchema > 0 && hostSchema < schema {
		schema = hostSchema
	}
	return map[string]any{
		"schema_version": schema,
		"metadata": map[string]any{
			"Name":             pluginName,
			"Version":          pluginVersion,
			"Author":           pluginAuthor,
			"GitHubRepository": pluginRepo,
			"Logo":             pluginLogo,
			"ConfigFields":     configFields(),
		},
		"capabilities": map[string]bool{
			"usage_plugin":             true,
			"request_lifecycle_plugin": true,
			"management_api":           true,
		},
	}
}

func configFields() []configField {
	d := defaultConfig()
	return []configField{
		{"endpoint", "string", nil, "OTLP/HTTP traces URL, e.g. http://127.0.0.1:4318/v1/traces or https://cloud.langfuse.com/api/public/otel/v1/traces. Empty disables export; the Traces page still works."},
		{"headers", "object", nil, "Extra exporter HTTP headers (e.g. Authorization: Basic <base64 public:secret> for Langfuse, x-honeycomb-team, DD-API-KEY). A map, or a list of \"Key: Value\" strings. Values are never logged."},
		{"service_name", "string", d.ServiceName, "OpenTelemetry service.name resource attribute."},
		{"sample_rate", "number", d.SampleRate, "Fraction of traces to keep (0..1). Decided per request trace ID, so all spans of a trace are kept or dropped together."},
		{"completion_grace_ms", "integer", d.CompletionGrace.Milliseconds(), "How long to wait after a request completes for late usage records before exporting its trace."},
		{"orphan_timeout_ms", "integer", d.OrphanTimeout.Milliseconds(), "Usage records whose request completion never arrives are exported with a synthesized root span after this delay."},
		{"capture_error_body", "boolean", d.CaptureErrorBody, "Attach upstream error bodies to failed spans (redacted, truncated to 1024 bytes)."},
		{"max_pending_traces", "integer", d.MaxPendingTraces, "Maximum traces buffered while waiting for completion/usage; the oldest is exported early when exceeded."},
		{"max_queue_spans", "integer", d.MaxQueueSpans, "Maximum finished spans queued for export; the oldest are dropped when exceeded."},
		{"flush_interval_ms", "integer", d.FlushInterval.Milliseconds(), "Export interval for queued spans."},
		{"max_batch_spans", "integer", d.MaxBatchSpans, "Maximum spans per OTLP export request."},
		{"export_timeout_ms", "integer", d.ExportTimeout.Milliseconds(), "HTTP timeout for one export attempt."},
		{"ui_enabled", "boolean", d.UIEnabled, "Keep recent traces in memory for the built-in Traces page (Management Center > LLM Tracing). Shows every request regardless of sample_rate."},
		{"ui_max_traces", "integer", d.UIMaxTraces, "How many of the newest requests keep full attempt details (waterfall, attributes) on the Traces page (10..5000). Older requests keep their totals."},
		{"ui_show_account_labels", "boolean", d.UIShowAccountLabels, "Show a readable account label (e-mail, label or auth file name from the host's auth list) for each attempt on the Traces page. Off: only auth type and a short auth index. Never exported unless export_account_labels is on."},
		{"export_account_labels", "boolean", d.ExportAccountLabels, "Also add the account label as cpa.account to exported attempt spans. Off by default because labels are often e-mail addresses."},
		{"pricing", "object", nil, "Extra or overriding model prices for the cost estimate, USD per million tokens: {<model>: {input, output, cache_read, cache_write}}. cache_read defaults to 10% and cache_write to 125% of input. Claude models are built in."},
		{"context_windows", "object", nil, "Extra or overriding context window sizes in tokens: {<model>: <tokens>}. Claude models are built in."},
		{"cache_write_ttl", "string", d.CacheWriteTTL, "Price of cache writes: auto (1-hour rate for Claude OAuth accounts, 5-minute rate otherwise), 5m or 1h."},
		{"save_history", "boolean", d.SaveHistory, "Save the Traces page history to data_dir so it survives proxy restarts and plugin updates (same sanitized data as the page; no prompts, responses or keys)."},
		{"data_dir", "string", d.DataDir, "Folder for the saved history, relative to the proxy's working directory. One file per day: traces-YYYYMMDD.jsonl."},
		{"history_days", "integer", d.HistoryDays, "Days of history kept in memory and on disk (1..90). Older files are deleted."},
		{"history_max_requests", "integer", d.HistoryMaxRequests, "Most requests kept in the history (1000..200000). Totals, cost and sessions use all of them; attempt details are kept for the newest ui_max_traces."},
		{"session_gap_minutes", "integer", int(d.SessionGap / time.Minute), "Idle minutes after which a client's next request starts a new session on the Traces page (1..1440)."},
	}
}

// dispatch handles one host RPC. Observation methods only decode and enqueue,
// so they return immediately; all network I/O happens on the background loop.
func dispatch(method string, raw []byte) (any, error) {
	return plug.dispatch(method, raw)
}

func (p *pluginState) dispatch(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req lifecycleRequest
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, fmt.Errorf("decode %s request: %w", method, err)
			}
		}
		cfg, warnings, err := parseConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		p.configure(cfg, warnings)
		return registration(req.SchemaVersion), nil
	case "usage.handle":
		var rec usageRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		trimUsage(&rec)
		p.tracer.addUsage(rec)
		return struct{}{}, nil
	case "request.complete":
		var c requestCompletion
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("decode request completion: %w", err)
		}
		c.TraceID = strings.TrimSpace(c.TraceID)
		p.tracer.addCompletion(c)
		return struct{}{}, nil
	case "management.register":
		return managementRegistrationResponse(), nil
	case "management.handle":
		var req managementRequest
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, fmt.Errorf("decode management request: %w", err)
			}
		}
		return p.handleManagement(req), nil
	case "plugin.quiesce":
		p.quiesce()
		return struct{}{}, nil
	case "plugin.shutdown":
		p.shutdown()
		return struct{}{}, nil
	default:
		return nil, fmt.Errorf("unsupported method: %s", method)
	}
}

func trimUsage(u *usageRecord) {
	u.TraceID = strings.TrimSpace(u.TraceID)
	u.RequestID = strings.TrimSpace(u.RequestID)
	u.Provider = strings.TrimSpace(u.Provider)
}
