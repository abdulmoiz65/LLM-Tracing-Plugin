package main

import (
	"sort"
	"strings"
	"time"
)

// Sessions are estimated: CPA does not tell plugins which conversation a
// request belongs to. Within one conversation every request resends the
// whole history, so the prompt only grows. A request therefore continues the
// open session of the same client key and requested model whose latest
// prompt is the largest one not much bigger than its own. A request that is
// much smaller (a new conversation, or a compacted one) or that comes after
// a long pause starts a new session.

// sessionShrinkTolerance lets a prompt shrink a little (edited or trimmed
// history) and still continue its session.
const sessionShrinkTolerance = 0.9

// maxSessionsList bounds the /sessions response.
const maxSessionsList = 200

type uiSession struct {
	id, clientKey, model string
	traces               []*uiTrace // oldest first
	lastPrompt           int64
	lastAt               int64 // latest start or end of a member
}

type sessionMember struct {
	session *uiSession
	seq     int // 1-based position in the session
}

type sessionIndex struct {
	sessions []*uiSession
	member   map[*uiTrace]sessionMember
}

func sessionKey(t *uiTrace) string {
	return t.clientKey + "\x00" + normalizeModelName(nonEmpty(t.model, t.servingModel))
}

// buildSessions groups traces into sessions. O(n log n + n*k) where k is the
// number of concurrently open sessions of one key.
func buildSessions(traces []*uiTrace, gap time.Duration) *sessionIndex {
	ordered := append([]*uiTrace(nil), traces...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].start != ordered[j].start {
			return ordered[i].start < ordered[j].start
		}
		return ordered[i].tid.String() < ordered[j].tid.String()
	})
	idx := &sessionIndex{member: make(map[*uiTrace]sessionMember, len(ordered))}
	open := map[string][]*uiSession{}
	gapNanos := gap.Nanoseconds()
	for _, t := range ordered {
		key := sessionKey(t)
		prompt := t.servingPrompt
		var best *uiSession
		live := open[key][:0]
		for _, sess := range open[key] {
			if t.start-sess.lastAt > gapNanos {
				continue // closed by the pause; drop it from the open list
			}
			live = append(live, sess)
			if prompt > 0 && sess.lastPrompt > 0 && float64(prompt) < float64(sess.lastPrompt)*sessionShrinkTolerance {
				continue
			}
			switch {
			case best == nil:
				best = sess
			case prompt > 0 && sess.lastPrompt != best.lastPrompt:
				if sess.lastPrompt > best.lastPrompt {
					best = sess
				}
			case sess.lastAt > best.lastAt:
				best = sess
			}
		}
		open[key] = live
		if best == nil {
			best = &uiSession{id: t.tid.String(), clientKey: t.clientKey, model: nonEmpty(t.model, t.servingModel)}
			idx.sessions = append(idx.sessions, best)
			open[key] = append(open[key], best)
		}
		best.traces = append(best.traces, t)
		if prompt > 0 {
			best.lastPrompt = prompt
		}
		best.lastAt = max(best.lastAt, t.start, t.end)
		idx.member[t] = sessionMember{session: best, seq: len(best.traces)}
	}
	return idx
}

// sessionsLocked returns the session index, rebuilt only when the stored
// traces or the session settings changed.
func (s *traceStore) sessionsLocked() *sessionIndex {
	if s.sessCache != nil && s.sessVersion == s.version {
		return s.sessCache
	}
	s.sessCache = buildSessions(s.buf, s.sessionGap)
	s.sessVersion = s.version
	return s.sessCache
}

// sessionJSON is one /sessions row. Context figures use the latest request:
// its prompt is what the model had to read, and the next request will be at
// least that large.
type sessionJSON struct {
	ID                 string  `json:"id"`
	ClientKey          string  `json:"client_key"`
	Model              string  `json:"model"`
	ServingModel       string  `json:"serving_model"`
	Provider           string  `json:"provider"`
	Account            string  `json:"account"`
	Requests           int     `json:"requests"`
	Errors             int     `json:"errors"`
	Canceled           int     `json:"canceled"`
	StartedAt          string  `json:"started_at"`
	LastAt             string  `json:"last_at"`
	Active             bool    `json:"active"`
	FirstContextTokens int64   `json:"first_context_tokens"`
	ContextTokens      int64   `json:"context_tokens"`
	PeakContextTokens  int64   `json:"peak_context_tokens"`
	ContextWindow      int64   `json:"context_window"`
	ContextUsed        float64 `json:"context_used"`
	// GrowthPerRequest is the average prompt growth between requests;
	// RequestsUntilFull extrapolates it (-1: unknown or not growing).
	GrowthPerRequest    int64   `json:"growth_per_request"`
	RequestsUntilFull   int     `json:"requests_until_full"`
	TotalInputTokens    int64   `json:"total_input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	OutputTokens        int64   `json:"output_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	NoCacheCostUSD      float64 `json:"cost_without_cache_usd"`
	Priced              bool    `json:"priced"`
	CostMinUSD          float64 `json:"cost_min_usd"`
	UnknownCostRequests int     `json:"unknown_cost_requests"`
}

// sessions lists sessions with activity at or after since (unix nanos; 0:
// all), most recently active first.
func (s *traceStore) sessions(since int64, limit int, now time.Time, labels map[string]string) []sessionJSON {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.sessionsLocked()
	picked := make([]*uiSession, 0, len(idx.sessions))
	for _, sess := range idx.sessions {
		if since <= 0 || sess.lastAt >= since {
			picked = append(picked, sess)
		}
	}
	sort.Slice(picked, func(i, j int) bool {
		if picked[i].lastAt != picked[j].lastAt {
			return picked[i].lastAt > picked[j].lastAt
		}
		return picked[i].id > picked[j].id
	})
	limit = min(max(limit, 1), maxSessionsList)
	if len(picked) > limit {
		picked = picked[:limit]
	}
	out := make([]sessionJSON, 0, len(picked))
	for _, sess := range picked {
		out = append(out, s.sessionRowLocked(sess, now, labels))
	}
	return out
}

func (s *traceStore) sessionRowLocked(sess *uiSession, now time.Time, labels map[string]string) sessionJSON {
	first, last := sess.traces[0], sess.traces[len(sess.traces)-1]
	row := sessionJSON{
		ID:                sess.id,
		ClientKey:         sess.clientKey,
		Model:             sess.model,
		Requests:          len(sess.traces),
		StartedAt:         time.Unix(0, first.start).UTC().Format(time.RFC3339Nano),
		LastAt:            time.Unix(0, sess.lastAt).UTC().Format(time.RFC3339Nano),
		Active:            now.UnixNano()-sess.lastAt <= s.sessionGap.Nanoseconds(),
		RequestsUntilFull: -1,
		Priced:            true,
	}
	var firstPrompt, lastPrompt int64
	prompted := 0
	for _, t := range sess.traces {
		if t.isCanceled() {
			row.Canceled++
		}
		if t.isError() {
			row.Errors++
		}
		row.TotalInputTokens += t.totalInput
		row.CacheReadTokens += t.cacheRead
		row.CacheCreationTokens += t.cacheWrite
		row.OutputTokens += t.output
		c := s.costLocked(t)
		row.CostUSD += c.cost
		row.CostMinUSD += c.costMin
		row.NoCacheCostUSD += c.noCache
		row.Priced = row.Priced && c.priced
		if c.unknown {
			row.UnknownCostRequests++
		}
		if t.servingPrompt > 0 {
			if prompted == 0 {
				firstPrompt = t.servingPrompt
			}
			lastPrompt = t.servingPrompt
			prompted++
			row.PeakContextTokens = max(row.PeakContextTokens, t.servingPrompt)
		}
		if t.servingModel != "" {
			row.ServingModel = t.servingModel
		}
		if t.provider != "" {
			row.Provider = t.provider
		}
	}
	row.Account = accountLabel(labels, last.authIndex, last.authType)
	row.FirstContextTokens, row.ContextTokens = firstPrompt, lastPrompt
	row.CacheHitRate = cacheHitRate(row.CacheReadTokens, row.TotalInputTokens)
	row.CostUSD, row.NoCacheCostUSD = roundUSD(row.CostUSD), roundUSD(row.NoCacheCostUSD)
	row.CostMinUSD = roundUSD(row.CostMinUSD)
	row.ContextWindow = s.models.contextWindow(nonEmpty(row.ServingModel, row.Model))
	if row.ContextWindow > 0 && lastPrompt > 0 {
		row.ContextUsed = roundTo(float64(lastPrompt)/float64(row.ContextWindow), 1e4)
	}
	if prompted > 1 && lastPrompt > firstPrompt {
		row.GrowthPerRequest = (lastPrompt - firstPrompt) / int64(prompted-1)
		if row.ContextWindow > 0 && row.GrowthPerRequest > 0 {
			row.RequestsUntilFull = int(max(row.ContextWindow-lastPrompt, 0) / row.GrowthPerRequest)
		}
	}
	return row
}

// validSessionID reports whether id looks like a session id (a trace id).
func validSessionID(id string) bool {
	_, ok := parseTraceIDParam(id)
	return ok && strings.ToLower(id) == id
}
