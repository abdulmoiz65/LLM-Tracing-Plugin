// Command genprices builds prices_generated.json, the plugin's built-in
// price and context-window table, from LiteLLM's
// model_prices_and_context_window.json (MIT licensed, see
// THIRD_PARTY_NOTICES.md).
//
//	go run ./tools/genprices -in model_prices_and_context_window.json \
//	    -commit <litellm commit sha> -date <commit date> -out prices_generated.json
//
// The input is untrusted: every name and number is validated, and entries
// that fail are skipped.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// providerRank orders LiteLLM providers: first-party APIs first, then the
// big clouds, then everything else (resellers, whose prices may include a
// markup). Lower wins.
var providerRank = map[string]int{
	"openai": 0, "anthropic": 0, "gemini": 0, "deepseek": 0, "xai": 0, "moonshot": 0,
	"dashscope": 0, "mistral": 0, "zai": 0, "minimax": 0, "meta": 0, "meta_llama": 0,
	"cohere_chat": 0, "chatgpt": 0, "text-completion-openai": 0,
	"vertex_ai-language-models": 1, "vertex_ai": 1, "vertex_ai-anthropic_models": 1,
	"bedrock": 2, "bedrock_converse": 2, "azure": 2, "azure_ai": 2,
}

const otherRank = 9

var (
	nameRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._:@+-]{0,199}$`)
	longInputRe = regexp.MustCompile(`^input_cost_per_token_above_(\d+)k_tokens$`)
)

type entry struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h float64
	Context                                            int64
	LongThreshold                                      int64
	LongInput, LongOutput, LongCacheRead               float64
	Source                                             string
	rank                                               int
	canonical                                          bool // LiteLLM key without a provider prefix
}

type output struct {
	Source  string `json:"source"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	License string `json:"license"`
	// Models maps a normalized model name to
	// [input, output, cache_read, cache_write, cache_write_1h, context,
	//  long_threshold, long_input, long_output, long_cache_read, source]
	// with prices in USD per million tokens.
	Models map[string][]any `json:"models"`
}

func perMillion(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	f *= 1e6
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 10_000 {
		return 0, false
	}
	return math.Round(f*1e6) / 1e6, true
}

func main() {
	in := flag.String("in", "", "LiteLLM model_prices_and_context_window.json")
	out := flag.String("out", "prices_generated.json", "output file")
	commit := flag.String("commit", "", "LiteLLM commit the input came from")
	date := flag.String("date", "", "commit date (RFC 3339)")
	flag.Parse()
	if *in == "" || *commit == "" || *date == "" {
		fmt.Fprintln(os.Stderr, "usage: genprices -in <file> -commit <sha> -date <date> [-out prices_generated.json]")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var src map[string]map[string]any
	if err = json.Unmarshal(raw, &src); err != nil {
		fmt.Fprintln(os.Stderr, "decode:", err)
		os.Exit(1)
	}

	best := map[string]entry{}
	for key, v := range src {
		mode, _ := v["mode"].(string)
		if mode != "chat" && mode != "responses" && mode != "completion" {
			continue
		}
		input, okIn := perMillion(v["input_cost_per_token"])
		outputRate, okOut := perMillion(v["output_cost_per_token"])
		if !okIn || !okOut || (input == 0 && outputRate == 0) {
			continue
		}
		name := strings.ToLower(key[strings.LastIndex(key, "/")+1:])
		if !nameRe.MatchString(name) {
			continue
		}
		provider, _ := v["litellm_provider"].(string)
		rank, known := providerRank[provider]
		if !known {
			rank = otherRank
		}
		e := entry{Input: input, Output: outputRate, Source: provider, rank: rank, canonical: !strings.Contains(key, "/")}
		// A missing or zero cache price means "not discounted": cached
		// tokens are then priced as plain input.
		if cr, ok := perMillion(v["cache_read_input_token_cost"]); ok && cr > 0 {
			e.CacheRead = cr
		} else {
			e.CacheRead = input
		}
		if cw, ok := perMillion(v["cache_creation_input_token_cost"]); ok && cw > 0 {
			e.CacheWrite = cw
		} else {
			e.CacheWrite = input
		}
		if cw1h, ok := perMillion(v["cache_creation_input_token_cost_above_1hr"]); ok && cw1h > 0 {
			e.CacheWrite1h = cw1h
		}
		if ctx, ok := v["max_input_tokens"].(float64); ok && ctx >= 1000 && ctx <= 100_000_000 {
			e.Context = int64(ctx)
		}
		// Long-context tier: the lowest "above_<N>k_tokens" threshold.
		for field := range v {
			m := longInputRe.FindStringSubmatch(field)
			if m == nil {
				continue
			}
			k, errAtoi := strconv.Atoi(m[1])
			if errAtoi != nil || k <= 0 || k > 100_000 {
				continue
			}
			threshold := int64(k) * 1000
			if e.LongThreshold != 0 && threshold >= e.LongThreshold {
				continue
			}
			li, okLI := perMillion(v[field])
			lo, okLO := perMillion(v[fmt.Sprintf("output_cost_per_token_above_%dk_tokens", k)])
			if !okLI || !okLO || li == 0 {
				continue
			}
			lcr, _ := perMillion(v[fmt.Sprintf("cache_read_input_token_cost_above_%dk_tokens", k)])
			e.LongThreshold, e.LongInput, e.LongOutput, e.LongCacheRead = threshold, li, lo, lcr
		}
		if cur, ok := best[name]; !ok || better(e, cur) {
			best[name] = e
		}
	}
	// "x-preview" from a first-party provider or Vertex also prices "x"
	// when "x" only has a lower-ranked entry (Vertex and Antigravity often
	// drop the suffix).
	for name, e := range best {
		base, ok := strings.CutSuffix(name, "-preview")
		if !ok || e.rank > 1 {
			continue
		}
		if cur, exists := best[base]; !exists || cur.rank > e.rank {
			alias := e
			alias.Source = e.Source + " (" + name + ")"
			best[base] = alias
		}
	}

	res := output{
		Source:  "https://github.com/BerriAI/litellm/blob/" + *commit + "/model_prices_and_context_window.json",
		Commit:  *commit,
		Date:    *date,
		License: "MIT (LiteLLM, Copyright (c) 2023 Berri AI)",
		Models:  make(map[string][]any, len(best)),
	}
	names := make([]string, 0, len(best))
	for name := range best {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := best[name]
		res.Models[name] = []any{e.Input, e.Output, e.CacheRead, e.CacheWrite, e.CacheWrite1h, e.Context,
			e.LongThreshold, e.LongInput, e.LongOutput, e.LongCacheRead, e.Source}
	}
	body, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = os.WriteFile(*out, append(body, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d models to %s\n", len(res.Models), *out)
}

// better prefers a lower provider rank, then an entry with a context
// window, then the lower input price (deterministic tie-break).
func better(a, b entry) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	if a.canonical != b.canonical {
		return a.canonical
	}
	if (a.Context > 0) != (b.Context > 0) {
		return a.Context > 0
	}
	if a.Input != b.Input {
		return a.Input < b.Input
	}
	return a.Source < b.Source
}
