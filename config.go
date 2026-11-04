package main

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	minUITraces = 10
	maxUITraces = 5000
	// defaultDataDir is relative to the proxy's working directory, like
	// other store plugins (plugins/<id>).
	defaultDataDir = "plugins/llm-tracing"
)

// config is the validated plugin configuration (plugins.configs.llm-tracing).
type config struct {
	Enabled          bool
	Endpoint         string
	Headers          map[string]string
	ServiceName      string
	SampleRate       float64
	CompletionGrace  time.Duration
	OrphanTimeout    time.Duration
	CaptureErrorBody bool
	MaxPendingTraces int
	MaxQueueSpans    int
	FlushInterval    time.Duration
	MaxBatchSpans    int
	ExportTimeout    time.Duration
	UIEnabled        bool
	UIMaxTraces      int
	// UIShowAccountLabels resolves credential labels (e-mail, label or file
	// name from host.auth.list) for the management-authenticated Traces page.
	UIShowAccountLabels bool
	// ExportAccountLabels adds cpa.account to exported attempt spans.
	ExportAccountLabels bool
	// Pricing and ContextWindows extend or override the built-in tables
	// (pricing.go), keyed by model name.
	Pricing        map[string]modelPrice
	ContextWindows map[string]int64
	// SessionGap is the idle time after which a client's next request starts
	// a new session on the Traces page.
	SessionGap time.Duration
	// CacheWriteTTL selects the cache write price: auto, 5m or 1h.
	CacheWriteTTL string
	// HistoryDays and HistoryMaxRequests bound the request history kept for
	// the page (totals for every request; spans only for the newest
	// UIMaxTraces). SaveHistory writes it to DataDir so it survives
	// restarts and plugin updates.
	HistoryDays        int
	HistoryMaxRequests int
	SaveHistory        bool
	DataDir            string
}

// historyAge is the history window as a duration.
func (c config) historyAge() time.Duration { return time.Duration(c.HistoryDays) * 24 * time.Hour }

// pricer builds the cost and context-window table for this config.
func (c config) pricer() *pricer {
	p := newPricer(c.Pricing, c.ContextWindows)
	if c.CacheWriteTTL != "" {
		p.cacheTTL = c.CacheWriteTTL
	}
	return p
}

// active reports whether the OTLP exporter runs (plugin enabled with an endpoint).
func (c config) active() bool { return c.Enabled && c.Endpoint != "" }

// uiActive reports whether finished traces are kept for the built-in Traces page.
func (c config) uiActive() bool { return c.Enabled && c.UIEnabled }

// recording reports whether the tracer joins events at all: it does whenever
// either the exporter or the Traces page consumes them.
func (c config) recording() bool { return c.active() || c.uiActive() }

func defaultConfig() config {
	return config{
		Enabled:          true,
		Headers:          map[string]string{},
		ServiceName:      "cliproxyapi",
		SampleRate:       1,
		CompletionGrace:  2 * time.Second,
		OrphanTimeout:    10 * time.Minute,
		CaptureErrorBody: true,
		MaxPendingTraces: 10000,
		MaxQueueSpans:    20000,
		FlushInterval:    time.Second,
		MaxBatchSpans:    512,
		ExportTimeout:    10 * time.Second,
		UIEnabled:        true,
		UIMaxTraces:      500,

		UIShowAccountLabels: true,
		ExportAccountLabels: false,
		SessionGap:          30 * time.Minute,
		CacheWriteTTL:       cacheTTLAuto,
		HistoryDays:         7,
		HistoryMaxRequests:  20000,
		SaveHistory:         true,
		DataDir:             defaultDataDir,
	}
}

// parseConfig decodes the YAML block the host passes on plugin.register and
// plugin.reconfigure. Malformed YAML is an error; out-of-range or unusable
// values fall back to defaults or are clamped, and are reported as warnings.
// Warnings never contain header values.
func parseConfig(raw []byte) (config, []string, error) {
	cfg := defaultConfig()
	var warnings []string
	values := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &values); err != nil {
			return cfg, nil, fmt.Errorf("llm-tracing: invalid config yaml: %w", err)
		}
	}
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	if v, ok := values["enabled"]; ok {
		if b, okBool := toBool(v); okBool {
			cfg.Enabled = b
		} else {
			warn("enabled: expected a boolean")
		}
	}
	if v, ok := values["endpoint"]; ok && v != nil {
		endpoint := strings.TrimSpace(fmt.Sprint(v))
		if endpoint != "" {
			if errEndpoint := validateEndpoint(endpoint); errEndpoint != nil {
				warn("endpoint: %v; tracing disabled", errEndpoint)
			} else {
				cfg.Endpoint = endpoint
			}
		}
	}
	if v, ok := values["service_name"]; ok && v != nil {
		if name := strings.TrimSpace(fmt.Sprint(v)); name != "" {
			cfg.ServiceName = name
		}
	}
	if v, ok := values["headers"]; ok && v != nil {
		headers, headerWarnings := parseHeaders(v)
		cfg.Headers = headers
		warnings = append(warnings, headerWarnings...)
	}
	if v, ok := values["sample_rate"]; ok && v != nil {
		if f, okFloat := toFloat(v); okFloat && !math.IsNaN(f) {
			if f < 0 || f > 1 {
				warn("sample_rate: %v outside 0..1; clamped", f)
			}
			cfg.SampleRate = math.Min(1, math.Max(0, f))
		} else {
			warn("sample_rate: expected a number between 0 and 1")
		}
	}
	boolField(values, "capture_error_body", &cfg.CaptureErrorBody, warn)
	boolField(values, "ui_enabled", &cfg.UIEnabled, warn)
	boolField(values, "ui_show_account_labels", &cfg.UIShowAccountLabels, warn)
	boolField(values, "export_account_labels", &cfg.ExportAccountLabels, warn)
	durationField(values, "completion_grace_ms", &cfg.CompletionGrace, 0, 5*time.Minute, warn)
	durationField(values, "orphan_timeout_ms", &cfg.OrphanTimeout, time.Second, 24*time.Hour, warn)
	durationField(values, "flush_interval_ms", &cfg.FlushInterval, 50*time.Millisecond, time.Minute, warn)
	durationField(values, "export_timeout_ms", &cfg.ExportTimeout, 100*time.Millisecond, 2*time.Minute, warn)
	intField(values, "max_pending_traces", &cfg.MaxPendingTraces, 1, 1_000_000, warn)
	intField(values, "max_queue_spans", &cfg.MaxQueueSpans, 1, 1_000_000, warn)
	intField(values, "max_batch_spans", &cfg.MaxBatchSpans, 1, 10_000, warn)
	intField(values, "ui_max_traces", &cfg.UIMaxTraces, minUITraces, maxUITraces, warn)
	intField(values, "history_days", &cfg.HistoryDays, 1, 90, warn)
	intField(values, "history_max_requests", &cfg.HistoryMaxRequests, 1000, 200_000, warn)
	boolField(values, "save_history", &cfg.SaveHistory, warn)
	if v, ok := values["data_dir"]; ok && v != nil {
		if dir := scalarString(v); dir != "" {
			if strings.ContainsRune(dir, 0) {
				warn("data_dir: invalid path; using %s", defaultDataDir)
			} else {
				cfg.DataDir = dir
			}
		}
	}
	if v, ok := values["session_gap_minutes"]; ok && v != nil {
		minutes := 30
		intField(map[string]any{"session_gap_minutes": v}, "session_gap_minutes", &minutes, 1, 24*60, warn)
		cfg.SessionGap = time.Duration(minutes) * time.Minute
	}
	if v, ok := values["cache_write_ttl"]; ok && v != nil {
		switch ttl := strings.ToLower(scalarString(v)); ttl {
		case cacheTTLAuto, cacheTTL5m, cacheTTL1h:
			cfg.CacheWriteTTL = ttl
		default:
			warn("cache_write_ttl: expected auto, 5m or 1h")
		}
	}
	cfg.Pricing = parsePricing(values["pricing"], warn)
	cfg.ContextWindows = parseContextWindows(values["context_windows"], warn)
	return cfg, warnings, nil
}

func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

// parseHeaders accepts either a mapping (`Authorization: Basic ...`) or a list
// whose items are "Key: Value" strings or {name|key, value} mappings.
func parseHeaders(v any) (map[string]string, []string) {
	out := map[string]string{}
	var warnings []string
	add := func(name, value string) {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !validHeaderName(name) {
			warnings = append(warnings, fmt.Sprintf("headers: invalid header name %q ignored", name))
			return
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			warnings = append(warnings, fmt.Sprintf("headers: value of %q contains control characters; ignored", name))
			return
		}
		out[name] = value
	}
	switch typed := v.(type) {
	case map[string]any:
		for name, value := range typed {
			add(name, scalarString(value))
		}
	case []any:
		for _, item := range typed {
			switch entry := item.(type) {
			case string:
				name, value, ok := strings.Cut(entry, ":")
				if !ok {
					warnings = append(warnings, "headers: list entry without ':' ignored")
					continue
				}
				add(name, value)
			case map[string]any:
				name := scalarString(entry["name"])
				if name == "" {
					name = scalarString(entry["key"])
				}
				add(name, scalarString(entry["value"]))
			default:
				warnings = append(warnings, "headers: unsupported list entry ignored")
			}
		}
	case string:
		if strings.TrimSpace(typed) != "" {
			warnings = append(warnings, "headers: expected a mapping or a list; ignored")
		}
	default:
		warnings = append(warnings, "headers: expected a mapping or a list; ignored")
	}
	return out, warnings
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", r) {
			return false
		}
	}
	return true
}

// sortedHeaderNames returns header names in a stable order.
func sortedHeaderNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func scalarString(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func boolField(values map[string]any, key string, dst *bool, warn func(string, ...any)) {
	v, ok := values[key]
	if !ok || v == nil {
		return
	}
	if b, okBool := toBool(v); okBool {
		*dst = b
	} else {
		warn("%s: expected a boolean", key)
	}
}

func durationField(values map[string]any, key string, dst *time.Duration, minimum, maximum time.Duration, warn func(string, ...any)) {
	v, ok := values[key]
	if !ok || v == nil {
		return
	}
	f, okFloat := toFloat(v)
	if !okFloat || math.IsNaN(f) {
		warn("%s: expected a number of milliseconds", key)
		return
	}
	d := time.Duration(f * float64(time.Millisecond))
	if d < minimum || d > maximum {
		warn("%s: %v outside %d..%d; clamped", key, f, minimum.Milliseconds(), maximum.Milliseconds())
		d = min(max(d, minimum), maximum)
	}
	*dst = d
}

func intField(values map[string]any, key string, dst *int, minimum, maximum int, warn func(string, ...any)) {
	v, ok := values[key]
	if !ok || v == nil {
		return
	}
	f, okFloat := toFloat(v)
	if !okFloat || math.IsNaN(f) {
		warn("%s: expected an integer", key)
		return
	}
	n := int(math.Min(math.Max(f, math.MinInt32), math.MaxInt32))
	if n < minimum || n > maximum {
		warn("%s: %d outside %d..%d; clamped", key, n, minimum, maximum)
		n = min(max(n, minimum), maximum)
	}
	*dst = n
}

// toFloat accepts YAML numbers and numeric strings (management clients may
// write either).
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func toBool(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(b))
		return parsed, err == nil
	default:
		return false, false
	}
}

const maxConfigModels = 500

// parsePricing reads `pricing: {<model>: {input, output, cache_read,
// cache_write}}` in USD per million tokens. cache_read defaults to 10% and
// cache_write to 125% of input. Invalid entries are skipped with a warning.
func parsePricing(v any, warn func(string, ...any)) map[string]modelPrice {
	out := map[string]modelPrice{}
	if v == nil {
		return out
	}
	entries, ok := v.(map[string]any)
	if !ok {
		warn("pricing: expected a mapping of model name to prices")
		return out
	}
	for name, raw := range entries {
		if len(out) >= maxConfigModels {
			warn("pricing: more than %d models; the rest are ignored", maxConfigModels)
			break
		}
		fields, okMap := raw.(map[string]any)
		if !okMap || strings.TrimSpace(name) == "" {
			warn("pricing: entry %q must be a mapping with input and output", name)
			continue
		}
		rate := func(key string) (float64, bool, bool) {
			fv, present := fields[key]
			if !present || fv == nil {
				return 0, false, true
			}
			f, okNum := toFloat(fv)
			if !okNum || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1e6 {
				return 0, true, false
			}
			return f, true, true
		}
		var price modelPrice
		valid := true
		var present bool
		var okRate bool
		if price.Input, present, okRate = rate("input"); !present || !okRate {
			valid = false
		}
		if price.Output, present, okRate = rate("output"); !present || !okRate {
			valid = false
		}
		if price.CacheRead, present, okRate = rate("cache_read"); !okRate {
			valid = false
		} else if !present {
			price.CacheRead = price.Input * 0.1
		}
		if price.CacheWrite, present, okRate = rate("cache_write"); !okRate {
			valid = false
		} else if !present {
			price.CacheWrite = price.Input * 1.25
		}
		if !valid {
			warn("pricing: entry %q needs non-negative numbers for input and output (cache_read, cache_write optional)", name)
			continue
		}
		out[name] = price
	}
	return out
}

// parseContextWindows reads `context_windows: {<model>: <tokens>}`.
func parseContextWindows(v any, warn func(string, ...any)) map[string]int64 {
	out := map[string]int64{}
	if v == nil {
		return out
	}
	entries, ok := v.(map[string]any)
	if !ok {
		warn("context_windows: expected a mapping of model name to tokens")
		return out
	}
	for name, raw := range entries {
		if len(out) >= maxConfigModels {
			warn("context_windows: more than %d models; the rest are ignored", maxConfigModels)
			break
		}
		f, okNum := toFloat(raw)
		if !okNum || math.IsNaN(f) || f < 1000 || f > 100_000_000 || strings.TrimSpace(name) == "" {
			warn("context_windows: entry %q must be a token count between 1000 and 100000000", name)
			continue
		}
		out[name] = int64(f)
	}
	return out
}
