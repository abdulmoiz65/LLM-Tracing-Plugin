package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, warnings, err := parseConfig([]byte("enabled: true\npriority: 0\n"))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	want := defaultConfig()
	if cfg.Endpoint != "" || cfg.active() || cfg.ServiceName != want.ServiceName || cfg.SampleRate != 1 ||
		cfg.CompletionGrace != 2*time.Second || cfg.OrphanTimeout != 10*time.Minute || !cfg.CaptureErrorBody ||
		cfg.MaxPendingTraces != 10000 || cfg.MaxQueueSpans != 20000 || cfg.FlushInterval != time.Second ||
		cfg.MaxBatchSpans != 512 || cfg.ExportTimeout != 10*time.Second {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

// The host re-marshals the plugins.configs.llm-tracing subtree with yaml.v3
// (4-space indent) and injects enabled/priority.
func TestParseConfigFull(t *testing.T) {
	raw := `enabled: true
priority: 10
endpoint: " https://cloud.langfuse.com/api/public/otel/v1/traces "
headers:
    Authorization: Basic cGs6c2s=
    x-honeycomb-team: hc
service_name: cpa-prod
sample_rate: 0.25
completion_grace_ms: 500
orphan_timeout_ms: "60000"
capture_error_body: false
max_pending_traces: 50
max_queue_spans: 100
flush_interval_ms: 200
max_batch_spans: 10
export_timeout_ms: 3000
store:
    version: 0.1.0
`
	cfg, warnings, err := parseConfig([]byte(raw))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if cfg.Endpoint != "https://cloud.langfuse.com/api/public/otel/v1/traces" || !cfg.active() {
		t.Fatalf("endpoint %q", cfg.Endpoint)
	}
	if cfg.Headers["Authorization"] != "Basic cGs6c2s=" || cfg.Headers["x-honeycomb-team"] != "hc" || len(cfg.Headers) != 2 {
		t.Fatalf("headers %v", cfg.Headers)
	}
	if cfg.ServiceName != "cpa-prod" || cfg.SampleRate != 0.25 || cfg.CompletionGrace != 500*time.Millisecond ||
		cfg.OrphanTimeout != time.Minute || cfg.CaptureErrorBody || cfg.MaxPendingTraces != 50 || cfg.MaxQueueSpans != 100 ||
		cfg.FlushInterval != 200*time.Millisecond || cfg.MaxBatchSpans != 10 || cfg.ExportTimeout != 3*time.Second {
		t.Fatalf("parsed wrong: %+v", cfg)
	}
}

func TestParseConfigHeaderList(t *testing.T) {
	raw := `endpoint: http://127.0.0.1:4318/v1/traces
headers:
    - "DD-API-KEY: dd-secret"
    - "Authorization: Basic a:b:c"
    - name: X-Scope-OrgID
      value: tenant-1
    - "bad header without colon"
    - "Bad Name: x"
`
	cfg, warnings, err := parseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Headers["DD-API-KEY"] != "dd-secret" || cfg.Headers["Authorization"] != "Basic a:b:c" || cfg.Headers["X-Scope-OrgID"] != "tenant-1" || len(cfg.Headers) != 3 {
		t.Fatalf("headers %v", cfg.Headers)
	}
	if len(warnings) != 2 {
		t.Fatalf("want 2 warnings, got %v", warnings)
	}
	for _, w := range warnings {
		if strings.Contains(w, "dd-secret") || strings.Contains(w, "tenant-1") {
			t.Fatalf("warning leaks a header value: %s", w)
		}
	}
}

func TestParseConfigClampsAndRejects(t *testing.T) {
	cfg, warnings, err := parseConfig([]byte("endpoint: ftp://x/y\nsample_rate: 7\nmax_batch_spans: 0\nflush_interval_ms: abc\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "" || cfg.active() {
		t.Fatal("invalid endpoint must disable tracing")
	}
	if cfg.SampleRate != 1 || cfg.MaxBatchSpans != 1 || cfg.FlushInterval != time.Second {
		t.Fatalf("clamping wrong: %+v", cfg)
	}
	if len(warnings) != 4 {
		t.Fatalf("want 4 warnings, got %v", warnings)
	}
	if _, _, err := parseConfig([]byte("endpoint: [unclosed")); err == nil {
		t.Fatal("malformed yaml must fail")
	}
	cfg, _, _ = parseConfig([]byte("enabled: false\nendpoint: http://127.0.0.1:4318/v1/traces\n"))
	if cfg.active() {
		t.Fatal("enabled: false must disable tracing")
	}
}
