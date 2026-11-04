package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The usage details below mirror how CLIProxyAPI fills pluginapi.UsageDetail
// (internal/runtime/executor/helps/usage_helpers.go):
//   - parseClaudeUsageNode: InputTokens = input_tokens (uncached only),
//     CacheReadTokens = cache_read_input_tokens, CacheCreationTokens =
//     cache_creation_input_tokens, CachedTokens = cache read (or creation when
//     there is no read), TotalTokens = input + output + read + creation.
//   - parseOpenAIStyleUsageNode: InputTokens = prompt_tokens (includes
//     cached), CachedTokens = CacheReadTokens = prompt_tokens_details.cached_tokens.
//   - parseGeminiFamilyUsageDetail: InputTokens = promptTokenCount (includes
//     cached), CachedTokens = CacheReadTokens = cachedContentTokenCount.

var claudeDetail = usageDetail{InputTokens: 145, OutputTokens: 300, CachedTokens: 12288, CacheReadTokens: 12288, CacheCreationTokens: 1024, TotalTokens: 145 + 300 + 12288 + 1024}

func TestNormalizeInputPerProvider(t *testing.T) {
	cases := []struct {
		name, provider, executor string
		d                        usageDetail
		want                     inputTokens
	}{
		{"claude excludes cache", "claude", "ClaudeExecutor", claudeDetail,
			inputTokens{total: 13457, uncached: 145, cacheRead: 12288, cacheWrite: 1024}},
		{"claude write only", "claude", "", usageDetail{InputTokens: 10, CachedTokens: 500, CacheCreationTokens: 500},
			inputTokens{total: 510, uncached: 10, cacheWrite: 500}},
		{"openai-compatible includes cached", "openai-compatible-mock", "OpenAICompatExecutor", usageDetail{InputTokens: 42, OutputTokens: 7, CachedTokens: 30, CacheReadTokens: 30, TotalTokens: 49},
			inputTokens{total: 42, uncached: 12, cacheRead: 30}},
		{"codex includes cached", "codex", "CodexExecutor", usageDetail{InputTokens: 1000, CachedTokens: 800, CacheReadTokens: 800},
			inputTokens{total: 1000, uncached: 200, cacheRead: 800}},
		{"gemini includes cached", "gemini", "GeminiExecutor", usageDetail{InputTokens: 100, CachedTokens: 40, CacheReadTokens: 40},
			inputTokens{total: 100, uncached: 60, cacheRead: 40}},
		{"cached only falls back to cache read", "openai", "", usageDetail{InputTokens: 50, CachedTokens: 20},
			inputTokens{total: 50, uncached: 30, cacheRead: 20}},
		{"unknown provider lower bound", "", "", usageDetail{InputTokens: 5, CacheReadTokens: 9},
			inputTokens{total: 9, uncached: 0, cacheRead: 9}},
		{"no usage", "claude", "", usageDetail{}, inputTokens{}},
	}
	for _, c := range cases {
		if got := normalizeInput(c.d, c.provider, c.executor); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	if tokenSemanticsFor("anthropic", "") != semanticsIndependent || tokenSemanticsFor("openai-compatible-claude", "") != semanticsSubset ||
		tokenSemanticsFor("vertex", "") != semanticsSeparateReasoning || tokenSemanticsFor("unknown", "") != semanticsUnknown {
		t.Fatal("semantics table differs from CPA's tokenAccountingSemanticsFor")
	}
}

func TestCacheHitRate(t *testing.T) {
	if cacheHitRate(0, 0) != 0 || cacheHitRate(5, 0) != 0 || cacheHitRate(0, 100) != 0 {
		t.Fatal("zero cases")
	}
	if got := cacheHitRate(12288, 13457); got != 0.9131 {
		t.Fatalf("rounded rate: %v", got)
	}
	if cacheHitRate(200, 100) != 1 {
		t.Fatal("rate must be capped at 1")
	}
}

// A Claude attempt (cache excluded from input) followed by an OpenAI-style
// attempt (cache included) must sum without double counting, in the API, the
// summary and the exported spans.
func TestTotalInputTokensThroughStoreAndExport(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	tr.addCompletion(completionAt("tok", baseTime, baseTime.Add(3*time.Second), outcomeSucceeded, 200))
	claude := usageAt("tok", "a1", baseTime.Add(100*time.Millisecond), time.Second)
	claude.Failed, claude.Failure = true, usageFailure{StatusCode: 429}
	claude.Detail = claudeDetail
	openai := usageAt("tok", "a2", baseTime.Add(1500*time.Millisecond), time.Second)
	openai.Provider, openai.ExecutorType = "openai-compatible-mock", "OpenAICompatExecutor"
	openai.Detail = usageDetail{InputTokens: 42, OutputTokens: 7, CachedTokens: 30, CacheReadTokens: 30, TotalTokens: 49}
	tr.addUsage(claude)
	tr.addUsage(openai)
	finalize(tr, clock)

	item := tr.store.list(traceFilter{limit: 10, status: "all"}, nil)[0]
	if item.InputTokens != 145+42 || item.TotalInputTokens != 13457+42 || item.CacheReadTokens != 12288+30 ||
		item.CacheCreationTokens != 1024 || item.OutputTokens != 307 {
		t.Fatalf("item tokens: %+v", item)
	}
	if item.CacheHitRate != cacheHitRate(12318, 13499) || item.Provider != "openai-compatible-mock" {
		t.Fatalf("hit rate / serving provider: %+v", item)
	}
	s := tr.summary()
	if s.TotalInputTokens != 13499 || s.InputTokens != 187 || s.CacheReadTokens != 12318 || s.CacheCreationTokens != 1024 || s.CacheHitRate != item.CacheHitRate {
		t.Fatalf("summary tokens: %+v", s.summaryStats)
	}
	if strings.Join(s.Providers, ",") != "openai-compatible-mock" {
		t.Fatalf("providers: %v", s.Providers)
	}

	detail, _ := tr.store.get(mustTraceID(t, item.TraceID), nil)
	var attempts []spanJSON
	for _, sp := range detail.Spans {
		if sp.Kind == "client" {
			attempts = append(attempts, sp)
		} else if sp.Tokens != nil || sp.Provider != "" {
			t.Fatalf("root span must not carry attempt fields: %+v", sp)
		}
	}
	if len(attempts) != 2 || attempts[0].Tokens == nil || attempts[1].Tokens == nil {
		t.Fatalf("attempts: %+v", attempts)
	}
	want0 := spanTokensJSON{InputTokens: 145, TotalInputTokens: 13457, UncachedInputTokens: 145, CacheReadTokens: 12288, CacheCreationTokens: 1024, CacheHitRate: 0.9131, OutputTokens: 300}
	want1 := spanTokensJSON{InputTokens: 42, TotalInputTokens: 42, UncachedInputTokens: 12, CacheReadTokens: 30, CacheHitRate: 0.7143, OutputTokens: 7}
	if *attempts[0].Tokens != want0 || *attempts[1].Tokens != want1 {
		t.Fatalf("per-attempt tokens:\n%+v\n%+v", *attempts[0].Tokens, *attempts[1].Tokens)
	}
	if attempts[0].Provider != "claude" || attempts[1].Provider != "openai-compatible-mock" {
		t.Fatalf("per-attempt provider: %q %q", attempts[0].Provider, attempts[1].Provider)
	}

	roots, children := rootsAndChildren(exportNow(t, tr, c))
	if len(roots) != 1 || len(children) != 2 {
		t.Fatalf("exported %d roots %d children", len(roots), len(children))
	}
	if intAttr(t, roots[0], "cpa.usage.total_input_tokens") != "13499" || intAttr(t, roots[0], "cpa.usage.input_tokens") != "187" {
		t.Fatal("root token totals")
	}
	for _, ch := range children {
		total, input := intAttr(t, ch, "cpa.usage.total_input_tokens"), intAttr(t, ch, "gen_ai.usage.input_tokens")
		// gen_ai.usage.input_tokens keeps the provider-reported value.
		if !(total == "13457" && input == "145") && !(total == "42" && input == "42") {
			t.Fatalf("attempt tokens total=%s input=%s", total, input)
		}
		if hasAttr(ch, "gen_ai.usage.cache_read.input_tokens") {
			t.Fatal("gen_ai cache attributes are not emitted (Langfuse maps gen_ai.usage.* as usage)")
		}
	}
	raw, _ := json.Marshal(detail)
	if !strings.Contains(string(raw), `"total_input_tokens":13457`) || !strings.Contains(string(raw), `"uncached_input_tokens":145`) {
		t.Fatalf("detail JSON: %s", raw)
	}
}
