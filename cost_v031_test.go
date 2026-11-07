package main

import (
	"testing"
	"time"
)

// A price change must not rewrite the cost of requests already recorded.
func TestCostIsFrozenAtRecordTime(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	cfg := uiConfig()
	h := newHistoryFile(dir, 7, clock.Now)
	tr := newTestTracer(t, cfg, clock)
	tr.store.setPersist(h.enqueue)
	historyTracerInto(t, tr, clock, 2)
	before := tr.summaryFor(0, "all").CostUSD
	if before <= 0 {
		t.Fatal("no cost recorded")
	}

	// The price of claude-opus-5-5 doubles.
	cfg.Pricing = map[string]modelPrice{"claude-opus-5-5": {Input: 8, Output: 40, CacheRead: 0.4, CacheWrite: 10, CacheWrite1h: 16}}
	tr.reconfigure(cfg)
	if got := tr.summaryFor(0, "all").CostUSD; got != before {
		t.Fatalf("old requests re-priced: before=%v after=%v", before, got)
	}
	// A new request uses the new price.
	historyTracerInto(t, tr, clock, 1)
	if got := tr.summaryFor(0, "all").CostUSD; got <= before*1.4 {
		t.Fatalf("new request not on the new price: before=%v after=%v", before, got)
	}
	h.close()

	// The frozen cost survives a restart, even with the new price configured.
	fresh := newTestTracer(t, cfg, clock)
	loaded, err := newHistoryFile(dir, 7, clock.Now).load()
	if err != nil {
		t.Fatal(err)
	}
	fresh.store.importHistory(loaded)
	items := fresh.store.list(traceFilter{limit: 10, status: "all"}, nil)
	if len(items) != 3 {
		t.Fatalf("items %d", len(items))
	}
	oldTotal := items[1].CostUSD + items[2].CostUSD
	if diff := oldTotal - before; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("frozen cost changed across restart: %v vs %v", oldTotal, before)
	}
	if items[2].PriceSource != claudeSource || items[0].PriceSource != "config" {
		t.Fatalf("sources: %q %q", items[2].PriceSource, items[0].PriceSource)
	}
}

// Claude OAuth cache writes have a guessed TTL: report the 5-minute lower
// bound next to the 1-hour figure.
func TestCostRangeForGuessedCacheWrite(t *testing.T) {
	p := newPricer(nil, nil)
	tok := spanTokens{totalInput: 1000, cacheWrite: 1000}
	c := p.cost("claude-opus-5-5", "claude", "oauth", tok)
	if !near(c.cost, 1000*8/1e6) || !near(c.costMin, 1000*5/1e6) {
		t.Fatalf("range: %+v", c)
	}
	if c := p.cost("claude-opus-5-5", "claude", "apikey", tok); c.costMin != c.cost {
		t.Fatalf("API key writes are not guessed: %+v", c)
	}
	p.cacheTTL = cacheTTL1h
	if c := p.cost("claude-opus-5-5", "claude", "oauth", tok); c.costMin != c.cost {
		t.Fatalf("configured TTL is not a guess: %+v", c)
	}
}

func TestCanceledWithoutUsageHasUnknownCost(t *testing.T) {
	clock := newFakeClock()
	tr := newTestTracer(t, uiConfig(), clock)
	start := clock.Now()
	tr.addCompletion(completionAt("esc", start, start.Add(time.Second), outcomeCanceled, 499))
	finalize(tr, clock)
	s := tr.summaryFor(0, "all")
	items := tr.store.list(traceFilter{limit: 5, status: "all"}, nil)
	if s.UnknownCostRequests != 1 || len(items) != 1 || !items[0].CostUnknown {
		t.Fatalf("summary=%d items=%+v", s.UnknownCostRequests, items)
	}
}

func TestPricesStaleness(t *testing.T) {
	_, meta := loadGenerated()
	at, err := time.Parse(time.RFC3339, meta.Date)
	if err != nil {
		t.Fatal(err)
	}
	if pricesAgeDays(at.Add(10*24*time.Hour)) != 10 {
		t.Fatal("age wrong")
	}
	clock := newFakeClock()
	tr := newTestTracer(t, uiConfig(), clock)
	tr.now = func() time.Time { return at.Add(40 * 24 * time.Hour) }
	if s := tr.summaryFor(0, "all"); !s.PricesStale || s.PricesAgeDays != 40 {
		t.Fatalf("stale=%v age=%d", s.PricesStale, s.PricesAgeDays)
	}
}
