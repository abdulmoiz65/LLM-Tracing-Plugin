package main

import (
	"testing"
	"time"
)

// opusTurn is one turn of a Claude Opus 5.5 conversation on an API key
// (5-minute cache writes: $5/MTok, reads $0.20/MTok).
func opusTurn(n int, at time.Duration, prompt, read, write int64) *uiTrace {
	t := sessTrace(n, "K", "claude-opus-5-5", at, prompt, false)
	t.provider, t.authType, t.authIndex = "claude", "apikey", "idx1"
	t.cacheRead, t.cacheWrite, t.totalInput = read, write, prompt
	t.usage = []attemptUsage{{model: "claude-opus-5-5", provider: "claude", authType: "apikey",
		tok: spanTokens{totalInput: prompt, uncached: prompt - read - write, cacheRead: read, cacheWrite: write, output: 100}}}
	return t
}

func insightStore(traces ...*uiTrace) *traceStore {
	s := newTraceStore()
	s.configure(true, 500, 20000, 0)
	for _, t := range traces {
		s.add(t)
	}
	return s
}

func TestInsightsCacheBreakKinds(t *testing.T) {
	s := insightStore(
		opusTurn(1, 0, 100_000, 0, 99_000),                        // first turn writes the cache: not a break
		opusTurn(2, time.Minute, 101_000, 99_000, 1_000),          // normal: reads, small write
		opusTurn(3, 2*time.Minute, 102_000, 0, 101_000),           // rewritten 1 minute later: prefix changed
		opusTurn(4, 3*time.Minute, 103_000, 101_000, 2_000),       // normal
		opusTurn(5, 20*time.Minute, 104_000, 0, 104_000),          // rewritten after 17 idle minutes: unknown cause
		opusTurn(6, 21*time.Minute, 105_000, 100_000, 5_000),      // normal
		opusTurn(7, 22*time.Minute, 106_000, 0, minBreakTokens-1), // too small to count
	)
	in := s.insights(0, "all", nil)
	b := in.CacheBreaks
	if b.PrefixChanged != 1 || b.ExpiredOrChanged != 1 || b.AccountChanged != 0 || len(b.Top) != 2 {
		t.Fatalf("breaks: %+v", b)
	}
	// Extra cost of a break: write tokens x (write rate - read rate).
	if !near(b.PrefixChangedUSD, 101_000*(5-0.2)/1e6) || !near(b.ExpiredOrChangedUSD, 104_000*(5-0.2)/1e6) {
		t.Fatalf("extra: %v %v", b.PrefixChangedUSD, b.ExpiredOrChangedUSD)
	}
	if b.Top[0].Kind != "expired_or_changed" || b.Top[1].Kind != "prefix_changed" || b.Top[1].GapSeconds != 59 {
		t.Fatalf("top: %+v", b.Top)
	}
	// A time range that starts after the breaks sees none.
	since := baseTime.Add(21 * time.Minute).UnixNano()
	if in := s.insights(since, "15m", nil); in.CacheBreaks.PrefixChanged+in.CacheBreaks.ExpiredOrChanged != 0 || in.Requests != 2 {
		t.Fatalf("range: %+v", in)
	}
}

// A conversation moved to another account rebuilds its cache: that cause
// is certain and wins over the timing-based kinds.
func TestInsightsAccountSwitch(t *testing.T) {
	second := opusTurn(2, time.Minute, 101_000, 0, 101_000)
	second.authIndex = "idx2"
	late := opusTurn(3, 30*time.Minute, 102_000, 0, 102_000)
	late.authIndex = "idx3"
	in := insightStore(opusTurn(1, 0, 100_000, 0, 99_000), second, late).insights(0, "all", nil)
	b := in.CacheBreaks
	if b.AccountChanged != 2 || b.PrefixChanged != 0 || b.ExpiredOrChanged != 0 || !near(b.AccountChangedUSD, 203_000*(5-0.2)/1e6) {
		t.Fatalf("account switch: %+v", b)
	}
}

func TestInsightsLargeContextAndRetries(t *testing.T) {
	big := opusTurn(1, 0, 300_000, 290_000, 1_000)
	small := opusTurn(2, time.Minute, 150_000, 0, 0)
	small.clientKey = "other" // its own session
	retried := opusTurn(3, 2*time.Minute, 1_000, 0, 0)
	retried.clientKey = "third"
	retried.attempts, retried.failed = 3, 2
	retried.hasServing, retried.servingFailed = true, false
	retried.servingStart = retried.start + int64(1500*time.Millisecond)
	retried.spans = []uiSpan{
		{name: "root", kind: spanKindServer},
		{kind: spanKindClient, failed: true, attrs: []uiAttr{{key: "cpa.auth_index", s: "aaaa1111"}, {key: "cpa.auth_type", s: "oauth"}}},
		{kind: spanKindClient, failed: true, attrs: []uiAttr{{key: "cpa.auth_index", s: "aaaa1111"}, {key: "cpa.auth_type", s: "oauth"}}},
		{kind: spanKindClient, attrs: []uiAttr{{key: "cpa.auth_index", s: "bbbb2222"}, {key: "cpa.auth_type", s: "oauth"}}},
	}
	s := insightStore(big, small, retried)
	in := s.insights(0, "all", map[string]string{"aaaa1111": "a@example.com", "bbbb2222": "b@example.com"})
	c := in.Context
	if c.Sessions != 1 || c.Requests != 1 || !near(c.AboveUSD, 100_000*0.2/1e6) || c.Top[0].PeakTokens != 300_000 {
		t.Fatalf("context: %+v", c)
	}
	r := in.Retries
	if r.RetriedRequests != 1 || r.FailedAttempts != 2 || r.AddedWaitMS != 1500 {
		t.Fatalf("retries: %+v", r)
	}
	if len(r.Accounts) != 2 || r.Accounts[0].Account != "a@example.com" || r.Accounts[0].Failed != 2 || r.Accounts[0].FailRate != 1 {
		t.Fatalf("accounts: %+v", r.Accounts)
	}
	if in.Requests != 3 || in.CostUSD <= 0 || in.CacheSavingsUSD <= 0 {
		t.Fatalf("totals: %+v", in)
	}
}

func TestWhatIf(t *testing.T) {
	s := insightStore(opusTurn(1, 0, 100_000, 90_000, 0), opusTurn(2, time.Minute, 100_000, 90_000, 0))
	w, ok := s.whatIf(0, "claude-sonnet-5-5")
	if !ok || w.Requests != 2 {
		t.Fatalf("what-if: %+v ok=%v", w, ok)
	}
	// Per turn: 10k uncached, 90k read, 100 output.
	opus := (10_000*4 + 90_000*0.2 + 100*20) / 1e6
	sonnet := (10_000*2 + 90_000*0.2 + 100*10) / 1e6
	if !near(w.CurrentUSD, roundUSD(2*opus)) || !near(w.WhatIfUSD, roundUSD(2*sonnet)) || w.DifferenceUSD >= 0 {
		t.Fatalf("prices: %+v", w)
	}
	if _, ok := s.whatIf(0, "mystery-model-9"); ok {
		t.Fatal("unpriced target accepted")
	}
}

func TestKeepTopBounded(t *testing.T) {
	var list []int
	for i := 0; i < 50; i++ {
		list = keepTop(list, i, func(a, b int) bool { return a > b })
	}
	if len(list) != maxInsightItems || list[0] != 49 || list[9] != 40 {
		t.Fatalf("top: %v", list)
	}
}
