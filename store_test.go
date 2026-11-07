package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// uiConfig returns a config with the Traces page on and no exporter.
func uiConfig() config {
	cfg := defaultConfig()
	cfg.UIMaxTraces = 500
	return cfg
}

func finalize(tr *tracer, clock *fakeClock) {
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
}

func TestTraceStoreRingBoundsAndOrder(t *testing.T) {
	s := newTraceStore()
	s.configure(true, 10, 10, 0)
	var ids []traceID
	for i := 0; i < 15; i++ {
		tid := newTraceID()
		ids = append(ids, tid)
		s.add(&uiTrace{tid: tid, outcome: outcomeSucceeded, start: baseTime.Add(time.Duration(i) * time.Second).UnixNano()})
	}
	if s.len() != 10 || s.capacity() != 10 {
		t.Fatalf("len=%d cap=%d", s.len(), s.capacity())
	}
	list := s.list(traceFilter{limit: 500, status: "all"}, nil)
	if len(list) != 10 || list[0].TraceID != ids[14].String() || list[9].TraceID != ids[5].String() {
		t.Fatalf("want newest first, got %d items starting %s", len(list), list[0].TraceID)
	}
	if _, ok := s.get(ids[4], nil); ok {
		t.Fatal("evicted trace still reachable")
	}
	if _, ok := s.get(ids[5], nil); !ok {
		t.Fatal("retained trace not reachable")
	}
	s.configure(true, 3, 3, 0) // shrink keeps the newest
	list = s.list(traceFilter{limit: 500, status: "all"}, nil)
	if len(list) != 3 || list[0].TraceID != ids[14].String() || list[2].TraceID != ids[12].String() {
		t.Fatalf("shrink kept wrong traces: %+v", list)
	}
	s.add(&uiTrace{tid: newTraceID(), outcome: outcomeSucceeded, start: baseTime.Add(time.Hour).UnixNano()})
	if s.len() != 3 {
		t.Fatalf("bound not enforced after shrink: %d", s.len())
	}
	s.configure(false, 3, 3, 0)
	if s.len() != 0 || s.capacity() != 0 || len(s.byID) != 0 {
		t.Fatal("disabled store must release everything")
	}
	s.add(&uiTrace{tid: newTraceID()})
	if s.len() != 0 {
		t.Fatal("disabled store must ignore inserts")
	}
}

func TestStoreKeepsAttemptsBounded(t *testing.T) {
	clock := newFakeClock()
	tr := newTestTracer(t, uiConfig(), clock)
	tr.addCompletion(completionAt("many", baseTime, baseTime.Add(10*time.Second), outcomeSucceeded, 200))
	for i := 0; i < maxStoredAttempts+5; i++ {
		tr.addUsage(usageAt("many", fmt.Sprintf("u%02d", i), baseTime.Add(time.Duration(i)*time.Millisecond), time.Millisecond))
	}
	finalize(tr, clock)
	list := tr.store.list(traceFilter{limit: 1, status: "all"}, nil)
	detail, _ := tr.store.get(mustTraceID(t, list[0].TraceID), nil)
	if detail.AttemptCount != maxStoredAttempts+5 || len(detail.Spans) != maxStoredAttempts+1 || detail.InputTokens != int64(10*(maxStoredAttempts+5)) {
		t.Fatalf("attempts=%d spans=%d input=%d", detail.AttemptCount, len(detail.Spans), detail.InputTokens)
	}
}

func mustTraceID(t *testing.T, s string) traceID {
	t.Helper()
	id, ok := parseTraceIDParam(s)
	if !ok {
		t.Fatalf("bad trace id %q", s)
	}
	return id
}

func TestLateUsageUpdatesStoredTrace(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	tr.addCompletion(completionAt("late", baseTime, baseTime.Add(3*time.Second), "failed", 502))
	tr.addUsage(usageAt("late", "u1", baseTime.Add(100*time.Millisecond), time.Second))
	finalize(tr, clock)
	exportNow(t, tr, c)
	list := tr.store.list(traceFilter{limit: 10, status: "all"}, nil)
	if len(list) != 1 || list[0].AttemptCount != 1 {
		t.Fatalf("stored before late usage: %+v", list)
	}

	late := usageAt("late", "u2", baseTime.Add(2*time.Second), 500*time.Millisecond)
	late.Failed = true
	late.Failure = usageFailure{StatusCode: 502, Body: "upstream exploded, key sk-abcdefghijklmnop"}
	late.Detail = usageDetail{InputTokens: 7, OutputTokens: 0, CachedTokens: 3, CacheReadTokens: 3}
	tr.addUsage(late)

	detail, ok := tr.store.get(mustTraceID(t, list[0].TraceID), nil)
	if !ok {
		t.Fatal("stored trace vanished")
	}
	if detail.AttemptCount != 2 || detail.FailedAttempts != 1 || detail.InputTokens != 17 || detail.CachedTokens != 3 || len(detail.Spans) != 3 {
		t.Fatalf("late usage not folded in: %+v", detail.traceItemJSON)
	}
	if !strings.Contains(detail.Error, "sk-[REDACTED]") || strings.Contains(detail.Error, "abcdefghijklmnop") {
		t.Fatalf("error not taken from sanitized late attempt: %q", detail.Error)
	}
	lateSpan := detail.Spans[2]
	if lateSpan.ParentSpanID != detail.Spans[0].SpanID || lateSpan.Status != "error" || lateSpan.Attributes["cpa.attempt"] != int64(2) {
		t.Fatalf("late span wrong: %+v", lateSpan)
	}
	// The late span is also exported, under the same OTel trace.
	spans := exportNow(t, tr, c)
	if len(spans) != 3 || spans[2].TraceID != list[0].TraceID {
		t.Fatalf("late span not exported to the same trace")
	}
}

func TestUIWorksWithoutEndpoint(t *testing.T) {
	clock := newFakeClock()
	p, logs := newTestPlugin(t, clock)
	p.autoLoop = true
	if _, err := p.dispatch("plugin.register", registerFixture("enabled: true\npriority: 0\n"+ownDataDir(t), 10)); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	if !running {
		t.Fatal("background loop must run for the Traces page even without an endpoint")
	}
	t.Cleanup(p.shutdown)
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "Traces page") {
		t.Fatalf("mode not logged: %v", *logs)
	}
	for _, body := range []string{failedUsageFixture, usageFixture} {
		if _, err := p.dispatch("usage.handle", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.dispatch("request.complete", []byte(completionFixture)); err != nil {
		t.Fatal(err)
	}
	p.tracer.flushPending()
	if p.tracer.queueLen() != 0 {
		t.Fatal("no exporter: nothing may be queued for export")
	}
	if n, err := p.tracer.flushOnce(t.Context()); n != 0 || err != nil {
		t.Fatalf("exporter must stay off: n=%d err=%v", n, err)
	}
	s := p.tracer.summary()
	if s.StoredTraces != 1 || s.Requests != 1 || s.RetriedRequests != 1 || s.Exporter.Enabled || s.Exporter.EndpointHost != "" || s.Version != "0.3.1" {
		t.Fatalf("summary wrong: %+v", s)
	}
}

func TestStoredTraceIsSanitized(t *testing.T) {
	clock := newFakeClock()
	p, _ := newTestPlugin(t, clock)
	if _, err := p.dispatch("plugin.register", registerFixture("enabled: true\n", 10)); err != nil {
		t.Fatal(err)
	}
	_, _ = p.dispatch("usage.handle", []byte(failedUsageFixture))
	_, _ = p.dispatch("usage.handle", []byte(usageFixture))
	_, _ = p.dispatch("request.complete", []byte(completionFixture))
	finalize(p.tracer, clock)
	_, _, list := mgmtGet(t, p, "/traces", nil)
	id := between(string(list), `"trace_id":"`, `"`)
	_, _, body := mgmtGet(t, p, "/trace", map[string]string{"id": id})
	all := string(list) + string(body)
	for _, leaked := range []string{"e2e-client-key-0001", "sk-should-be-redacted-123456", "AIzaQUERYSECRET", "user:pw", "session=secret", "someone@example.com", "AuthID", "langfuse."} {
		if strings.Contains(all, leaked) {
			t.Fatalf("stored trace leaked %q", leaked)
		}
	}
	if !strings.Contains(all, maskClientKey("e2e-client-key-0001")) || !strings.Contains(all, "https://upstream.example.com/v1") {
		t.Fatal("expected sanitized fields missing")
	}
}

func between(s, start, end string) string {
	_, rest, ok := strings.Cut(s, start)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, end)
	return v
}

func TestSampledOutTracesStillStored(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	cfg := testConfig(c.endpoint())
	cfg.SampleRate = 0
	tr := newTestTracer(t, cfg, clock)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("s%d", i)
		tr.addCompletion(completionAt(id, baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200))
		tr.addUsage(usageAt(id, id+"-u", baseTime.Add(10*time.Millisecond), 100*time.Millisecond))
	}
	tr.addUsage(usageAt("", "loose", baseTime, time.Second))
	finalize(tr, clock)
	tr.addUsage(usageAt("s0", "s0-late", baseTime.Add(500*time.Millisecond), 100*time.Millisecond))
	if spans := exportNow(t, tr, c); len(spans) != 0 {
		t.Fatalf("sample_rate 0 exported %d spans", len(spans))
	}
	if got := tr.store.len(); got != 6 {
		t.Fatalf("Traces page must show sampled-out requests: stored %d", got)
	}
	var late int
	for _, item := range tr.store.list(traceFilter{limit: 10, status: "all"}, nil) {
		if item.CPATraceID == "s0" {
			late = item.AttemptCount
		}
	}
	if late != 2 {
		t.Fatalf("late usage of sampled-out trace not stored: attempts=%d", late)
	}

	// With the page off, sampling drops data before it is buffered.
	cfg.UIEnabled = false
	tr2 := newTestTracer(t, cfg, clock)
	tr2.addCompletion(completionAt("x", baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200))
	tr2.addUsage(usageAt("x", "x-u", baseTime, time.Second))
	if tr2.pendingLen() != 0 || tr2.store.capacity() != 0 || tr2.snapshot().sampledOut != 2 {
		t.Fatalf("page off: pending=%d cap=%d sampledOut=%d", tr2.pendingLen(), tr2.store.capacity(), tr2.snapshot().sampledOut)
	}
}

func TestSummaryMath(t *testing.T) {
	clock := newFakeClock()
	clock.Advance(2 * time.Hour) // now = baseTime + 2h01m
	tr := newTestTracer(t, uiConfig(), clock)
	windowStart := clock.Now().Add(-time.Hour)

	// Outside the window: started 61 minutes ago.
	old := completionAt("old", windowStart.Add(-time.Minute), windowStart.Add(-time.Minute+time.Second), "failed", 500)
	tr.addCompletion(old)
	for i := 1; i <= 20; i++ {
		id := fmt.Sprintf("w%02d", i)
		start := windowStart.Add(time.Duration(i) * time.Minute)
		outcome, status := outcomeSucceeded, 200
		if i%5 == 0 {
			outcome, status = "failed", 502
		}
		if i == 3 {
			status = 429 // a succeeded outcome with an error status still counts as an error
		}
		tr.addCompletion(completionAt(id, start, start.Add(time.Duration(i)*100*time.Millisecond), outcome, status))
		tr.addUsage(usageAt(id, id+"-a", start.Add(time.Millisecond), time.Millisecond))
		if i%4 == 0 {
			tr.addUsage(usageAt(id, id+"-b", start.Add(2*time.Millisecond), time.Millisecond))
		}
	}
	finalize(tr, clock)
	s := tr.summary()
	if s.StoredTraces != 21 || s.Capacity != 20000 || s.DetailTraces != 500 || s.WindowSeconds != 3600 {
		t.Fatalf("store stats: %+v", s)
	}
	if s.Requests != 20 || s.Errors != 5 || s.ErrorRate != 0.25 || s.RetriedRequests != 5 {
		t.Fatalf("counts: requests=%d errors=%d rate=%v retried=%d", s.Requests, s.Errors, s.ErrorRate, s.RetriedRequests)
	}
	// Durations 100..2000 ms: mean 1050; nearest-rank p95 = 19th of 20 = 1900.
	if s.AvgLatencyMS != 1050 || s.P95LatencyMS != 1900 {
		t.Fatalf("latency avg=%v p95=%v", s.AvgLatencyMS, s.P95LatencyMS)
	}
	if s.InputTokens != 250 || s.OutputTokens != 125 {
		t.Fatalf("tokens in=%d out=%d", s.InputTokens, s.OutputTokens)
	}

	empty := newTestTracer(t, uiConfig(), clock).summary()
	if empty.Requests != 0 || empty.ErrorRate != 0 || empty.P95LatencyMS != 0 {
		t.Fatalf("empty summary: %+v", empty)
	}
	if percentile([]float64{5}, 0.95) != 5 || percentile([]float64{4, 1, 3, 2}, 0.5) != 2 {
		t.Fatal("percentile nearest-rank wrong")
	}
}

func TestSummaryExporterStatus(t *testing.T) {
	c := newCollector(t, 401)
	clock := newFakeClock()
	cfg := testConfig(c.endpoint() + "?token=secret-query")
	cfg.Headers = map[string]string{"Authorization": "Basic c2VjcmV0LWhlYWRlcg=="}
	tr := newTestTracer(t, cfg, clock)
	tr.addUsage(usageAt("", "one", baseTime, time.Second))
	exportNow(t, tr, c)
	s := tr.summary()
	host := strings.TrimPrefix(c.server.URL, "http://")
	if !s.Exporter.Enabled || s.Exporter.EndpointHost != host || s.Exporter.DroppedSpans != 1 || s.Exporter.ExportedSpans != 0 {
		t.Fatalf("exporter status: %+v", s.Exporter)
	}
	if !strings.Contains(s.Exporter.LastError, "401") || strings.Contains(s.Exporter.LastError, "secret-query") || strings.Contains(s.Exporter.LastError, "c2VjcmV0") || s.Exporter.LastExportAt != "" {
		t.Fatalf("last error wrong: %+v", s.Exporter)
	}
	tr.addUsage(usageAt("", "two", baseTime, time.Second))
	exportNow(t, tr, c)
	s = tr.summary()
	if s.Exporter.LastError != "" || s.Exporter.LastExportAt != clock.Now().UTC().Format(time.RFC3339) || s.Exporter.ExportedSpans != 1 {
		t.Fatalf("success not reflected: %+v", s.Exporter)
	}
	if endpointHost("https://cloud.langfuse.com/api/public/otel/v1/traces") != "cloud.langfuse.com:443" || endpointHost("http://u:p@[::1]/x") != "[::1]:80" {
		t.Fatal("endpointHost wrong")
	}
}

func TestParseUIConfig(t *testing.T) {
	cfg, warnings, err := parseConfig([]byte("enabled: true\n"))
	if err != nil || len(warnings) != 0 || !cfg.UIEnabled || cfg.UIMaxTraces != 500 || !cfg.uiActive() || !cfg.recording() || cfg.active() {
		t.Fatalf("defaults: %+v %v", cfg, warnings)
	}
	cfg, warnings, _ = parseConfig([]byte("ui_enabled: \"false\"\nui_max_traces: 3\n"))
	if cfg.UIEnabled || cfg.UIMaxTraces != 10 || cfg.recording() || len(warnings) != 1 {
		t.Fatalf("clamp low / disable: %+v %v", cfg, warnings)
	}
	cfg, warnings, _ = parseConfig([]byte("ui_max_traces: \"99999\"\nui_enabled: maybe\n"))
	if cfg.UIMaxTraces != 5000 || !cfg.UIEnabled || len(warnings) != 2 {
		t.Fatalf("clamp high / bad bool: %+v %v", cfg, warnings)
	}
	names := map[string]bool{}
	for _, f := range configFields() {
		names[f.Name] = true
	}
	if !names["ui_enabled"] || !names["ui_max_traces"] {
		t.Fatal("ui config fields missing from registration")
	}
}

func TestCanceledIsNotAnError(t *testing.T) {
	clock := newFakeClock()
	tr := newTestTracer(t, uiConfig(), clock)
	start := clock.Now()
	tr.addCompletion(completionAt("ok", start, start.Add(time.Second), outcomeSucceeded, 200))
	tr.addCompletion(completionAt("esc", start.Add(time.Second), start.Add(2*time.Second), outcomeCanceled, 499))
	tr.addCompletion(completionAt("bad", start.Add(2*time.Second), start.Add(3*time.Second), "failed", 502))
	finalize(tr, clock)
	s := tr.summaryFor(0, "all")
	if s.Requests != 3 || s.Errors != 1 || s.Canceled != 1 || s.ErrorRate != 0.3333 {
		t.Fatalf("errors=%d canceled=%d rate=%v", s.Errors, s.Canceled, s.ErrorRate)
	}
	if got := tr.store.list(traceFilter{limit: 10, status: "canceled"}, nil); len(got) != 1 || got[0].Outcome != outcomeCanceled {
		t.Fatalf("canceled filter: %+v", got)
	}
	if got := tr.store.list(traceFilter{limit: 10, status: "error"}, nil); len(got) != 1 || got[0].StatusCode != 502 {
		t.Fatalf("error filter: %+v", got)
	}
}
