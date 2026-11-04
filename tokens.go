package main

import "strings"

// tokenSemantics describes how a provider's usage buckets overlap. It mirrors
// tokenAccountingSemanticsFor in CLIProxyAPI (sdk/cliproxy/usage/accounting.go),
// which the host uses to build its own token breakdown; the plugin ABI only
// carries the raw UsageDetail counters, so the plugin re-derives it.
type tokenSemantics uint8

const (
	// semanticsUnknown: unknown provider; the prompt size is a lower bound.
	semanticsUnknown tokenSemantics = iota
	// semanticsSubset: cache reads/writes are included in InputTokens
	// (OpenAI chat/responses, Codex, OpenAI-compatible, xAI, ...).
	semanticsSubset
	// semanticsIndependent: InputTokens is the uncached remainder; cache reads
	// and writes are separate buckets (Anthropic Messages API).
	semanticsIndependent
	// semanticsSeparateReasoning: cache reads are included in InputTokens,
	// reasoning is separate from output (Gemini family).
	semanticsSeparateReasoning
)

func tokenSemanticsFor(provider, executorType string) tokenSemantics {
	p := strings.ToLower(strings.TrimSpace(provider))
	e := strings.ToLower(strings.TrimSpace(executorType))
	value := strings.TrimSpace(p + " " + e)
	if value == "" || value == "unknown" || value == "unknown unknown" {
		return semanticsUnknown
	}
	if e == "openaicompatexecutor" || p == "openai-compatibility" || strings.HasPrefix(p, "openai-compatible-") {
		return semanticsSubset
	}
	if strings.Contains(value, "claude") || strings.Contains(value, "anthropic") {
		return semanticsIndependent
	}
	for _, marker := range []string{"gemini", "aistudio", "antigravity", "vertex", "interaction"} {
		if strings.Contains(value, marker) {
			return semanticsSeparateReasoning
		}
	}
	for _, marker := range []string{"openai", "codex", "xai", "grok", "kimi", "qwen", "deepseek", "openrouter"} {
		if strings.Contains(value, marker) {
			return semanticsSubset
		}
	}
	return semanticsUnknown
}

// inputTokens is the normalized, non-overlapping input of one attempt:
// total = uncached + cacheRead + cacheWrite.
type inputTokens struct {
	total, uncached, cacheRead, cacheWrite int64
}

// normalizeInput derives the true prompt size from a usage record.
//
//   - Claude (independent): InputTokens excludes the cache, so
//     total = InputTokens + CacheReadTokens + CacheCreationTokens.
//   - OpenAI-style and Gemini (subset): InputTokens already contains cached
//     tokens, so total = InputTokens (never double counted).
//   - Unknown providers: the largest of InputTokens and the cache buckets,
//     the same lower bound the host uses.
//
// CachedTokens is only used as the cache-read count when neither cache bucket
// is set (some executors fill only CachedTokens).
func normalizeInput(d usageDetail, provider, executorType string) inputTokens {
	in := max(d.InputTokens, 0)
	read, write := max(d.CacheReadTokens, 0), max(d.CacheCreationTokens, 0)
	if read == 0 && write == 0 {
		read = max(d.CachedTokens, 0)
	}
	if tokenSemanticsFor(provider, executorType) == semanticsIndependent {
		return inputTokens{total: in + read + write, uncached: in, cacheRead: read, cacheWrite: write}
	}
	total := max(in, read+write)
	return inputTokens{total: total, uncached: total - read - write, cacheRead: read, cacheWrite: write}
}

// cacheHitRate is cache reads over the whole prompt, rounded to 4 decimals.
func cacheHitRate(cacheRead, totalInput int64) float64 {
	if totalInput <= 0 || cacheRead <= 0 {
		return 0
	}
	return roundTo(float64(min(cacheRead, totalInput))/float64(totalInput), 1e4)
}
