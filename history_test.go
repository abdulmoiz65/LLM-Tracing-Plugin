package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistoryRoundTripSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	cfg := uiConfig()
	cfg.UIMaxTraces = 3 // only the newest 3 keep spans
	h := newHistoryFile(dir, 7, clock.Now)
	tr := newTestTracer(t, cfg, clock)
	tr.store.setPersist(h.enqueue)
	historyTracerInto(t, tr, clock, 6)
	h.close()
	before := tr.summaryFor(0, "all")
	beforeSessions := tr.store.sessions(0, 50, clock.Now(), nil)

	files, _ := historyFiles(dir)
	if len(files) != 1 || h.status().Written < 6 {
		t.Fatalf("files=%v status=%+v", files, h.status())
	}

	// "Restart": a fresh tracer loads the saved history.
	fresh := newTestTracer(t, cfg, clock)
	h2 := newHistoryFile(dir, 7, clock.Now)
	loaded, err := h2.load()
	if err != nil {
		t.Fatal(err)
	}
	if n := fresh.store.importHistory(loaded); n != 6 {
		t.Fatalf("imported %d", n)
	}
	after := fresh.summaryFor(0, "all")
	if after.Requests != 6 || after.CostUSD != before.CostUSD || after.TotalInputTokens != before.TotalInputTokens || after.CostUSD <= 0 {
		t.Fatalf("before=%+v after=%+v", before.summaryStats, after.summaryStats)
	}
	afterSessions := fresh.store.sessions(0, 50, clock.Now(), nil)
	if len(afterSessions) != 1 || afterSessions[0].Requests != 6 || afterSessions[0].CostUSD != beforeSessions[0].CostUSD {
		t.Fatalf("sessions before=%+v after=%+v", beforeSessions, afterSessions)
	}
	items := fresh.store.list(traceFilter{limit: 50, status: "all"}, nil)
	detailed := 0
	for _, it := range items {
		if it.HasDetail {
			detailed++
		}
	}
	if detailed != 3 || !items[0].HasDetail || items[5].HasDetail {
		t.Fatalf("detail tier: %d detailed, newest=%v oldest=%v", detailed, items[0].HasDetail, items[5].HasDetail)
	}
	d, ok := fresh.store.get(mustTraceID(t, items[0].TraceID), nil)
	if !ok || len(d.Spans) != 2 {
		t.Fatalf("newest detail spans: %d", len(d.Spans))
	}
	old, ok := fresh.store.get(mustTraceID(t, items[5].TraceID), nil)
	if !ok || len(old.Spans) != 0 || old.CostUSD <= 0 {
		t.Fatalf("old trace must keep totals without spans: %+v", old.traceItemJSON)
	}
	// Importing the same history again adds nothing.
	if n := fresh.store.importHistory(loaded); n != 0 {
		t.Fatalf("duplicate import added %d", n)
	}
}

// historyTracerInto runs historyTracer's requests on an existing tracer.
func historyTracerInto(t *testing.T, tr *tracer, clock *fakeClock, n int) *tracer {
	t.Helper()
	start := clock.Now()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("h%02d", i)
		at := start.Add(time.Duration(i) * time.Minute)
		c := completionAt(id, at, at.Add(time.Second), outcomeSucceeded, 200)
		c.Model, c.RequestedModel = "claude-opus-5-5", "claude-opus-5-5"
		tr.addCompletion(c)
		u := usageAt(id, id+"-a", at.Add(time.Millisecond), time.Millisecond)
		u.Model, u.Alias = "claude-opus-5-5", "claude-opus-5-5"
		u.Detail = usageDetail{InputTokens: 10, CacheReadTokens: int64(100_000 + i*1000), CacheCreationTokens: 50, OutputTokens: 200}
		tr.addUsage(u)
	}
	clock.Advance(time.Duration(n) * time.Minute)
	finalize(tr, clock)
	return tr
}

func TestHistoryTiersAndAgeLimit(t *testing.T) {
	clock := newFakeClock()
	s := newTraceStore()
	s.now = clock.Now
	s.configure(true, 2, 4, time.Hour)
	add := func(n int, at time.Time) {
		tr := &uiTrace{outcome: outcomeSucceeded, start: at.UnixNano(), end: at.UnixNano(), spans: []uiSpan{{name: "root"}}}
		tr.tid[0], tr.tid[15] = 9, byte(n)
		s.add(tr)
	}
	for i := 0; i < 6; i++ {
		add(i, clock.Now().Add(time.Duration(i)*time.Second))
	}
	if s.len() != 4 || s.detailed != 2 {
		t.Fatalf("len=%d detailed=%d", s.len(), s.detailed)
	}
	if s.buf[0].hasSpans || s.buf[1].hasSpans || !s.buf[2].hasSpans || !s.buf[3].hasSpans || s.buf[0].spans != nil {
		t.Fatal("only the newest 2 may keep spans")
	}
	// Two hours later every old request is past the age limit.
	clock.Advance(2 * time.Hour)
	add(99, clock.Now())
	if s.len() != 1 || len(s.byID) != 1 {
		t.Fatalf("age limit: len=%d ids=%d", s.len(), len(s.byID))
	}
}

func TestHistoryLateUpdateLastLineWins(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	h := newHistoryFile(dir, 7, clock.Now)
	tr := newTestTracer(t, uiConfig(), clock)
	tr.store.setPersist(h.enqueue)
	start := clock.Now()
	tr.addCompletion(completionAt("late", start, start.Add(time.Second), outcomeSucceeded, 200))
	tr.addUsage(usageAt("late", "late-a", start.Add(time.Millisecond), time.Millisecond))
	finalize(tr, clock)
	tr.addUsage(usageAt("late", "late-b", start.Add(2*time.Millisecond), time.Millisecond)) // after finalize
	h.close()
	loaded, err := newHistoryFile(dir, 7, clock.Now).load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("loaded %d err=%v", len(loaded), err)
	}
	if loaded[0].attempts != 2 || loaded[0].input != 20 {
		t.Fatalf("late attempt not saved: attempts=%d input=%d", loaded[0].attempts, loaded[0].input)
	}
}

func TestHistoryPruneAndDamagedLines(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	oldName := historyFilePrefix + clock.Now().UTC().AddDate(0, 0, -10).Format("20060102") + historyFileSuffix
	keepName := historyFilePrefix + clock.Now().UTC().Format("20060102") + historyFileSuffix
	good := encodeTrace(&uiTrace{tid: newTraceID(), start: clock.Now().UnixNano(), end: clock.Now().UnixNano(), outcome: outcomeSucceeded, model: "m"})
	tampered := strings.Replace(string(encodeTrace(&uiTrace{tid: newTraceID(), start: clock.Now().UnixNano(), outcome: outcomeSucceeded})),
		`"tok":[0,`, `"tok":[-5,`, 1)
	content := strings.Join([]string{string(good), "not json", `{"v":99}`, tampered, `{"v":1,"id":"zz","s":1}`}, "\n") + "\n"
	for name, body := range map[string]string{oldName: content, keepName: content, "notes.txt": "keep me"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := newHistoryFile(dir, 7, clock.Now)
	loaded, err := h.load()
	if err != nil || len(loaded) != 1 || loaded[0].model != "m" || h.status().Skipped != 4 {
		t.Fatalf("loaded=%d err=%v status=%+v", len(loaded), err, h.status())
	}
	h.flush() // first flush of the day prunes
	if _, errStat := os.Stat(filepath.Join(dir, oldName)); !os.IsNotExist(errStat) {
		t.Fatal("file older than history_days not deleted")
	}
	for _, name := range []string{keepName, "notes.txt"} {
		if _, errStat := os.Stat(filepath.Join(dir, name)); errStat != nil {
			t.Fatalf("%s must be kept: %v", name, errStat)
		}
	}
}

func TestPluginLoadsHistoryOnStart(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	cfg := uiConfig()
	cfg.DataDir = dir

	// First run writes three requests and shuts down.
	first := newPluginState(clock.Now, nil, false)
	first.tracer.reconfigure(cfg)
	first.mu.Lock()
	first.syncHistoryLocked(cfg)
	first.mu.Unlock()
	waitLoaded(t, first)
	historyTracerInto(t, first.tracer, clock, 3)
	first.shutdown()

	// Second run (same data_dir) sees them.
	second := newPluginState(clock.Now, nil, false)
	second.tracer.reconfigure(cfg)
	second.mu.Lock()
	second.syncHistoryLocked(cfg)
	second.mu.Unlock()
	waitLoaded(t, second)
	if got := second.tracer.summaryFor(0, "all").Requests; got != 3 {
		t.Fatalf("requests after restart: %d", got)
	}
	resp := second.handleManagement(managementRequest{Method: "GET", Path: apiBase + "/summary"})
	if !strings.Contains(string(resp.Body), `"loaded_requests":3`) {
		t.Fatalf("history status: %s", resp.Body)
	}
	second.shutdown()
}

// waitLoaded waits for the background history load (a condition, not a
// fixed sleep).
func waitLoaded(t *testing.T, p *pluginState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := p.currentHistory().status(); st.Enabled && !st.Loading {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("history did not finish loading")
}
