package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCompletionThenUsage(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)

	tr.addCompletion(completionAt("trace-a", baseTime, baseTime.Add(3*time.Second), outcomeSucceeded, 200))
	clock.Advance(500 * time.Millisecond)
	tr.addUsage(usageAt("trace-a", "u1", baseTime.Add(100*time.Millisecond), 2*time.Second))

	tr.sweep(clock.Now())
	if got := tr.queueLen(); got != 0 {
		t.Fatalf("exported before grace elapsed: %d spans queued", got)
	}
	clock.Advance(1500 * time.Millisecond) // grace (2s) measured from completion arrival
	tr.sweep(clock.Now())
	spans := exportNow(t, tr, c)
	assertSingleRequestTrace(t, spans)
}

func TestUsageThenCompletion(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)

	tr.addUsage(usageAt("trace-a", "u1", baseTime.Add(100*time.Millisecond), 2*time.Second))
	clock.Advance(time.Second)
	tr.addCompletion(completionAt("trace-a", baseTime, baseTime.Add(3*time.Second), outcomeSucceeded, 200))
	clock.Advance(2 * time.Second)
	tr.sweep(clock.Now())
	spans := exportNow(t, tr, c)
	assertSingleRequestTrace(t, spans)
	if tr.pendingLen() != 0 {
		t.Fatalf("pending buckets left: %d", tr.pendingLen())
	}
}

func assertSingleRequestTrace(t *testing.T, spans []otlpSpan) {
	t.Helper()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	roots, children := rootsAndChildren(spans)
	if len(roots) != 1 || len(children) != 1 {
		t.Fatalf("want 1 root + 1 child, got %d + %d", len(roots), len(children))
	}
	root, child := roots[0], children[0]
	if child.ParentSpanID != root.SpanID || child.TraceID != root.TraceID {
		t.Fatalf("child not parented to root: %+v / %+v", child, root)
	}
	if root.Kind != spanKindServer || child.Kind != spanKindClient {
		t.Fatalf("kinds: root=%d child=%d", root.Kind, child.Kind)
	}
	if root.Name != "cpa.request sonnet" || child.Name != "cpa.attempt claude" {
		t.Fatalf("names: %q %q", root.Name, child.Name)
	}
	if strAttr(t, root, "cpa.trace_id") != "trace-a" || intAttr(t, root, "cpa.attempt_count") != "1" {
		t.Fatalf("root attributes wrong")
	}
	if intAttr(t, root, "cpa.usage.input_tokens") != "10" || intAttr(t, child, "gen_ai.usage.output_tokens") != "5" {
		t.Fatalf("token attributes wrong")
	}
	if root.StartTimeUnixNano != unixNano(baseTime) || root.EndTimeUnixNano != unixNano(baseTime.Add(3*time.Second)) {
		t.Fatalf("root times wrong")
	}
	if child.StartTimeUnixNano != unixNano(baseTime.Add(100*time.Millisecond)) || child.EndTimeUnixNano != unixNano(baseTime.Add(2100*time.Millisecond)) {
		t.Fatalf("child times wrong")
	}
	if root.Status.Code != statusUnset || child.Status.Code != statusUnset {
		t.Fatalf("success spans must have unset status")
	}
}

func TestRetryTrace(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)

	first := usageAt("trace-r", "u1", baseTime.Add(10*time.Millisecond), 200*time.Millisecond)
	first.Failed, first.Failure = true, usageFailure{StatusCode: 429, Body: `{"error":"rate limited"}`}
	second := usageAt("trace-r", "u2", baseTime.Add(300*time.Millisecond), 200*time.Millisecond)
	second.Failed, second.Failure = true, usageFailure{StatusCode: 503, Body: "overloaded"}
	second.Provider = "codex"
	third := usageAt("trace-r", "u3", baseTime.Add(600*time.Millisecond), time.Second)
	third.TTFT = 250 * time.Millisecond

	// Deliver out of order to prove ordinals follow start time.
	tr.addUsage(third)
	tr.addUsage(first)
	tr.addCompletion(completionAt("trace-r", baseTime, baseTime.Add(2*time.Second), outcomeSucceeded, 200))
	tr.addUsage(second)
	clock.Advance(2 * time.Second)
	tr.sweep(clock.Now())
	spans := exportNow(t, tr, c)

	roots, children := rootsAndChildren(spans)
	if len(roots) != 1 || len(children) != 3 {
		t.Fatalf("want 1 root + 3 children, got %d + %d", len(roots), len(children))
	}
	root := roots[0]
	if intAttr(t, root, "cpa.attempt_count") != "3" || intAttr(t, root, "cpa.usage.total_tokens") != "45" {
		t.Fatalf("root rollup wrong")
	}
	byOrdinal := map[string]otlpSpan{}
	for _, ch := range children {
		if ch.ParentSpanID != root.SpanID || ch.TraceID != root.TraceID {
			t.Fatalf("child %s not parented to root", ch.Name)
		}
		byOrdinal[intAttr(t, ch, "cpa.attempt")] = ch
	}
	if strAttr(t, byOrdinal["1"], "cpa.request_id") != "u1" || strAttr(t, byOrdinal["2"], "cpa.request_id") != "u2" || strAttr(t, byOrdinal["3"], "cpa.request_id") != "u3" {
		t.Fatalf("ordinals not by start time")
	}
	for _, ord := range []string{"1", "2"} {
		s := byOrdinal[ord]
		if s.Status.Code != statusError || !hasAttr(s, "http.response.status_code") || !hasAttr(s, "error.type") {
			t.Fatalf("attempt %s should be an error span: %+v", ord, s.Status)
		}
	}
	if intAttr(t, byOrdinal["1"], "http.response.status_code") != "429" || strAttr(t, byOrdinal["2"], "error.type") != "503" {
		t.Fatalf("failure status codes wrong")
	}
	if byOrdinal["2"].Name != "cpa.attempt codex" {
		t.Fatalf("provider in name: %q", byOrdinal["2"].Name)
	}
	ok := byOrdinal["3"]
	if ok.Status.Code != statusUnset || len(ok.Events) != 1 || ok.Events[0].Name != "first_token" ||
		ok.Events[0].TimeUnixNano != unixNano(baseTime.Add(850*time.Millisecond)) || intAttr(t, ok, "cpa.ttft_ms") != "250" {
		t.Fatalf("successful attempt wrong: %+v", ok)
	}
}

func TestFailedRequestRootIsError(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	comp := completionAt("trace-f", baseTime, baseTime.Add(time.Second), "failed", 502)
	comp.Error = "upstream said: invalid api_key=abc123secret"
	tr.addCompletion(comp)
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
	spans := exportNow(t, tr, c)
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	root := spans[0]
	if root.Status.Code != statusError || strAttr(t, root, "error.type") != "502" || strAttr(t, root, "cpa.outcome") != "failed" {
		t.Fatalf("root error status wrong: %+v", root.Status)
	}
	if e := strAttr(t, root, "cpa.error"); strings.Contains(e, "abc123secret") || !strings.Contains(e, "[REDACTED]") {
		t.Fatalf("error not redacted: %q", e)
	}
}

func TestOrphanUsageFlush(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	cfg := testConfig(c.endpoint())
	cfg.OrphanTimeout = time.Minute
	tr := newTestTracer(t, cfg, clock)

	tr.addUsage(usageAt("trace-o", "u1", baseTime, time.Second))
	clock.Advance(time.Minute - time.Millisecond)
	tr.sweep(clock.Now())
	if tr.queueLen() != 0 {
		t.Fatalf("orphan exported too early")
	}
	clock.Advance(time.Millisecond)
	tr.sweep(clock.Now())
	spans := exportNow(t, tr, c)
	roots, children := rootsAndChildren(spans)
	if len(roots) != 1 || len(children) != 1 || children[0].ParentSpanID != roots[0].SpanID {
		t.Fatalf("want synthesized root + child, got %d + %d", len(roots), len(children))
	}
	if v, _ := attr(roots[0], "cpa.root_synthesized"); v.BoolValue == nil || !*v.BoolValue {
		t.Fatalf("root not marked synthesized")
	}
	if strAttr(t, roots[0], "cpa.outcome") != "unknown" {
		t.Fatalf("synthesized outcome wrong")
	}
}

func TestEmptyTraceIDUsageIsOwnTrace(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	tr.addUsage(usageAt("", "u1", baseTime, time.Second))
	tr.addUsage(usageAt("", "u2", baseTime, time.Second))
	spans := exportNow(t, tr, c) // no sweep needed: exported immediately
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	if spans[0].ParentSpanID != "" || spans[1].ParentSpanID != "" || spans[0].TraceID == spans[1].TraceID {
		t.Fatalf("empty-trace usage must be independent root spans")
	}
	if tr.pendingLen() != 0 {
		t.Fatalf("empty-trace usage must not be buffered")
	}
}

func TestLateUsageAttachesToExportedTrace(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	tr.addCompletion(completionAt("trace-l", baseTime, baseTime.Add(3*time.Second), outcomeSucceeded, 200))
	tr.addUsage(usageAt("trace-l", "u1", baseTime.Add(100*time.Millisecond), time.Second))
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
	exportNow(t, tr, c)

	tr.addUsage(usageAt("trace-l", "u2", baseTime.Add(2*time.Second), 500*time.Millisecond))
	spans := exportNow(t, tr, c)
	if len(spans) != 3 {
		t.Fatalf("want 3 spans, got %d", len(spans))
	}
	roots, children := rootsAndChildren(spans)
	if len(roots) != 1 || len(children) != 2 {
		t.Fatalf("want 1 root + 2 children")
	}
	late := children[1]
	if late.TraceID != roots[0].TraceID || late.ParentSpanID != roots[0].SpanID || intAttr(t, late, "cpa.attempt") != "2" {
		t.Fatalf("late usage not attached to exported root: %+v", late)
	}
	if tr.snapshot().lateAttached != 1 {
		t.Fatalf("late attach not counted")
	}
}

func TestSharedTraceIDSplitsByCompletionWindow(t *testing.T) {
	// A WebSocket connection reuses one TraceID for every turn.
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	turn1 := completionAt("ws", baseTime, baseTime.Add(2*time.Second), outcomeSucceeded, 200)
	turn2 := completionAt("ws", baseTime.Add(10*time.Second), baseTime.Add(12*time.Second), outcomeSucceeded, 200)
	turn2.RequestID = "req-ws-2"
	tr.addCompletion(turn1)
	tr.addCompletion(turn2)
	tr.addUsage(usageAt("ws", "u-turn2", baseTime.Add(10500*time.Millisecond), time.Second))
	tr.addUsage(usageAt("ws", "u-turn1", baseTime.Add(500*time.Millisecond), time.Second))
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
	spans := exportNow(t, tr, c)
	roots, children := rootsAndChildren(spans)
	if len(roots) != 2 || len(children) != 2 {
		t.Fatalf("want 2 roots + 2 children, got %d + %d", len(roots), len(children))
	}
	rootByReq := map[string]otlpSpan{}
	for _, r := range roots {
		rootByReq[strAttr(t, r, "cpa.request_id")] = r
	}
	for _, ch := range children {
		want := rootByReq["req-ws"]
		if strAttr(t, ch, "cpa.request_id") == "u-turn2" {
			want = rootByReq["req-ws-2"]
		}
		if ch.ParentSpanID != want.SpanID || ch.TraceID != want.TraceID {
			t.Fatalf("usage %s attached to wrong turn", strAttr(t, ch, "cpa.request_id"))
		}
	}
}

func TestSamplingKeepsOrDropsWholeTraces(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	cfg := testConfig(c.endpoint())
	cfg.SampleRate = 0.5
	tr := newTestTracer(t, cfg, clock)
	const n = 200
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("trace-%03d", i)
		tr.addUsage(usageAt(id, id+"-u1", baseTime.Add(100*time.Millisecond), time.Second))
		tr.addCompletion(completionAt(id, baseTime, baseTime.Add(3*time.Second), outcomeSucceeded, 200))
		tr.addUsage(usageAt(id, id+"-u2", baseTime.Add(200*time.Millisecond), time.Second))
	}
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
	spans := exportNow(t, tr, c)
	perTrace := map[string]int{}
	for _, s := range spans {
		perTrace[strAttr(t, s, "cpa.trace_id")]++
	}
	for id, count := range perTrace {
		if count != 3 {
			t.Fatalf("trace %s partially sampled: %d spans", id, count)
		}
	}
	kept := len(perTrace)
	if kept == 0 || kept == n {
		t.Fatalf("sampling at 0.5 kept %d of %d traces", kept, n)
	}
	if kept < n/4 || kept > 3*n/4 {
		t.Fatalf("sampling rate far off: kept %d of %d", kept, n)
	}
	// Deterministic: the same IDs give the same decision.
	tr2 := newTestTracer(t, cfg, clock)
	tr2.mu.Lock()
	for id := range perTrace {
		if !tr2.sampledLocked(id) {
			t.Fatalf("sampling not deterministic for %s", id)
		}
	}
	tr2.mu.Unlock()
}

func TestBoundedQueueDropsOldest(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	cfg := testConfig(c.endpoint())
	cfg.MaxQueueSpans = 3
	tr := newTestTracer(t, cfg, clock)
	for i := 1; i <= 5; i++ {
		tr.addUsage(usageAt("", fmt.Sprintf("u%d", i), baseTime, time.Second))
	}
	if tr.queueLen() != 3 || tr.snapshot().queueDropped != 2 {
		t.Fatalf("queue=%d dropped=%d", tr.queueLen(), tr.snapshot().queueDropped)
	}
	spans := exportNow(t, tr, c)
	var ids []string
	for _, s := range spans {
		ids = append(ids, strAttr(t, s, "cpa.request_id"))
	}
	if strings.Join(ids, ",") != "u3,u4,u5" {
		t.Fatalf("oldest spans not dropped first: %v", ids)
	}
}

func TestBoundedPendingEvictsOldest(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	cfg := testConfig(c.endpoint())
	cfg.MaxPendingTraces = 2
	tr := newTestTracer(t, cfg, clock)
	for _, id := range []string{"t1", "t2", "t3"} {
		tr.addCompletion(completionAt(id, baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200))
	}
	if tr.pendingLen() != 2 || tr.snapshot().evicted != 1 {
		t.Fatalf("pending=%d evicted=%d", tr.pendingLen(), tr.snapshot().evicted)
	}
	spans := exportNow(t, tr, c)
	if len(spans) != 1 || strAttr(t, spans[0], "cpa.trace_id") != "t1" {
		t.Fatalf("oldest trace should be exported early")
	}
}

func TestPerTraceOverflowBounded(t *testing.T) {
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig("http://127.0.0.1:1/v1/traces"), clock)
	for i := 0; i < maxOrphansPerTrace+10; i++ {
		tr.addUsage(usageAt("big", fmt.Sprintf("u%d", i), baseTime, time.Second))
	}
	if got := tr.snapshot().overflow; got != 10 {
		t.Fatalf("overflow=%d", got)
	}
}

func TestReconfigureKeepsBufferedData(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	cfg := testConfig("http://127.0.0.1:1/v1/traces")
	tr := newTestTracer(t, cfg, clock)
	tr.addCompletion(completionAt("keep", baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200))
	tr.addUsage(usageAt("", "loose", baseTime, time.Second))

	next := testConfig(c.endpoint())
	next.ServiceName = "renamed"
	tr.reconfigure(next)
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
	if spans := exportNow(t, tr, c); len(spans) != 2 {
		t.Fatalf("buffered data lost on reconfigure: %d spans", len(spans))
	}
	if !strings.Contains(string(c.bodies()[0]), `"renamed"`) {
		t.Fatalf("new service name not applied")
	}

	tr.addCompletion(completionAt("gone", baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200))
	off := defaultConfig()
	off.UIEnabled = false
	tr.reconfigure(off) // no endpoint and no Traces page: no-op, buffers released
	if tr.pendingLen() != 0 || tr.queueLen() != 0 {
		t.Fatalf("disabled plugin must not buffer")
	}
	tr.addUsage(usageAt("", "ignored", baseTime, time.Second))
	if tr.queueLen() != 0 {
		t.Fatalf("disabled plugin must ignore events")
	}
}

func TestFlushPendingIgnoresDeadlines(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	tr.addCompletion(completionAt("p1", baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200))
	tr.addUsage(usageAt("p2", "orphan", baseTime, time.Second))
	tr.flushPending()
	if spans := exportNow(t, tr, c); len(spans) != 3 {
		t.Fatalf("want 3 spans (root, synthesized root, child), got %d", len(spans))
	}
}

func TestIDsAreRandomAndNonZero(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tid, sid := newTraceID(), newSpanID()
		if tid == (traceID{}) || sid == (spanID{}) {
			t.Fatal("zero id")
		}
		if len(tid.String()) != 32 || len(sid.String()) != 16 || strings.ToLower(tid.String()) != tid.String() {
			t.Fatal("bad id encoding")
		}
		if seen[tid.String()] {
			t.Fatal("duplicate trace id")
		}
		seen[tid.String()] = true
	}
}

func TestSpanRingGrowAndShrink(t *testing.T) {
	r := spanRing{limit: 100}
	for i := 0; i < 150; i++ {
		r.push(otlpSpan{Name: fmt.Sprint(i)})
	}
	if r.len() != 100 {
		t.Fatalf("len=%d", r.len())
	}
	if dropped := r.setLimit(10); dropped != 90 || r.len() != 10 {
		t.Fatalf("dropped=%d len=%d", dropped, r.len())
	}
	out := r.pop(3)
	if out[0].Name != "140" || out[2].Name != "142" {
		t.Fatalf("order wrong: %v", out)
	}
}

func TestModelListCompletionsAreSkipped(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	// WriteModelListResponse emits a completion with no model and no attempts.
	tr.addCompletion(requestCompletion{RequestID: "r", TraceID: "models", SourceFormat: "openai", Outcome: outcomeSucceeded, StatusCode: 200, StartedAt: baseTime, CompletedAt: baseTime})
	// A model-less failure is still traced.
	tr.addCompletion(requestCompletion{RequestID: "r2", TraceID: "bad", Outcome: "rejected", StatusCode: 403, StartedAt: baseTime, CompletedAt: baseTime})
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
	spans := exportNow(t, tr, c)
	if len(spans) != 1 || strAttr(t, spans[0], "cpa.trace_id") != "bad" || tr.snapshot().skipped != 1 {
		t.Fatalf("want only the rejected request traced, got %d spans", len(spans))
	}
}

func TestRootCarriesMaskedClientKey(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	u := usageAt("k", "u1", baseTime.Add(time.Millisecond), time.Millisecond)
	u.APIKey = "raw-client-key"
	tr.addUsage(u)
	tr.addCompletion(completionAt("k", baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200))
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
	roots, children := rootsAndChildren(exportNow(t, tr, c))
	if strAttr(t, roots[0], "cpa.client_key") != maskClientKey("raw-client-key") || strAttr(t, children[0], "cpa.client_key") != maskClientKey("raw-client-key") {
		t.Fatal("masked client key missing")
	}
	if strings.Contains(string(c.bodies()[0]), "raw-client-key") {
		t.Fatal("raw client key exported")
	}
}
