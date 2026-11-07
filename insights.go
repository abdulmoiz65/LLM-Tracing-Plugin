package main

import (
	"sort"
	"time"
)

// Insights turn the stored history into findings with a dollar figure. They
// are computed on request in one pass over the history (at most
// history_max_requests traces) and keep no state of their own. Every figure
// is an estimate at API list prices; the wording on the page says so.

const (
	// minBreakTokens: smaller cache writes are normal growth, not a break.
	minBreakTokens = 10_000
	// breakShareOfPrompt: a break rewrites at least this share of the prompt.
	breakShareOfPrompt = 0.5
	// shortCacheTTL: a rewrite sooner than this after the previous request
	// cannot be an expiry, so the cached prefix must have changed.
	shortCacheTTL = 5 * time.Minute
	// largeContextTokens: history above this is what compaction could save.
	largeContextTokens = 200_000
	// maxInsightItems bounds every list in the response.
	maxInsightItems = 10
)

type cacheBreakJSON struct {
	TraceID     string  `json:"trace_id"`
	SessionID   string  `json:"session_id"`
	At          string  `json:"at"`
	Model       string  `json:"model"`
	Account     string  `json:"account"`
	WriteTokens int64   `json:"write_tokens"`
	GapSeconds  float64 `json:"gap_seconds"`
	// Kind is "account_changed" (served by a different credential than the
	// previous request: prompt caches are per account), "prefix_changed"
	// (same account, within 5 minutes, so not an expiry) or
	// "expired_or_changed" (same account, after an idle gap; cause unknown).
	Kind     string  `json:"kind"`
	ExtraUSD float64 `json:"extra_usd"`
}

type cacheBreaksJSON struct {
	AccountChanged      int              `json:"account_changed"`
	AccountChangedUSD   float64          `json:"account_changed_usd"`
	PrefixChanged       int              `json:"prefix_changed"`
	PrefixChangedUSD    float64          `json:"prefix_changed_usd"`
	ExpiredOrChanged    int              `json:"expired_or_changed"`
	ExpiredOrChangedUSD float64          `json:"expired_or_changed_usd"`
	Top                 []cacheBreakJSON `json:"top"`
}

type contextSessionJSON struct {
	SessionID     string  `json:"session_id"`
	Model         string  `json:"model"`
	Account       string  `json:"account"`
	Requests      int     `json:"requests"`
	PeakTokens    int64   `json:"peak_tokens"`
	AboveUSD      float64 `json:"above_threshold_usd"`
	LastRequestAt string  `json:"last_at"`
}

type contextJSON struct {
	ThresholdTokens int64                `json:"threshold_tokens"`
	Sessions        int                  `json:"sessions"`
	Requests        int                  `json:"requests"`
	AboveUSD        float64              `json:"above_threshold_usd"`
	Top             []contextSessionJSON `json:"top"`
}

type accountHealthJSON struct {
	Account  string  `json:"account"`
	Attempts int     `json:"attempts"`
	Failed   int     `json:"failed"`
	FailRate float64 `json:"fail_rate"`
}

type retriesJSON struct {
	RetriedRequests int     `json:"retried_requests"`
	FailedAttempts  int     `json:"failed_attempts"`
	AddedWaitMS     float64 `json:"added_wait_ms"`
	// Accounts covers the requests that still keep attempt details (the
	// newest ui_max_traces); SampledRequests says how many that was.
	SampledRequests int                 `json:"sampled_requests"`
	Accounts        []accountHealthJSON `json:"accounts"`
}

type insightsJSON struct {
	Range           string          `json:"range"`
	Requests        int             `json:"requests"`
	CostUSD         float64         `json:"cost_usd"`
	CacheSavingsUSD float64         `json:"cache_savings_usd"`
	CacheBreaks     cacheBreaksJSON `json:"cache_breaks"`
	Context         contextJSON     `json:"context"`
	Retries         retriesJSON     `json:"retries"`
}

// cacheRates returns the per-token write and read price a trace's model
// paid, choosing the write TTL like cost() does.
func (s *traceStore) cacheRates(t *uiTrace) (write, read float64, ok bool) {
	model := nonEmpty(t.servingModel, t.model)
	price, priced := s.models.price(model)
	if !priced {
		return 0, 0, false
	}
	write, read = price.CacheWrite, price.CacheRead
	if s.models.writeIs1h(t.provider, t.authType) {
		switch {
		case price.CacheWrite1h > 0:
			write = price.CacheWrite1h
		case tokenSemanticsFor(t.provider, "") == semanticsIndependent:
			write = price.Input * 2
		}
	}
	return write / 1e6, read / 1e6, true
}

func (s *traceStore) insights(since int64, label string, labels map[string]string) insightsJSON {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := insightsJSON{Range: label, Context: contextJSON{ThresholdTokens: largeContextTokens}}
	inRange := func(t *uiTrace) bool { return since <= 0 || t.start >= since }

	// Totals and retries: one pass over the history.
	var noCache float64
	health := map[string]*accountHealthJSON{}
	for _, t := range s.buf {
		if !inRange(t) {
			continue
		}
		out.Requests++
		c := s.costLocked(t)
		out.CostUSD += c.cost
		noCache += c.noCache
		if t.attempts > 1 {
			out.Retries.RetriedRequests++
			out.Retries.FailedAttempts += t.failed
			if t.hasServing && !t.servingFailed && t.servingStart > t.start {
				out.Retries.AddedWaitMS += nanosToMS(t.servingStart - t.start)
			}
		}
		if !t.hasSpans {
			continue
		}
		out.Retries.SampledRequests++
		for i := range t.spans {
			sp := &t.spans[i]
			if sp.kind != spanKindClient {
				continue
			}
			name := accountLabel(labels, sp.str("cpa.auth_index"), sp.str("cpa.auth_type"))
			h := health[name]
			if h == nil {
				if len(health) >= 4*maxInsightItems {
					continue
				}
				h = &accountHealthJSON{Account: name}
				health[name] = h
			}
			h.Attempts++
			if sp.failed {
				h.Failed++
			}
		}
	}
	out.CacheSavingsUSD = roundUSD(max(noCache-out.CostUSD, 0))
	out.CostUSD = roundUSD(out.CostUSD)
	out.Retries.AddedWaitMS = roundTo(out.Retries.AddedWaitMS, 1e3)
	for _, h := range health {
		h.FailRate = roundTo(float64(h.Failed)/float64(max(h.Attempts, 1)), 1e4)
		out.Retries.Accounts = append(out.Retries.Accounts, *h)
	}
	sort.Slice(out.Retries.Accounts, func(i, j int) bool {
		a, b := out.Retries.Accounts[i], out.Retries.Accounts[j]
		if a.Failed != b.Failed {
			return a.Failed > b.Failed
		}
		return a.Account < b.Account
	})
	if len(out.Retries.Accounts) > maxInsightItems {
		out.Retries.Accounts = out.Retries.Accounts[:maxInsightItems]
	}

	// Cache breaks and large context: per session, in request order.
	breaks := make([]cacheBreakJSON, 0, maxInsightItems)
	var contexts []contextSessionJSON
	for _, sess := range s.sessionsLocked().sessions {
		var prev *uiTrace
		ctx := contextSessionJSON{SessionID: sess.id, Model: sess.model}
		for _, t := range sess.traces {
			if !inRange(t) {
				prev = t
				continue
			}
			write, read, priced := s.cacheRates(t)
			// A break: a large rewrite of most of the prompt after an
			// earlier request of the same conversation.
			if prev != nil && priced && t.cacheWrite >= minBreakTokens &&
				float64(t.cacheWrite) >= breakShareOfPrompt*float64(max(t.totalInput, 1)) {
				gap := time.Duration(max(t.start-prev.end, 0))
				extra := float64(t.cacheWrite) * max(write-read, 0)
				b := cacheBreakJSON{
					TraceID: t.tid.String(), SessionID: sess.id,
					At:    time.Unix(0, t.start).UTC().Format(time.RFC3339Nano),
					Model: nonEmpty(t.servingModel, t.model), Account: accountLabel(labels, t.authIndex, t.authType),
					WriteTokens: t.cacheWrite, GapSeconds: roundTo(gap.Seconds(), 10), ExtraUSD: roundUSD(extra),
				}
				switch {
				case prev.authIndex != "" && t.authIndex != "" && prev.authIndex != t.authIndex:
					b.Kind = "account_changed"
					out.CacheBreaks.AccountChanged++
					out.CacheBreaks.AccountChangedUSD += extra
				case gap <= shortCacheTTL:
					b.Kind = "prefix_changed"
					out.CacheBreaks.PrefixChanged++
					out.CacheBreaks.PrefixChangedUSD += extra
				default:
					b.Kind = "expired_or_changed"
					out.CacheBreaks.ExpiredOrChanged++
					out.CacheBreaks.ExpiredOrChangedUSD += extra
				}
				breaks = keepTop(breaks, b, func(a, c cacheBreakJSON) bool { return a.ExtraUSD > c.ExtraUSD })
			}
			// Large context: what re-reading history above the threshold
			// cost (as cache reads; uncached it would cost more).
			if t.servingPrompt > largeContextTokens && priced {
				above := float64(t.servingPrompt-largeContextTokens) * read
				ctx.AboveUSD += above
				ctx.Requests++
				out.Context.Requests++
				out.Context.AboveUSD += above
				ctx.PeakTokens = max(ctx.PeakTokens, t.servingPrompt)
				ctx.Account = accountLabel(labels, t.authIndex, t.authType)
				ctx.LastRequestAt = time.Unix(0, t.start).UTC().Format(time.RFC3339Nano)
			}
			prev = t
		}
		if ctx.Requests > 0 {
			out.Context.Sessions++
			ctx.AboveUSD = roundUSD(ctx.AboveUSD)
			contexts = keepTop(contexts, ctx, func(a, c contextSessionJSON) bool { return a.AboveUSD > c.AboveUSD })
		}
	}
	out.CacheBreaks.Top = breaks
	out.CacheBreaks.AccountChangedUSD = roundUSD(out.CacheBreaks.AccountChangedUSD)
	out.CacheBreaks.PrefixChangedUSD = roundUSD(out.CacheBreaks.PrefixChangedUSD)
	out.CacheBreaks.ExpiredOrChangedUSD = roundUSD(out.CacheBreaks.ExpiredOrChangedUSD)
	out.Context.AboveUSD = roundUSD(out.Context.AboveUSD)
	out.Context.Top = contexts
	if out.Context.Top == nil {
		out.Context.Top = []contextSessionJSON{}
	}
	if out.Retries.Accounts == nil {
		out.Retries.Accounts = []accountHealthJSON{}
	}
	return out
}

// keepTop inserts v into list (sorted by better, at most maxInsightItems).
func keepTop[T any](list []T, v T, better func(a, b T) bool) []T {
	i := sort.Search(len(list), func(i int) bool { return better(v, list[i]) })
	if i >= maxInsightItems {
		return list
	}
	list = append(list, v)
	copy(list[i+1:], list[i:])
	list[i] = v
	if len(list) > maxInsightItems {
		list = list[:maxInsightItems]
	}
	return list
}

type whatIfJSON struct {
	Model          string  `json:"model"`
	PriceSource    string  `json:"price_source"`
	Requests       int     `json:"requests"`
	CurrentUSD     float64 `json:"current_usd"`
	WhatIfUSD      float64 `json:"what_if_usd"`
	DifferenceUSD  float64 `json:"difference_usd"`
	SkippedNoPrice int     `json:"skipped_no_price"`
}

// whatIf re-prices the recorded tokens of priced attempts in range on
// another model. Same token counts are assumed: tokenizers differ between
// model families, and switching models mid-session also loses the cache.
func (s *traceStore) whatIf(since int64, model string) (whatIfJSON, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.models.priceSource(model)
	if src == "" {
		return whatIfJSON{}, false
	}
	out := whatIfJSON{Model: model, PriceSource: src}
	for _, t := range s.buf {
		if since > 0 && t.start < since {
			continue
		}
		counted := false
		for i := range t.usage {
			u := &t.usage[i]
			cur := s.models.cost(u.model, u.provider, u.authType, u.tok)
			if u.frozen {
				cur = attemptCost{cost: u.cost, ok: true}
			}
			if !cur.ok {
				out.SkippedNoPrice++
				continue
			}
			alt := s.models.cost(model, u.provider, u.authType, u.tok)
			out.CurrentUSD += cur.cost
			out.WhatIfUSD += alt.cost
			counted = true
		}
		if counted {
			out.Requests++
		}
	}
	out.DifferenceUSD = roundUSD(out.WhatIfUSD - out.CurrentUSD)
	out.CurrentUSD, out.WhatIfUSD = roundUSD(out.CurrentUSD), roundUSD(out.WhatIfUSD)
	return out, true
}
