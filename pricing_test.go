package main

import (
	"math"
	"net/url"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestModelLookup(t *testing.T) {
	p := newPricer(nil, nil)
	cases := map[string]float64{
		"claude-opus-5-5":              4,
		"Claude-Opus-5-5":              4,
		"claude-opus-5-5-20260401":     4,
		"claude-opus-5-5(high)":        4,
		"claude-opus-5-5[1m]":          4,
		"anthropic/claude-opus-5-5":    4,
		"us.anthropic.claude-opus-5-5": 4,
		"claude-opus-5":                5,
		"claude-opus-5-20260101":       5,
		"claude-fable-5-1":             10,
		"claude-sonnet-5-5-thinking":   2,
		"claude-haiku-4-5":             1,
		"claude-haiku-5-5":             0.10,
		"claude-sonnet-4-6@20260101":   3,
	}
	for model, input := range cases {
		price, ok := p.price(model)
		if !ok || !near(price.Input, input) {
			t.Errorf("%s: got %+v ok=%v, want input %v", model, price, ok, input)
		}
	}
	for _, model := range []string{"", "mystery-model-9", "claude-opus", "claude-opus-55", "claude-opus-5x"} {
		if _, ok := p.price(model); ok {
			t.Errorf("%q must have no price", model)
		}
	}
	if p.contextWindow("claude-opus-5-5") != 1_000_000 || p.contextWindow("claude-haiku-4-5") != 200_000 || p.contextWindow("mystery-model-9") != 0 {
		t.Fatal("context windows wrong")
	}
}

func TestCostClaudeCache(t *testing.T) {
	p := newPricer(nil, nil)
	// 15 uncached + 4529 cache write, 491 output on Opus 5.5:
	// (15*4 + 4529*5 + 491*20) / 1e6.
	tok := spanTokens{totalInput: 4544, uncached: 15, cacheWrite: 4529, output: 491}
	c := p.cost("claude-opus-5-5", "claude", "apikey", tok)
	if !c.ok || !near(c.cost, (15*4+4529*5+491*20)/1e6) || !near(c.noCache, (4544*4+491*20)/1e6) {
		t.Fatalf("write cost: %+v", c)
	}
	// Cache read of 369k: 0.05x input on Opus 5.5.
	tok = spanTokens{totalInput: 370_000, uncached: 526, cacheRead: 369_000, cacheWrite: 474, output: 403}
	c = p.cost("claude-opus-5-5", "claude", "", tok)
	want := (526*4 + 369_000*0.2 + 474*5 + 403*20) / 1e6
	if !c.ok || !near(c.cost, want) || c.noCache <= c.cost {
		t.Fatalf("read cost: %+v want %v", c, want)
	}
}

func TestCacheWriteTTL(t *testing.T) {
	tok := spanTokens{totalInput: 1000, cacheWrite: 1000}
	p := newPricer(nil, nil)
	if c := p.cost("claude-opus-5-5", "claude", "oauth", tok); !near(c.cost, 1000*8/1e6) {
		t.Fatalf("auto oauth must use the 1h rate (2x input): %v", c.cost)
	}
	if c := p.cost("claude-opus-5-5", "claude", "apikey", tok); !near(c.cost, 1000*5/1e6) {
		t.Fatalf("auto api key must use the 5m rate: %v", c.cost)
	}
	p.cacheTTL = cacheTTL1h
	if c := p.cost("claude-opus-5-5", "claude", "apikey", tok); !near(c.cost, 1000*8/1e6) {
		t.Fatalf("forced 1h: %v", c.cost)
	}
	p.cacheTTL = cacheTTL5m
	if c := p.cost("claude-opus-5-5", "claude", "oauth", tok); !near(c.cost, 1000*5/1e6) {
		t.Fatalf("forced 5m: %v", c.cost)
	}
	cfg, warnings, _ := parseConfig([]byte("cache_write_ttl: 1H"))
	if cfg.CacheWriteTTL != cacheTTL1h || len(warnings) != 0 || cfg.pricer().cacheTTL != cacheTTL1h {
		t.Fatalf("parse: %q %v", cfg.CacheWriteTTL, warnings)
	}
	if _, warnings, _ = parseConfig([]byte("cache_write_ttl: 2h")); len(warnings) != 1 {
		t.Fatal("bad ttl accepted")
	}
}

func TestCostLongContextTierAndGemini(t *testing.T) {
	p := newPricer(map[string]modelPrice{"gemini-9-pro": {Input: 1, Output: 10, CacheRead: 0.1, CacheWrite: 0}}, nil)
	short := p.cost("claude-haiku-5-5", "claude", "", spanTokens{totalInput: 1000, uncached: 1000, output: 1000})
	long := p.cost("claude-haiku-5-5", "claude", "", spanTokens{totalInput: 200_000, uncached: 200_000, output: 1000})
	if !near(short.cost, (1000*0.1+1000*0.5)/1e6) || !near(long.cost, (200_000*0.5+1000*2.5)/1e6) {
		t.Fatalf("haiku tiers: short=%+v long=%+v", short, long)
	}
	// Gemini thinking tokens are billed as output.
	g := p.cost("gemini-9-pro", "gemini", "oauth", spanTokens{totalInput: 100, uncached: 100, output: 10, reasoning: 40})
	if !g.ok || !near(g.cost, (100*1+50*10)/1e6) {
		t.Fatalf("gemini: %+v", g)
	}
	// Unknown model with tokens is unpriced; no tokens is free.
	if c := p.cost("mystery", "x", "", spanTokens{totalInput: 1, uncached: 1}); c.ok {
		t.Fatal("unknown model priced")
	}
	if c := p.cost("mystery", "x", "", spanTokens{}); !c.ok || c.cost != 0 {
		t.Fatal("empty usage must cost nothing")
	}
}

func TestParsePricingConfig(t *testing.T) {
	cfg, warnings, err := parseConfig([]byte(`
pricing:
  gpt-9: {input: 1.25, output: 10}
  bad: {input: -1, output: 2}
  worse: 7
context_windows:
  gpt-9: 400000
  tiny: 5
session_gap_minutes: 45
`))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Pricing["gpt-9"]
	if !near(got.Input, 1.25) || !near(got.Output, 10) || !near(got.CacheRead, 0.125) || !near(got.CacheWrite, 1.5625) {
		t.Fatalf("gpt-9 price: %+v", got)
	}
	if _, ok := cfg.Pricing["bad"]; ok || len(cfg.Pricing) != 1 {
		t.Fatalf("invalid prices kept: %+v", cfg.Pricing)
	}
	if cfg.ContextWindows["gpt-9"] != 400000 || len(cfg.ContextWindows) != 1 || cfg.SessionGap != 45*time.Minute {
		t.Fatalf("contexts=%v gap=%v", cfg.ContextWindows, cfg.SessionGap)
	}
	if len(warnings) != 3 {
		t.Fatalf("warnings: %v", warnings)
	}
	// User prices win over built-in ones.
	p := newPricer(map[string]modelPrice{"claude-opus-5-5": {Input: 1, Output: 1}}, nil)
	if price, _ := p.price("claude-opus-5-5-20260401"); !near(price.Input, 1) {
		t.Fatal("override ignored")
	}
}

func TestParseRange(t *testing.T) {
	for label, want := range map[string]time.Duration{"15m": 15 * time.Minute, "ALL": 0, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour} {
		got, _, errMsg := parseRange(url.Values{"range": {label}}, "1h")
		if errMsg != "" || got != want {
			t.Errorf("%s: %v %q", label, got, errMsg)
		}
	}
	if got, label, _ := parseRange(url.Values{}, "1h"); got != time.Hour || label != "1h" {
		t.Fatal("default range")
	}
	if _, _, errMsg := parseRange(url.Values{"range": {"2y"}}, "1h"); errMsg == "" {
		t.Fatal("bad range accepted")
	}
}

func TestMatchRules(t *testing.T) {
	cases := []struct {
		key, model string
		want       int
	}{
		{"kimi-k2", "kimi-k2", 7},
		{"kimi-k2", "kimi-k2.8", -1}, // a dot is a new version, not a suffix
		{"gemini-3", "gemini-3.5-flash", -1},
		{"claude-opus-5-5", "claude-opus-5-5-high", 15},
		{"gpt-6-sol", "gpt-6-sol-2026-09-01", 9},
		{"deepseek", "deepseek-v9", -1}, // keys without a digit match exactly only
		{"deepseek", "deepseek", 8},
	}
	for _, c := range cases {
		if got := matchLen(c.key, c.model); got != c.want {
			t.Errorf("matchLen(%q, %q) = %d, want %d", c.key, c.model, got, c.want)
		}
	}
}

// The generated LiteLLM table prices the other providers CPA serves.
func TestGeneratedTableCoversProviders(t *testing.T) {
	_, meta := loadGenerated()
	if meta.Models < 1000 || meta.Commit == "" || meta.Date == "" {
		t.Fatalf("generated table: %+v", meta)
	}
	p := newPricer(nil, nil)
	for _, model := range []string{
		"gpt-6-sol", "gpt-5.6-terra", "gpt-5.5", // OpenAI / Codex
		"gemini-2.5-pro", "gemini-3-pro", "gemini-3-flash", // Gemini, Vertex (no -preview)
		"gemini-4-argon-high", "claude-opus-5-5-high", "gemini-3.8-flash-high", // Antigravity
		"deepseek-chat", "deepseek-reasoner", "grok-4.7", "kimi-k3", "glm-5",
	} {
		price, ok := p.price(model)
		if !ok || price.Input <= 0 || price.Output <= 0 || p.contextWindow(model) <= 0 {
			t.Errorf("%s: price %+v ok=%v context=%d", model, price, ok, p.contextWindow(model))
		}
	}
	if src := p.priceSource("claude-opus-5-5"); src != claudeSource {
		t.Errorf("built-in Claude prices must win over LiteLLM, got %q", src)
	}
	if src := p.priceSource("gpt-6-sol"); src != "litellm: openai" {
		t.Errorf("gpt-6-sol source %q", src)
	}
	// Gemini 2.5 Pro charges more above 200k prompt tokens.
	short := p.cost("gemini-2.5-pro", "gemini", "", spanTokens{totalInput: 1000, uncached: 1000})
	long := p.cost("gemini-2.5-pro", "gemini", "", spanTokens{totalInput: 300_000, uncached: 300_000})
	if !(long.cost/300 > short.cost*1.5) {
		t.Errorf("long-context tier not applied: short=%v long=%v", short.cost, long.cost)
	}
	// Config wins over both tables.
	o := newPricer(map[string]modelPrice{"gpt-6-sol": {Input: 1, Output: 1}}, nil)
	if src := o.priceSource("gpt-6-sol-2026-09-01"); src != "config" {
		t.Errorf("config override source %q", src)
	}
}
