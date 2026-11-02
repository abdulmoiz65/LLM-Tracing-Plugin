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
}

// active reports whether the plugin should record and export anything.
func (c config) active() bool { return c.Enabled && c.Endpoint != "" }

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
	if v, ok := values["capture_error_body"]; ok && v != nil {
		if b, okBool := toBool(v); okBool {
			cfg.CaptureErrorBody = b
		} else {
			warn("capture_error_body: expected a boolean")
		}
	}
	durationField(values, "completion_grace_ms", &cfg.CompletionGrace, 0, 5*time.Minute, warn)
	durationField(values, "orphan_timeout_ms", &cfg.OrphanTimeout, time.Second, 24*time.Hour, warn)
	durationField(values, "flush_interval_ms", &cfg.FlushInterval, 50*time.Millisecond, time.Minute, warn)
	durationField(values, "export_timeout_ms", &cfg.ExportTimeout, 100*time.Millisecond, 2*time.Minute, warn)
	intField(values, "max_pending_traces", &cfg.MaxPendingTraces, 1, 1_000_000, warn)
	intField(values, "max_queue_spans", &cfg.MaxQueueSpans, 1, 1_000_000, warn)
	intField(values, "max_batch_spans", &cfg.MaxBatchSpans, 1, 10_000, warn)
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
