package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxStoredAttempts bounds attempt spans kept per stored trace. The
	// attempt and token counters still include attempts beyond the cap.
	maxStoredAttempts = 16
	// maxStoredAttrBytes bounds every stored string attribute (error text).
	maxStoredAttrBytes = 512
	// maxItemErrorBytes bounds the error column of a trace summary.
	maxItemErrorBytes = 256
	// summaryWindow is the window of the summary statistics.
	summaryWindow = time.Hour
	// maxSummaryProviders bounds the provider list of the summary.
	maxSummaryProviders = 50
	// maxStoredUsage bounds per-attempt usage entries kept for costing; later
	// attempts are folded into the last entry.
	maxStoredUsage = 64
	// maxUnpricedModels bounds the unpriced model list of the summary.
	maxUnpricedModels = 10
)

// attemptUsage is the costing input of one attempt.
type attemptUsage struct {
	model, provider, authType string
	tok                       spanTokens
}

const (
	attrString uint8 = iota
	attrInt
	attrBool
	attrDouble
)

// uiAttr is one stored span attribute. Values come from the exported (already
// sanitized) OTLP span, never from raw host records.
type uiAttr struct {
	key  string
	kind uint8
	s    string
	n    int64
	f    float64
	b    bool
}

func (a uiAttr) value() any {
	switch a.kind {
	case attrInt:
		return a.n
	case attrBool:
		return a.b
	case attrDouble:
		return a.f
	default:
		return a.s
	}
}

type uiEvent struct {
	name string
	at   int64
}

// uiSpan is a compact copy of one exported span.
type uiSpan struct {
	spanID, parentID, name string
	kind                   int
	start, end             int64
	failed                 bool
	attrs                  []uiAttr
	events                 []uiEvent
}

func (s *uiSpan) str(key string) string {
	for _, a := range s.attrs {
		if a.key == key && a.kind == attrString {
			return a.s
		}
	}
	return ""
}

func (s *uiSpan) num(key string) int64 {
	for _, a := range s.attrs {
		if a.key == key && a.kind == attrInt {
			return a.n
		}
	}
	return 0
}

func (s *uiSpan) flag(key string) bool {
	for _, a := range s.attrs {
		if a.key == key && a.kind == attrBool {
			return a.b
		}
	}
	return false
}

// spanTokens is the token breakdown of one attempt span. input is the
// provider-reported gen_ai.usage.input_tokens; totalInput is the normalized
// prompt size (uncached + cacheRead + cacheWrite, see normalizeInput).
type spanTokens struct {
	input, totalInput, uncached, cacheRead, cacheWrite, cached, output, reasoning int64
}

func (s *uiSpan) tokens() spanTokens {
	t := spanTokens{
		input:      s.num("gen_ai.usage.input_tokens"),
		totalInput: s.num("cpa.usage.total_input_tokens"),
		cacheRead:  s.num("cpa.usage.cache_read_tokens"),
		cacheWrite: s.num("cpa.usage.cache_creation_tokens"),
		cached:     s.num("cpa.usage.cached_tokens"),
		output:     s.num("gen_ai.usage.output_tokens"),
		reasoning:  s.num("cpa.usage.reasoning_tokens"),
	}
	if t.cacheRead == 0 && t.cacheWrite == 0 {
		t.cacheRead = t.cached
	}
	t.totalInput = max(t.totalInput, t.cacheRead+t.cacheWrite)
	t.uncached = t.totalInput - t.cacheRead - t.cacheWrite
	return t
}

// uiTrace is one stored request: its summary plus compact spans (root first).
type uiTrace struct {
	tid                                                        traceID
	cpaTraceID                                                 string
	start, end                                                 int64
	model, sourceFormat, outcome, clientKey, rootErr, lastFail string
	stream                                                     bool
	statusCode                                                 int
	attempts, failed                                           int
	input, output, cached                                      int64
	totalInput, cacheRead, cacheWrite, reasoning               int64
	// The serving attempt is the latest successful attempt, or the latest
	// attempt when all failed. Its credential is resolved to an account
	// label only when the page asks (labels are not stored).
	provider, authIndex, authType string
	servingStart                  int64
	servingFailed, hasServing     bool
	// servingModel and servingPrompt describe the serving attempt: the
	// upstream model and its prompt size (the context sent to the model).
	servingModel              string
	servingPrompt, servingOut int64
	usage                     []attemptUsage
	// detail is true while the trace keeps its spans (the newest
	// ui_max_traces requests); older history keeps totals only.
	hasSpans bool
	spans    []uiSpan
}

// attemptModel is the upstream model of an attempt span.
func attemptModel(s *uiSpan, fallback string) string {
	return nonEmpty(s.str("gen_ai.response.model"), nonEmpty(s.str("gen_ai.request.model"), fallback))
}

func (t *uiTrace) isCanceled() bool { return t.outcome == outcomeCanceled }

// isError excludes client cancellations.
func (t *uiTrace) isError() bool {
	return !t.isCanceled() && (t.outcome != outcomeSucceeded || t.statusCode >= 400)
}

func (t *uiTrace) durationMS() float64 { return nanosToMS(t.end - t.start) }

func (t *uiTrace) errorText() string {
	if t.rootErr != "" {
		return t.rootErr
	}
	if t.outcome != outcomeSucceeded {
		return t.lastFail
	}
	return ""
}

func parseNano(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// toUISpan converts an exported span. Langfuse mapping attributes duplicate
// other keys and are dropped; long strings are truncated.
func toUISpan(s *otlpSpan) uiSpan {
	out := uiSpan{
		spanID:   s.SpanID,
		parentID: s.ParentSpanID,
		name:     s.Name,
		kind:     s.Kind,
		start:    parseNano(s.StartTimeUnixNano),
		end:      parseNano(s.EndTimeUnixNano),
		failed:   s.Status.Code == statusError,
		attrs:    make([]uiAttr, 0, len(s.Attributes)),
	}
	for _, kv := range s.Attributes {
		// Langfuse mapping keys duplicate other keys. cpa.account (only with
		// export_account_labels) is resolved freshly by the page instead.
		if strings.HasPrefix(kv.Key, "langfuse.") || kv.Key == "cpa.account" {
			continue
		}
		a := uiAttr{key: kv.Key}
		switch v := kv.Value; {
		case v.StringValue != nil:
			a.s = *v.StringValue
			if len(a.s) > maxStoredAttrBytes {
				a.s = truncateUTF8(a.s, maxStoredAttrBytes-len(truncatedMarker)) + truncatedMarker
			}
		case v.IntValue != nil:
			a.kind = attrInt
			a.n, _ = strconv.ParseInt(*v.IntValue, 10, 64)
		case v.BoolValue != nil:
			a.kind, a.b = attrBool, *v.BoolValue
		case v.DoubleValue != nil:
			a.kind, a.f = attrDouble, *v.DoubleValue
		default:
			continue
		}
		out.attrs = append(out.attrs, a)
	}
	for _, e := range s.Events {
		out.events = append(out.events, uiEvent{name: e.Name, at: parseNano(e.TimeUnixNano)})
	}
	return out
}

func truncateItemError(s string) string {
	if len(s) > maxItemErrorBytes {
		return truncateUTF8(s, maxItemErrorBytes-len(truncatedMarker)) + truncatedMarker
	}
	return s
}

// newUITrace builds a stored trace from the spans handed to the exporter. A
// nil root means a usage record without TraceID: its single span is the root.
func newUITrace(tid traceID, root *otlpSpan, attempts []otlpSpan) *uiTrace {
	t := &uiTrace{tid: tid, outcome: "unknown", hasSpans: true}
	if root == nil {
		if len(attempts) == 0 {
			return nil
		}
		s := toUISpan(&attempts[0])
		t.start, t.end = s.start, s.end
		t.cpaTraceID = s.str("cpa.trace_id")
		t.model = nonEmpty(s.str("cpa.alias"), s.str("gen_ai.request.model"))
		t.stream = s.flag("cpa.stream")
		t.statusCode = int(s.num("http.response.status_code"))
		t.spans = make([]uiSpan, 0, 1)
		t.addAttempt(s)
		return t
	}
	r := toUISpan(root)
	t.start, t.end = r.start, r.end
	t.cpaTraceID = r.str("cpa.trace_id")
	t.model = nonEmpty(r.str("gen_ai.request.model"), r.str("cpa.model"))
	t.sourceFormat = r.str("cpa.source_format")
	t.stream = r.flag("cpa.stream")
	t.outcome = nonEmpty(r.str("cpa.outcome"), "unknown")
	t.statusCode = int(r.num("http.response.status_code"))
	t.clientKey = r.str("cpa.client_key")
	t.rootErr = truncateItemError(r.str("cpa.error"))
	t.spans = make([]uiSpan, 0, 1+min(len(attempts), maxStoredAttempts))
	t.spans = append(t.spans, r)
	for i := range attempts {
		t.addAttempt(toUISpan(&attempts[i]))
	}
	return t
}

// addAttempt folds one attempt span into the counters and keeps the span
// while under maxStoredAttempts.
func (t *uiTrace) addAttempt(s uiSpan) {
	t.attempts++
	tok := s.tokens()
	t.input += tok.input
	t.totalInput += tok.totalInput
	t.cacheRead += tok.cacheRead
	t.cacheWrite += tok.cacheWrite
	t.output += tok.output
	t.reasoning += tok.reasoning
	t.cached += max(tok.cached, s.num("cpa.usage.cache_read_tokens"))
	model := attemptModel(&s, t.model)
	provider := s.str("gen_ai.provider.name")
	if len(t.usage) < maxStoredUsage {
		t.usage = append(t.usage, attemptUsage{model: model, provider: provider, authType: s.str("cpa.auth_type"), tok: tok})
	} else {
		last := &t.usage[len(t.usage)-1]
		last.tok.uncached += tok.uncached
		last.tok.totalInput += tok.totalInput
		last.tok.cacheRead += tok.cacheRead
		last.tok.cacheWrite += tok.cacheWrite
		last.tok.output += tok.output
		last.tok.reasoning += tok.reasoning
	}
	if !t.hasServing || (t.servingFailed && !s.failed) || (t.servingFailed == s.failed && s.start >= t.servingStart) {
		t.hasServing, t.servingFailed, t.servingStart = true, s.failed, s.start
		t.provider = provider
		t.authIndex, t.authType = s.str("cpa.auth_index"), s.str("cpa.auth_type")
		t.servingModel, t.servingPrompt, t.servingOut = model, tok.totalInput, tok.output
	}
	if t.clientKey == "" {
		t.clientKey = s.str("cpa.client_key")
	}
	if s.failed {
		t.failed++
		if text := s.str("cpa.error"); text != "" {
			t.lastFail = truncateItemError(text)
		}
	}
	if t.hasSpans && len(t.spans) <= maxStoredAttempts {
		t.spans = append(t.spans, s)
	}
}

// traceStore holds the request history behind the Traces page, oldest
// first. Every request in the history window keeps its totals (tokens, cost,
// session); only the newest detailLimit requests also keep their attempt
// spans. It has its own lock so page requests never hold the tracer lock
// while filtering or encoding. Lock order: tracer.mu, then store.mu.
type traceStore struct {
	mu sync.Mutex
	// detailLimit is ui_max_traces; historyMax and historyAge bound the
	// history (historyAge 0: no age limit). detailLimit 0 disables the store.
	detailLimit int
	historyMax  int
	historyAge  time.Duration
	buf         []*uiTrace // insertion order, oldest first
	detailed    int        // traces at the end of buf that still have spans
	byID        map[traceID]*uiTrace
	now         func() time.Time
	// persist, when set, receives an encoded copy of every new or updated
	// trace (see history.go). It must not block.
	persist func(line []byte)
	// models prices attempts and knows context windows; sessionGap splits
	// sessions. Both come from config.
	models     *pricer
	sessionGap time.Duration
	// version changes whenever traces or settings change; it invalidates
	// the cached session index.
	version     uint64
	sessCache   *sessionIndex
	sessVersion uint64
}

func (s *traceStore) setModels(p *pricer, gap time.Duration) {
	if gap <= 0 {
		gap = 30 * time.Minute
	}
	s.mu.Lock()
	s.models, s.sessionGap = p, gap
	s.version++
	s.mu.Unlock()
}

// traceCost sums attempt costs at API list prices. priced is false when any
// attempt used tokens of a model without a price; unpriced names it.
type traceCost struct {
	cost, noCache float64
	priced        bool
	unpriced      string
}

func (s *traceStore) costLocked(t *uiTrace) traceCost {
	out := traceCost{priced: true}
	for _, u := range t.usage {
		c := s.models.cost(u.model, u.provider, u.authType, u.tok)
		if !c.ok {
			out.priced = false
			if out.unpriced == "" {
				out.unpriced = nonEmpty(u.model, "(unknown model)")
			}
			continue
		}
		out.cost += c.cost
		out.noCache += c.noCache
	}
	return out
}

func newTraceStore() *traceStore {
	return &traceStore{byID: map[traceID]*uiTrace{}, models: newPricer(nil, nil), sessionGap: 30 * time.Minute, now: time.Now}
}

// configure sets the limits. A detail limit of 0 disables the store and
// releases everything. historyMax below the detail limit is raised to it.
func (s *traceStore) configure(enabled bool, detailLimit, historyMax int, historyAge time.Duration) {
	if !enabled {
		detailLimit, historyMax = 0, 0
	}
	historyMax = max(historyMax, detailLimit)
	s.mu.Lock()
	defer s.mu.Unlock()
	if detailLimit == s.detailLimit && historyMax == s.historyMax && historyAge == s.historyAge {
		return
	}
	s.detailLimit, s.historyMax, s.historyAge = detailLimit, historyMax, historyAge
	if detailLimit == 0 {
		s.buf, s.detailed = nil, 0
		clear(s.byID)
	}
	s.enforceLocked()
	s.version++
}

// enforceLocked drops the oldest traces beyond the history limits and strips
// spans from traces beyond the detail limit.
func (s *traceStore) enforceLocked() {
	drop := max(len(s.buf)-s.historyMax, 0)
	if s.historyAge > 0 && s.now != nil {
		cutoff := s.now().Add(-s.historyAge).UnixNano()
		for drop < len(s.buf) && s.buf[drop].start < cutoff {
			drop++
		}
	}
	if drop > 0 {
		for _, t := range s.buf[:drop] {
			delete(s.byID, t.tid)
		}
		rest := s.buf[drop:]
		if cap(s.buf) > 2*len(rest)+64 {
			rest = append([]*uiTrace(nil), rest...) // release the old backing array
		}
		s.buf = rest
		s.detailed = min(s.detailed, len(s.buf))
	}
	for s.detailed > s.detailLimit {
		t := s.buf[len(s.buf)-s.detailed]
		t.spans, t.hasSpans = nil, false
		s.detailed--
	}
}

func (s *traceStore) pushLocked(t *uiTrace) {
	if s.detailLimit <= 0 || t == nil {
		return
	}
	if old := s.byID[t.tid]; old != nil {
		return // already stored (a replayed history record)
	}
	s.buf = append(s.buf, t)
	if t.hasSpans {
		s.detailed++
	}
	s.byID[t.tid] = t
	s.enforceLocked()
	s.version++
}

func (s *traceStore) add(t *uiTrace) {
	if t == nil {
		return
	}
	t.hasSpans = true
	s.mu.Lock()
	s.pushLocked(t)
	if s.persist != nil && s.byID[t.tid] == t {
		s.persist(encodeTrace(t))
	}
	s.mu.Unlock()
}

// addLateAttempt joins a usage record that arrived after its trace was
// finalized. It is a no-op when the trace has already been evicted.
func (s *traceStore) addLateAttempt(tid traceID, span *otlpSpan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.byID[tid]; t != nil {
		t.addAttempt(toUISpan(span))
		s.version++
		if s.persist != nil {
			s.persist(encodeTrace(t))
		}
	}
}

// setPersist installs (or with nil removes) the history writer hook.
func (s *traceStore) setPersist(fn func(line []byte)) {
	s.mu.Lock()
	s.persist = fn
	s.mu.Unlock()
}

// importHistory merges traces loaded from disk (any order). Traces already
// in memory win. Only the newest detailLimit traces keep spans.
func (s *traceStore) importHistory(loaded []*uiTrace) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detailLimit <= 0 || len(loaded) == 0 {
		return 0
	}
	fresh := make([]*uiTrace, 0, len(loaded))
	for _, t := range loaded {
		if t != nil && s.byID[t.tid] == nil {
			fresh = append(fresh, t)
		}
	}
	merged := append(fresh, s.buf...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].start < merged[j].start })
	s.buf = merged
	s.detailed = 0
	for i := len(merged) - 1; i >= 0 && merged[i].hasSpans; i-- {
		s.detailed++
	}
	// Strip spans from any detailed trace that is not in the newest tail.
	for _, t := range merged[:len(merged)-s.detailed] {
		t.spans, t.hasSpans = nil, false
	}
	for _, t := range fresh {
		s.byID[t.tid] = t
	}
	s.enforceLocked()
	s.version++
	return len(fresh)
}

func (s *traceStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

func (s *traceStore) capacity() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.historyMax
}

// --- JSON views -----------------------------------------------------------

// traceItemJSON is one /traces row. Token fields sum over all attempts:
// input_tokens is the provider-reported value (Claude: uncached only),
// total_input_tokens the normalized prompt size including cache reads and
// writes exactly once.
type traceItemJSON struct {
	TraceID             string  `json:"trace_id"`
	CPATraceID          string  `json:"cpa_trace_id"`
	StartedAt           string  `json:"started_at"`
	DurationMS          float64 `json:"duration_ms"`
	RequestedModel      string  `json:"requested_model"`
	SourceFormat        string  `json:"source_format"`
	Stream              bool    `json:"stream"`
	Outcome             string  `json:"outcome"`
	StatusCode          int     `json:"status_code"`
	AttemptCount        int     `json:"attempt_count"`
	FailedAttempts      int     `json:"failed_attempts"`
	InputTokens         int64   `json:"input_tokens"`
	TotalInputTokens    int64   `json:"total_input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	Provider            string  `json:"provider"`
	Account             string  `json:"account"`
	ClientKey           string  `json:"client_key"`
	Error               string  `json:"error"`
	// Cost fields are estimates at API list prices (see pricing.go); priced
	// is false when a model has no price, and then cost covers priced
	// attempts only.
	CostUSD        float64 `json:"cost_usd"`
	NoCacheCostUSD float64 `json:"cost_without_cache_usd"`
	Priced         bool    `json:"priced"`
	// HasDetail is false for older history requests whose attempt spans
	// are no longer kept (totals only).
	HasDetail bool `json:"has_detail"`
	// PriceSource names where the serving model's price comes from.
	PriceSource string `json:"price_source"`
	// Context is the serving attempt's prompt size against the model's
	// context window (0 when unknown).
	ServingModel  string  `json:"serving_model"`
	ContextTokens int64   `json:"context_tokens"`
	ContextWindow int64   `json:"context_window"`
	ContextUsed   float64 `json:"context_used"`
	// Session is the estimated conversation this request belongs to.
	SessionID       string `json:"session_id"`
	SessionSeq      int    `json:"session_seq"`
	SessionRequests int    `json:"session_requests"`
}

type spanEventJSON struct {
	Name     string  `json:"name"`
	OffsetMS float64 `json:"offset_ms"`
}

// spanTokensJSON is the per-attempt token breakdown.
type spanTokensJSON struct {
	InputTokens         int64   `json:"input_tokens"`
	TotalInputTokens    int64   `json:"total_input_tokens"`
	UncachedInputTokens int64   `json:"uncached_input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
}

type spanJSON struct {
	SpanID        string          `json:"span_id"`
	ParentSpanID  string          `json:"parent_span_id"`
	Name          string          `json:"name"`
	Kind          string          `json:"kind"`
	StartOffsetMS float64         `json:"start_offset_ms"`
	DurationMS    float64         `json:"duration_ms"`
	Status        string          `json:"status"`
	Provider      string          `json:"provider,omitempty"`
	Account       string          `json:"account,omitempty"`
	Tokens        *spanTokensJSON `json:"tokens,omitempty"`
	CostUSD       *float64        `json:"cost_usd,omitempty"`
	Attributes    map[string]any  `json:"attributes"`
	Events        []spanEventJSON `json:"events"`
}

type traceDetailJSON struct {
	traceItemJSON
	Spans []spanJSON `json:"spans"`
}

func roundTo(v, scale float64) float64 { return math.Round(v*scale) / scale }

func nanosToMS(d int64) float64 {
	if d <= 0 {
		return 0
	}
	return math.Round(float64(d)/1e3) / 1e3
}

func offsetMS(at, origin int64) float64 {
	return math.Round(float64(at-origin)/1e3) / 1e3
}

// item renders the summary row. labels maps auth index -> account label
// (nil: fallback labels only).
func (t *uiTrace) item(labels map[string]string) traceItemJSON {
	return traceItemJSON{
		TraceID:             t.tid.String(),
		CPATraceID:          t.cpaTraceID,
		StartedAt:           time.Unix(0, t.start).UTC().Format(time.RFC3339Nano),
		DurationMS:          t.durationMS(),
		RequestedModel:      t.model,
		SourceFormat:        t.sourceFormat,
		Stream:              t.stream,
		Outcome:             t.outcome,
		StatusCode:          t.statusCode,
		AttemptCount:        t.attempts,
		FailedAttempts:      t.failed,
		InputTokens:         t.input,
		TotalInputTokens:    t.totalInput,
		CacheReadTokens:     t.cacheRead,
		CacheCreationTokens: t.cacheWrite,
		CacheHitRate:        cacheHitRate(t.cacheRead, t.totalInput),
		OutputTokens:        t.output,
		ReasoningTokens:     t.reasoning,
		CachedTokens:        t.cached,
		Provider:            t.provider,
		Account:             accountLabel(labels, t.authIndex, t.authType),
		ClientKey:           t.clientKey,
		Error:               t.errorText(),
		ServingModel:        t.servingModel,
		ContextTokens:       t.servingPrompt,
		Priced:              true,
		HasDetail:           t.hasSpans,
	}
}

// decorateLocked adds cost, context window and session fields.
func (s *traceStore) decorateLocked(item *traceItemJSON, t *uiTrace, sess *sessionIndex) {
	c := s.costLocked(t)
	item.CostUSD, item.NoCacheCostUSD, item.Priced = roundUSD(c.cost), roundUSD(c.noCache), c.priced
	item.ContextWindow = s.models.contextWindow(nonEmpty(t.servingModel, t.model))
	item.PriceSource = s.models.priceSource(nonEmpty(t.servingModel, t.model))
	if item.ContextWindow > 0 && t.servingPrompt > 0 {
		item.ContextUsed = roundTo(float64(t.servingPrompt)/float64(item.ContextWindow), 1e4)
	}
	if sess != nil {
		if m, ok := sess.member[t]; ok {
			item.SessionID, item.SessionSeq, item.SessionRequests = m.session.id, m.seq, len(m.session.traces)
		}
	}
}

func spanKindName(kind int) string {
	switch kind {
	case spanKindServer:
		return "server"
	case spanKindClient:
		return "client"
	default:
		return "internal"
	}
}

func (t *uiTrace) detail(labels map[string]string, models *pricer) traceDetailJSON {
	out := traceDetailJSON{traceItemJSON: t.item(labels), Spans: make([]spanJSON, 0, len(t.spans))}
	ordered := make([]*uiSpan, 0, len(t.spans))
	for i := range t.spans {
		ordered = append(ordered, &t.spans[i])
	}
	// Root (no parent) first, then attempts by start time.
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if (a.parentID == "") != (b.parentID == "") {
			return a.parentID == ""
		}
		return a.start < b.start
	})
	for _, s := range ordered {
		status := "unset"
		if s.failed {
			status = "error"
		}
		attrs := make(map[string]any, len(s.attrs))
		for _, a := range s.attrs {
			attrs[a.key] = a.value()
		}
		events := make([]spanEventJSON, 0, len(s.events))
		for _, e := range s.events {
			events = append(events, spanEventJSON{Name: e.name, OffsetMS: offsetMS(e.at, t.start)})
		}
		span := spanJSON{
			SpanID:        s.spanID,
			ParentSpanID:  s.parentID,
			Name:          s.name,
			Kind:          spanKindName(s.kind),
			StartOffsetMS: offsetMS(s.start, t.start),
			DurationMS:    nanosToMS(s.end - s.start),
			Status:        status,
			Attributes:    attrs,
			Events:        events,
		}
		if s.kind == spanKindClient {
			tok := s.tokens()
			span.Provider = s.str("gen_ai.provider.name")
			span.Account = accountLabel(labels, s.str("cpa.auth_index"), s.str("cpa.auth_type"))
			if c := models.cost(attemptModel(s, t.model), span.Provider, s.str("cpa.auth_type"), tok); c.ok {
				v := roundUSD(c.cost)
				span.CostUSD = &v
			}
			span.Tokens = &spanTokensJSON{
				InputTokens:         tok.input,
				TotalInputTokens:    tok.totalInput,
				UncachedInputTokens: tok.uncached,
				CacheReadTokens:     tok.cacheRead,
				CacheCreationTokens: tok.cacheWrite,
				CacheHitRate:        cacheHitRate(tok.cacheRead, tok.totalInput),
				OutputTokens:        tok.output,
				ReasoningTokens:     tok.reasoning,
			}
		}
		out.Spans = append(out.Spans, span)
	}
	return out
}

// traceFilter is a validated /traces query.
type traceFilter struct {
	limit    int
	status   string // all, ok, error
	model    string // lower-case substring
	provider string // lower-case exact match on the serving attempt's provider
	minMS    float64
	since    int64  // unix nanos; traces that started earlier are skipped (0: no limit)
	session  string // session id; empty: any
}

func (f traceFilter) match(t *uiTrace) bool {
	switch f.status {
	case "ok":
		if t.isError() {
			return false
		}
	case "error":
		if !t.isError() {
			return false
		}
	case "canceled":
		if !t.isCanceled() {
			return false
		}
	}
	if f.model != "" && !strings.Contains(strings.ToLower(t.model), f.model) {
		return false
	}
	if f.provider != "" && strings.ToLower(t.provider) != f.provider {
		return false
	}
	if f.since > 0 && t.start < f.since {
		return false
	}
	return f.minMS <= 0 || t.durationMS() >= f.minMS
}

// list returns matching traces, newest start time first. labels maps auth
// index -> account label (nil: fallback labels only).
func (s *traceStore) list(f traceFilter, labels map[string]string) []traceItemJSON {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessionsLocked()
	matches := make([]*uiTrace, 0, min(len(s.buf), 64))
	for _, t := range s.buf {
		if !f.match(t) {
			continue
		}
		if f.session != "" {
			if m, ok := sess.member[t]; !ok || m.session.id != f.session {
				continue
			}
		}
		matches = append(matches, t)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start > matches[j].start
		}
		return matches[i].tid.String() > matches[j].tid.String()
	})
	if len(matches) > f.limit {
		matches = matches[:f.limit]
	}
	out := make([]traceItemJSON, 0, len(matches))
	for _, t := range matches {
		item := t.item(labels)
		s.decorateLocked(&item, t, sess)
		out = append(out, item)
	}
	return out
}

func (s *traceStore) get(tid traceID, labels map[string]string) (traceDetailJSON, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byID[tid]
	if t == nil {
		return traceDetailJSON{}, false
	}
	d := t.detail(labels, s.models)
	s.decorateLocked(&d.traceItemJSON, t, s.sessionsLocked())
	return d, true
}

type summaryStats struct {
	Requests            int     `json:"requests"`
	Errors              int     `json:"errors"`
	ErrorRate           float64 `json:"error_rate"`
	AvgLatencyMS        float64 `json:"avg_latency_ms"`
	P95LatencyMS        float64 `json:"p95_latency_ms"`
	InputTokens         int64   `json:"input_tokens"`
	TotalInputTokens    int64   `json:"total_input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	OutputTokens        int64   `json:"output_tokens"`
	RetriedRequests     int     `json:"retried_requests"`
	// Canceled counts client cancellations; they are not errors.
	Canceled int `json:"canceled"`
	// Cost of the window at API list prices; UnpricedRequests used a model
	// without a price (their priced attempts are still counted).
	CostUSD          float64        `json:"cost_usd"`
	NoCacheCostUSD   float64        `json:"cost_without_cache_usd"`
	CacheSavingsUSD  float64        `json:"cache_savings_usd"`
	UnpricedRequests int            `json:"unpriced_requests"`
	UnpricedModels   []string       `json:"unpriced_models"`
	PricesAsOf       string         `json:"prices_as_of"`
	PriceTable       map[string]any `json:"price_table"`
	// OldestTraceAt / NewestTraceAt bound the requests in the window.
	OldestTraceAt string `json:"oldest_trace_at"`
	NewestTraceAt string `json:"newest_trace_at"`
	StoredTraces  int    `json:"stored_traces"`
	// DetailTraces is ui_max_traces: the newest requests that keep spans.
	DetailTraces int `json:"detail_traces"`
	Capacity     int `json:"capacity"`
	// Providers lists the serving providers of all stored traces (sorted,
	// at most maxSummaryProviders) for the page's provider filter.
	Providers []string `json:"providers"`
}

// summary aggregates stored traces that started within summaryWindow.
func (s *traceStore) summary(now time.Time) summaryStats { return s.summaryFor(now, summaryWindow) }

// summaryFor aggregates stored traces that started within window before now
// (window <= 0: every stored trace). It is O(n log n) over at most
// ui_max_traces entries.
func (s *traceStore) summaryFor(now time.Time, window time.Duration) summaryStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := int64(math.MinInt64)
	if window > 0 {
		cutoff = now.Add(-window).UnixNano()
	}
	out := summaryStats{StoredTraces: len(s.buf), DetailTraces: s.detailLimit, Capacity: s.historyMax, Providers: []string{}, UnpricedModels: []string{}, PricesAsOf: pricesAsOfText(), PriceTable: priceTableInfo()}
	var oldest, newest int64
	unpriced := map[string]bool{}
	durations := make([]float64, 0, len(s.buf))
	var total float64
	providers := map[string]bool{}
	for _, t := range s.buf {
		if t.provider != "" && !providers[t.provider] && len(providers) < maxSummaryProviders {
			providers[t.provider] = true
			out.Providers = append(out.Providers, t.provider)
		}
		if t.start < cutoff {
			continue
		}
		out.Requests++
		if t.isCanceled() {
			out.Canceled++
		}
		if t.isError() {
			out.Errors++
		}
		if t.attempts > 1 {
			out.RetriedRequests++
		}
		out.InputTokens += t.input
		out.TotalInputTokens += t.totalInput
		out.CacheReadTokens += t.cacheRead
		out.CacheCreationTokens += t.cacheWrite
		out.OutputTokens += t.output
		c := s.costLocked(t)
		out.CostUSD += c.cost
		out.NoCacheCostUSD += c.noCache
		if !c.priced {
			out.UnpricedRequests++
			if !unpriced[c.unpriced] && len(unpriced) < maxUnpricedModels {
				unpriced[c.unpriced] = true
				out.UnpricedModels = append(out.UnpricedModels, c.unpriced)
			}
		}
		if oldest == 0 || t.start < oldest {
			oldest = t.start
		}
		newest = max(newest, t.start)
		d := t.durationMS()
		total += d
		durations = append(durations, d)
	}
	sort.Strings(out.Providers)
	sort.Strings(out.UnpricedModels)
	out.CacheSavingsUSD = roundUSD(out.NoCacheCostUSD - out.CostUSD)
	out.CostUSD, out.NoCacheCostUSD = roundUSD(out.CostUSD), roundUSD(out.NoCacheCostUSD)
	if oldest > 0 {
		out.OldestTraceAt = time.Unix(0, oldest).UTC().Format(time.RFC3339Nano)
		out.NewestTraceAt = time.Unix(0, newest).UTC().Format(time.RFC3339Nano)
	}
	out.CacheHitRate = cacheHitRate(out.CacheReadTokens, out.TotalInputTokens)
	if out.Requests > 0 {
		out.ErrorRate = math.Round(float64(out.Errors)/float64(out.Requests)*1e4) / 1e4
		out.AvgLatencyMS = math.Round(total/float64(out.Requests)*1e3) / 1e3
		out.P95LatencyMS = percentile(durations, 0.95)
	}
	return out
}

// percentile uses the nearest-rank method: the smallest value with at least
// p of the samples at or below it.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	rank := int(math.Ceil(p*float64(len(values)))) - 1
	return values[min(max(rank, 0), len(values)-1)]
}
