package main

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

func sessTrace(n int, key, model string, at time.Duration, prompt int64, failed bool) *uiTrace {
	t := &uiTrace{
		clientKey: key, model: model, servingModel: model, servingPrompt: prompt,
		start: baseTime.Add(at).UnixNano(), end: baseTime.Add(at + time.Second).UnixNano(),
		outcome: outcomeSucceeded, statusCode: 200, totalInput: prompt,
	}
	if failed {
		t.outcome, t.statusCode = "failed", 500
	}
	t.tid[15] = byte(n)
	t.tid[0] = 1
	return t
}

func TestBuildSessionsGroupsConversations(t *testing.T) {
	m := "claude-opus-5-5"
	traces := []*uiTrace{
		sessTrace(1, "A", m, 0, 100_000, false),
		sessTrace(2, "A", m, time.Minute, 120_000, false),
		sessTrace(3, "A", m, 2*time.Minute, 140_000, false),
		sessTrace(4, "A", m, 150*time.Second, 10_000, false), // new conversation, same key
		sessTrace(5, "A", m, 3*time.Minute, 15_000, false),   // continues #4
		sessTrace(6, "A", m, 4*time.Minute, 150_000, false),  // continues #1..#3
		sessTrace(7, "B", m, 4*time.Minute, 150_000, false),  // other client key
		sessTrace(8, "A", m, 5*time.Minute, 0, true),         // error without usage: latest open session
		sessTrace(9, "A", m, 40*time.Minute, 160_000, false), // after a 35 min pause
		sessTrace(10, "A", "claude-haiku-5-5", 4*time.Minute, 150_000, false),
	}
	idx := buildSessions(traces, 30*time.Minute)
	if len(idx.sessions) != 5 {
		t.Fatalf("sessions: %d", len(idx.sessions))
	}
	group := func(n int) string { return idx.member[traces[n-1]].session.id }
	for _, n := range []int{2, 3, 6, 8} {
		if group(n) != group(1) {
			t.Errorf("trace %d not in session of trace 1", n)
		}
	}
	if group(5) != group(4) || group(4) == group(1) {
		t.Error("parallel conversation not separated")
	}
	if group(7) == group(1) || group(9) == group(1) || group(10) == group(1) {
		t.Error("key, gap or model did not split sessions")
	}
	if m := idx.member[traces[5]]; m.seq != 4 || len(m.session.traces) != 5 {
		t.Errorf("seq=%d len=%d", m.seq, len(m.session.traces))
	}
	if group(1) != traces[0].tid.String() {
		t.Error("session id must be the first trace id")
	}
}

func TestSessionRowsAndCostThroughStore(t *testing.T) {
	clock := newFakeClock()
	tr := newTestTracer(t, uiConfig(), clock)
	start := clock.Now()
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("s%d", i)
		at := start.Add(time.Duration(i) * time.Minute)
		c := completionAt(id, at, at.Add(time.Second), outcomeSucceeded, 200)
		c.Model, c.RequestedModel = "claude-opus-5-5", "claude-opus-5-5"
		tr.addCompletion(c)
		u := usageAt(id, id+"-a", at.Add(time.Millisecond), time.Millisecond)
		u.Model, u.Alias = "claude-opus-5-5", "claude-opus-5-5"
		prompt := int64(300_000 + i*100_000)
		u.Detail = usageDetail{InputTokens: 100, CacheReadTokens: prompt - 100, OutputTokens: 500}
		tr.addUsage(u)
	}
	// An unpriced model on another key.
	other := start.Add(30 * time.Second)
	tr.addCompletion(completionAt("x", other, other.Add(time.Second), outcomeSucceeded, 200))
	ux := usageAt("x", "x-a", other.Add(time.Millisecond), time.Millisecond)
	ux.Model, ux.Alias = "mystery-model-9", "mystery-model-9"
	tr.addUsage(ux)
	finalize(tr, clock)

	rows := tr.store.sessions(0, 50, clock.Now(), nil)
	var opus *sessionJSON
	for i := range rows {
		if rows[i].Model == "claude-opus-5-5" {
			opus = &rows[i]
		}
	}
	if len(rows) != 2 || opus == nil {
		t.Fatalf("rows: %+v", rows)
	}
	if opus.Requests != 4 || opus.FirstContextTokens != 300_000 || opus.ContextTokens != 600_000 || opus.ContextWindow != 1_000_000 ||
		opus.ContextUsed != 0.6 || opus.GrowthPerRequest != 100_000 || opus.RequestsUntilFull != 4 || !opus.Priced {
		t.Fatalf("opus session: %+v", *opus)
	}
	perRequest := func(prompt float64) float64 { return (100*4 + (prompt-100)*0.2 + 500*20) / 1e6 }
	want := perRequest(300_000) + perRequest(400_000) + perRequest(500_000) + perRequest(600_000)
	if !near(opus.CostUSD, roundUSD(want)) || opus.NoCacheCostUSD <= opus.CostUSD {
		t.Fatalf("cost %v want %v", opus.CostUSD, want)
	}

	items := tr.store.list(traceFilter{limit: 50, status: "all", session: opus.ID}, nil)
	if len(items) != 4 || items[0].SessionSeq != 4 || items[0].SessionRequests != 4 || items[0].ContextTokens != 600_000 || items[0].ContextUsed != 0.6 {
		t.Fatalf("session filter items: %+v", items)
	}

	s := tr.summaryFor(0, "all")
	if s.Requests != 5 || s.UnpricedRequests != 1 || len(s.UnpricedModels) != 1 || s.UnpricedModels[0] != "mystery-model-9" {
		t.Fatalf("summary: %+v", s.summaryStats)
	}
	if !near(s.CostUSD, roundUSD(want)) || s.CacheSavingsUSD <= 0 || s.Range != "all" || s.WindowSeconds != 0 || s.OldestTraceAt == "" {
		t.Fatalf("summary cost: %+v", s)
	}

	// Through the management handler: range and session validation.
	p := &pluginState{tracer: tr}
	bad := p.handleManagement(managementRequest{Method: "GET", Path: apiBase + "/traces", Query: url.Values{"session": {"nope"}}})
	if bad.StatusCode != 400 {
		t.Fatalf("bad session: %d", bad.StatusCode)
	}
	ok := p.handleManagement(managementRequest{Method: "GET", Path: apiBase + "/sessions", Query: url.Values{"range": {"15m"}}})
	if ok.StatusCode != 200 {
		t.Fatalf("sessions: %d %s", ok.StatusCode, ok.Body)
	}
	badRange := p.handleManagement(managementRequest{Method: "GET", Path: apiBase + "/summary", Query: url.Values{"range": {"1y"}}})
	if badRange.StatusCode != 400 {
		t.Fatalf("bad range: %d", badRange.StatusCode)
	}
}
