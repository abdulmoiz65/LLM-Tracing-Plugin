package main

import (
	_ "embed"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"unicode"
)

// modelPrice is a USD price per one million tokens. CacheWrite is the
// standard (for Claude: 5-minute) cache write rate and CacheWrite1h the
// 1-hour rate (0: unknown). When LongThreshold > 0 and a request's prompt
// exceeds it, input uses LongInput, output LongOutput and cache reads
// LongCacheRead (0: scaled like input); cache writes scale like input.
type modelPrice struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h float64
	LongThreshold                                      int64
	LongInput, LongOutput, LongCacheRead               float64
	// Source says where the price came from (config, built-in, or the
	// LiteLLM provider entry).
	Source string
}

// claudePricesAsOf dates the built-in Claude table (Anthropic first-party
// API list prices). It wins over the generated LiteLLM table.
const claudePricesAsOf = "2026-10-06"

const claudeSource = "anthropic (built-in " + claudePricesAsOf + ")"

func claude(input, output, cacheRead float64) modelPrice {
	return modelPrice{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: input * 1.25, CacheWrite1h: input * 2, Source: claudeSource}
}

var builtinPrices = map[string]modelPrice{
	"claude-fable-5-1":  claude(10, 50, 0.25),
	"claude-mythos-5-1": claude(10, 50, 0.25),
	"claude-fable-5":    claude(10, 50, 1),
	"claude-mythos-5":   claude(10, 50, 1),
	"claude-opus-5-5":   claude(4, 20, 0.20),
	"claude-opus-5":     claude(5, 25, 0.50),
	"claude-opus-4-8":   claude(5, 25, 0.50),
	"claude-opus-4-7":   claude(5, 25, 0.50),
	"claude-opus-4-6":   claude(5, 25, 0.50),
	"claude-sonnet-5-5": claude(2, 10, 0.20),
	"claude-sonnet-5":   claude(2, 10, 0.20),
	"claude-sonnet-4-6": claude(3, 15, 0.30),
	"claude-haiku-5-5": func() modelPrice {
		p := claude(0.10, 0.50, 0.01)
		p.LongThreshold, p.LongInput, p.LongOutput = 100_000, 0.50, 2.50
		return p
	}(),
	"claude-haiku-4-5": claude(1, 5, 0.10),
}

// builtinContextWindows are maximum input sizes in tokens.
var builtinContextWindows = map[string]int64{
	"claude-fable-5-1":  1_000_000,
	"claude-mythos-5-1": 1_000_000,
	"claude-fable-5":    1_000_000,
	"claude-mythos-5":   1_000_000,
	"claude-opus-5-5":   1_000_000,
	"claude-opus-5":     1_000_000,
	"claude-opus-4-8":   1_000_000,
	"claude-opus-4-7":   1_000_000,
	"claude-opus-4-6":   1_000_000,
	"claude-sonnet-5-5": 1_000_000,
	"claude-sonnet-5":   1_000_000,
	"claude-sonnet-4-6": 1_000_000,
	"claude-haiku-5-5":  1_000_000,
	"claude-haiku-4-5":  200_000,
}

// prices_generated.json is built by tools/genprices from LiteLLM's price
// list (MIT). It covers OpenAI/Codex, Gemini, DeepSeek, xAI, Moonshot,
// Mistral, Z.ai, MiniMax and many more.
//
//go:embed prices_generated.json
var generatedPricesJSON []byte

type generatedMeta struct {
	Commit, Date string
	Models       int
}

type priceTable struct {
	prices   map[string]modelPrice
	contexts map[string]int64
}

var loadGenerated = sync.OnceValues(func() (*priceTable, generatedMeta) {
	var raw struct {
		Commit string           `json:"commit"`
		Date   string           `json:"date"`
		Models map[string][]any `json:"models"`
	}
	table := &priceTable{prices: map[string]modelPrice{}, contexts: map[string]int64{}}
	if err := json.Unmarshal(generatedPricesJSON, &raw); err != nil {
		return table, generatedMeta{}
	}
	num := func(v any) float64 {
		f, _ := v.(float64)
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return 0
		}
		return f
	}
	for name, row := range raw.Models {
		if len(row) != 11 || name == "" {
			continue
		}
		source, _ := row[10].(string)
		p := modelPrice{
			Input: num(row[0]), Output: num(row[1]), CacheRead: num(row[2]), CacheWrite: num(row[3]), CacheWrite1h: num(row[4]),
			LongThreshold: int64(num(row[6])), LongInput: num(row[7]), LongOutput: num(row[8]), LongCacheRead: num(row[9]),
			Source: "litellm: " + source,
		}
		table.prices[name] = p
		if ctx := int64(num(row[5])); ctx > 0 {
			table.contexts[name] = ctx
		}
	}
	return table, generatedMeta{Commit: raw.Commit, Date: raw.Date, Models: len(table.prices)}
})

// pricesAsOfText dates both price sources for the page.
func pricesAsOfText() string {
	_, meta := loadGenerated()
	date := meta.Date
	if len(date) > 10 {
		date = date[:10]
	}
	return "Claude " + claudePricesAsOf + ", others LiteLLM " + date
}

// priceTableInfo describes the price sources for the summary.
func priceTableInfo() map[string]any {
	_, meta := loadGenerated()
	commit := meta.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return map[string]any{
		"claude_built_in_as_of": claudePricesAsOf,
		"litellm_date":          meta.Date,
		"litellm_commit":        commit,
		"litellm_models":        meta.Models,
	}
}

// pricer resolves model names to prices and context windows from three
// layers: config (highest), built-in Claude, generated LiteLLM. The longest
// matching name wins; on equal length the higher layer wins. Results are
// cached per model name.
type pricer struct {
	layers []*priceTable
	// cacheTTL picks the Claude cache write rate: "5m", "1h", or "auto" (1h
	// for Claude OAuth accounts, which CPA sends with the 1-hour cache like
	// Claude Code does; 5m otherwise). Usage records do not carry the TTL.
	cacheTTL string

	mu    sync.Mutex
	cache map[string]resolved
}

type resolved struct {
	price   modelPrice
	priced  bool
	context int64
}

const (
	cacheTTLAuto = "auto"
	cacheTTL5m   = "5m"
	cacheTTL1h   = "1h"
	// maxResolvedCache bounds the per-pricer lookup cache.
	maxResolvedCache = 4096
)

func newPricer(userPrices map[string]modelPrice, userContexts map[string]int64) *pricer {
	user := &priceTable{prices: map[string]modelPrice{}, contexts: map[string]int64{}}
	for k, v := range userPrices {
		if v.Source == "" {
			v.Source = "config"
		}
		user.prices[normalizeModelName(k)] = v
	}
	for k, v := range userContexts {
		user.contexts[normalizeModelName(k)] = v
	}
	generated, _ := loadGenerated()
	builtin := &priceTable{prices: builtinPrices, contexts: builtinContextWindows}
	return &pricer{layers: []*priceTable{user, builtin, generated}, cacheTTL: cacheTTLAuto, cache: map[string]resolved{}}
}

// normalizeModelName lower-cases a model name and strips routing decoration:
// path or platform prefixes ("anthropic/", "us.anthropic."), a trailing
// "(...)" thinking suffix and a "[...]" variant suffix.
func normalizeModelName(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, "claude-"); i > 0 && strings.Contains(m[:i], ".") {
		m = m[i:]
	}
	if i := strings.IndexAny(m, "(["); i > 0 {
		m = m[:i]
	}
	return strings.TrimSpace(m)
}

// matchLen returns how well key matches the normalized model: len(key) for
// an exact match or for a prefix followed by '-', '@' or ':' (dates,
// "-thinking", "-high"), else -1. A dot does not separate: "kimi-k2" must
// not price "kimi-k2.8". Keys without a digit match exactly only, so a
// generic name never prices a whole family.
func matchLen(key, m string) int {
	if key == m {
		return len(key)
	}
	if len(m) <= len(key) || !strings.HasPrefix(m, key) || !strings.ContainsRune("-@:", rune(m[len(key)])) {
		return -1
	}
	if !strings.ContainsFunc(key, unicode.IsDigit) {
		return -1
	}
	return len(key)
}

func bestMatch[V any](layers []map[string]V, m string) (V, bool) {
	var zero V
	bestLen, bestVal, found := -1, zero, false
	for _, layer := range layers {
		if v, ok := layer[m]; ok && len(m) > bestLen {
			bestLen, bestVal, found = len(m), v, true
		}
		for key, v := range layer {
			if n := matchLen(key, m); n > bestLen {
				bestLen, bestVal, found = n, v, true
			}
		}
	}
	return bestVal, found
}

func (p *pricer) resolve(model string) resolved {
	if p == nil {
		return resolved{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.cache[model]; ok {
		return r
	}
	m := normalizeModelName(model)
	var r resolved
	if m != "" {
		prices := make([]map[string]modelPrice, 0, len(p.layers))
		contexts := make([]map[string]int64, 0, len(p.layers))
		for _, layer := range p.layers {
			prices = append(prices, layer.prices)
			contexts = append(contexts, layer.contexts)
		}
		r.price, r.priced = bestMatch(prices, m)
		r.context, _ = bestMatch(contexts, m)
	}
	if len(p.cache) >= maxResolvedCache {
		clear(p.cache)
	}
	p.cache[model] = r
	return r
}

func (p *pricer) price(model string) (modelPrice, bool) {
	r := p.resolve(model)
	return r.price, r.priced
}

// contextWindow returns the model's context window, or 0 when unknown.
func (p *pricer) contextWindow(model string) int64 { return p.resolve(model).context }

// priceSource names where a model's price comes from ("" when unpriced).
func (p *pricer) priceSource(model string) string {
	r := p.resolve(model)
	if !r.priced {
		return ""
	}
	return r.price.Source
}

func (p *pricer) writeIs1h(provider, authType string) bool {
	switch p.cacheTTL {
	case cacheTTL1h:
		return true
	case cacheTTL5m:
		return false
	}
	return tokenSemanticsFor(provider, "") == semanticsIndependent && strings.EqualFold(strings.TrimSpace(authType), "oauth")
}

// attemptCost is the API-list-price cost of one attempt and what the same
// tokens would have cost with no prompt cache (every input token at the
// input rate). ok is false when tokens were used but the model has no price.
type attemptCost struct {
	cost, noCache float64
	ok            bool
}

func (p *pricer) cost(model, provider, authType string, tok spanTokens) attemptCost {
	output := tok.output
	// Gemini reports thinking tokens outside output_tokens; they bill as output.
	if tokenSemanticsFor(provider, "") == semanticsSeparateReasoning {
		output += tok.reasoning
	}
	if tok.totalInput <= 0 && output <= 0 {
		return attemptCost{ok: true}
	}
	price, ok := p.price(model)
	if !ok {
		return attemptCost{}
	}
	in, out, read, write, write1h := price.Input, price.Output, price.CacheRead, price.CacheWrite, price.CacheWrite1h
	if price.LongThreshold > 0 && tok.totalInput > price.LongThreshold && price.LongInput > 0 {
		scale := 1.0
		if price.Input > 0 {
			scale = price.LongInput / price.Input
		}
		in, out = price.LongInput, price.LongOutput
		if price.LongCacheRead > 0 {
			read = price.LongCacheRead
		} else {
			read *= scale
		}
		write *= scale
		write1h *= scale
	}
	if p.writeIs1h(provider, authType) {
		switch {
		case write1h > 0:
			write = write1h
		case tokenSemanticsFor(provider, "") == semanticsIndependent:
			write = in * 2
		}
	}
	cost := float64(tok.uncached)*in + float64(tok.cacheRead)*read + float64(tok.cacheWrite)*write + float64(output)*out
	noCache := float64(tok.totalInput)*in + float64(output)*out
	return attemptCost{cost: cost / 1e6, noCache: noCache / 1e6, ok: true}
}

// roundUSD keeps six decimals: enough for sub-cent requests, stable in JSON.
func roundUSD(v float64) float64 { return math.Round(v*1e6) / 1e6 }
