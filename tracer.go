package main

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const (
	// maxAttemptsPerRequest bounds usage records buffered for one completion.
	maxAttemptsPerRequest = 256
	// maxOrphansPerTrace bounds unmatched usage records buffered per CPA trace ID.
	maxOrphansPerTrace = 256
	// windowSlack widens a completion's [StartedAt, CompletedAt] window when
	// matching usage records to it (both are wall-clock times from one process).
	windowSlack = time.Second
	// logInterval rate-limits repeated warnings.
	logInterval = time.Minute
)

// pendingCompletion is a completed request waiting out completion_grace_ms for
// late usage records.
type pendingCompletion struct {
	c        requestCompletion
	usages   []usageRecord
	deadline time.Time
}

// pendingUsage is a usage record with no matching completion yet.
type pendingUsage struct {
	u       usageRecord
	expires time.Time
}

// bucket groups everything buffered for one CPA TraceID (one inbound HTTP
// request; a long-lived WebSocket connection reuses one TraceID for many turns,
// so a bucket can hold several completions).
type bucket struct {
	key     string
	comps   []*pendingCompletion
	orphans []pendingUsage
	elem    *list.Element
}

// tombstone remembers an exported request so usage records arriving after the
// grace period still join the same OTel trace, parented to the exported root.
type tombstone struct {
	key        string
	trace      traceID
	root       spanID
	start, end time.Time
	attempts   int
	expires    time.Time
	dead       bool
	// exported reports whether the trace went to the OTLP exporter (it was
	// sampled in); late spans follow the same decision.
	exported bool
}

type counters struct {
	sampledOut     uint64
	evicted        uint64
	queueDropped   uint64
	overflow       uint64
	exportDropped  uint64
	rejected       uint64
	exported       uint64
	lateAttached   uint64
	syntheticRoots uint64
	skipped        uint64
}

// tracer joins usage records and completions into traces and buffers finished
// spans for export. All state is guarded by mu; public methods never block on
// network I/O, so host callbacks return immediately.
type tracer struct {
	mu      sync.Mutex
	cfg     config
	now     func() time.Time
	pending map[string]*bucket
	order   *list.List // buckets, oldest first
	tombs   map[string][]*tombstone
	tombQ   []*tombstone // oldest first
	queue   spanRing
	stats   counters

	reported counters
	lastStat time.Time
	lastLog  map[string]time.Time
	logger   func(level, message string)

	exp *exporter

	// store keeps recent traces for the built-in Traces page.
	store *traceStore
	// accounts caches credential labels from host.auth.list. It is never
	// refreshed on the request path; spans only read its snapshot.
	accounts *accountResolver
	// lastExportErr and lastExportAt describe the exporter for the page.
	lastExportErr string
	lastExportAt  time.Time
}

func newTracer(cfg config, now func() time.Time) *tracer {
	if now == nil {
		now = time.Now
	}
	t := &tracer{
		cfg:     cfg,
		now:     now,
		pending: map[string]*bucket{},
		order:   list.New(),
		tombs:   map[string][]*tombstone{},
		lastLog: map[string]time.Time{},
		logger:  func(string, string) {},
		exp:     newExporter(),
		store:   newTraceStore(),

		accounts: newAccountResolver(hostAuthList),
	}
	t.queue.limit = cfg.MaxQueueSpans
	t.lastStat = now()
	t.store.configure(cfg.uiActive(), cfg.UIMaxTraces)
	t.exp.configure(cfg)
	return t
}

// reconfigure applies a new config, keeping buffered data while the plugin
// keeps recording. Deadlines already scheduled keep their original values.
// Without an exporter the export queue is released; without the Traces page
// the stored traces are released.
func (t *tracer) reconfigure(cfg config) {
	t.mu.Lock()
	if cfg.Endpoint != t.cfg.Endpoint {
		t.lastExportErr, t.lastExportAt = "", time.Time{}
	}
	t.cfg = cfg
	if !cfg.recording() {
		t.pending = map[string]*bucket{}
		t.order.Init()
		t.tombs = map[string][]*tombstone{}
		t.tombQ = nil
	} else {
		for len(t.pending) > cfg.MaxPendingTraces {
			t.evictOldestLocked()
		}
		t.pruneTombsLocked(t.now())
	}
	if !cfg.active() {
		t.queue.clear()
		t.queue.limit = cfg.MaxQueueSpans
	} else {
		t.stats.queueDropped += uint64(t.queue.setLimit(cfg.MaxQueueSpans))
	}
	t.store.configure(cfg.uiActive(), cfg.UIMaxTraces)
	t.mu.Unlock()
	t.exp.configure(cfg)
}

func (t *tracer) config() config {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg
}

// sampledLocked makes a deterministic per-trace decision so every span of a
// trace is kept or dropped together.
func (t *tracer) sampledLocked(key string) bool {
	rate := t.cfg.SampleRate
	if rate >= 1 {
		return true
	}
	if rate <= 0 {
		return false
	}
	sum := sha256.Sum256([]byte(key))
	return float64(binary.BigEndian.Uint64(sum[:8])>>11)/float64(uint64(1)<<53) < rate
}

// keepLocked reports whether events of a trace are buffered at all. The
// Traces page shows every request regardless of sample_rate, so sampling
// only drops data early when the page is off.
func (t *tracer) keepLocked(key string) bool {
	if t.cfg.uiActive() || t.sampledLocked(key) {
		return true
	}
	t.stats.sampledOut++
	return false
}

// exportLocked decides, when a trace is finalized, whether its spans go to
// the OTLP exporter.
func (t *tracer) exportLocked(key string) bool {
	if !t.cfg.active() {
		return false
	}
	if !t.sampledLocked(key) {
		t.stats.sampledOut++
		return false
	}
	return true
}

func contains(c *requestCompletion, at time.Time) bool {
	if at.IsZero() || c.StartedAt.IsZero() || c.CompletedAt.IsZero() {
		return true
	}
	return !at.Before(c.StartedAt.Add(-windowSlack)) && !at.After(c.CompletedAt.Add(windowSlack))
}

// addCompletion buffers a RequestCompletion (request.complete).
func (t *tracer) addCompletion(c requestCompletion) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cfg.recording() || !t.keepLocked(nonEmpty(c.TraceID, c.RequestID)) {
		return
	}
	now := t.now()
	pc := &pendingCompletion{c: c, deadline: now.Add(t.cfg.CompletionGrace)}
	if c.TraceID == "" {
		// Nothing can join a request without a trace ID.
		t.emitCompletionLocked(pc, now)
		return
	}
	b := t.bucketLocked(c.TraceID)
	kept := b.orphans[:0]
	for _, o := range b.orphans {
		if len(pc.usages) < maxAttemptsPerRequest && contains(&pc.c, o.u.RequestedAt) {
			pc.usages = append(pc.usages, o.u)
		} else {
			kept = append(kept, o)
		}
	}
	clear(b.orphans[len(kept):])
	b.orphans = kept
	b.comps = append(b.comps, pc)
}

// addUsage buffers a UsageRecord (usage.handle).
func (t *tracer) addUsage(u usageRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cfg.recording() || !t.keepLocked(nonEmpty(u.TraceID, u.RequestID)) {
		return
	}
	now := t.now()
	if u.TraceID == "" {
		// No parent request: a single-span trace.
		tid := newTraceID()
		span := t.attemptSpanLocked(&u, tid, newSpanID(), spanID{}, 1)
		if t.exportLocked(u.RequestID) {
			t.enqueueLocked(span)
		}
		if t.cfg.uiActive() {
			t.store.add(newUITrace(tid, nil, []otlpSpan{span}))
		}
		return
	}
	if b := t.pending[u.TraceID]; b != nil {
		for i := len(b.comps) - 1; i >= 0; i-- {
			pc := b.comps[i]
			if contains(&pc.c, u.RequestedAt) {
				if len(pc.usages) >= maxAttemptsPerRequest {
					t.stats.overflow++
					return
				}
				pc.usages = append(pc.usages, u)
				return
			}
		}
	}
	if ts := t.findTombLocked(u.TraceID, u.RequestedAt); ts != nil {
		ts.attempts++
		t.stats.lateAttached++
		span := t.attemptSpanLocked(&u, ts.trace, newSpanID(), ts.root, ts.attempts)
		if ts.exported && t.cfg.active() {
			t.enqueueLocked(span)
		}
		if t.cfg.uiActive() {
			t.store.addLateAttempt(ts.trace, &span)
		}
		return
	}
	b := t.bucketLocked(u.TraceID)
	if len(b.orphans) >= maxOrphansPerTrace {
		t.stats.overflow++
		return
	}
	b.orphans = append(b.orphans, pendingUsage{u: u, expires: now.Add(t.cfg.OrphanTimeout)})
}

func (t *tracer) bucketLocked(key string) *bucket {
	if b := t.pending[key]; b != nil {
		return b
	}
	for len(t.pending) >= t.cfg.MaxPendingTraces && t.order.Len() > 0 {
		t.evictOldestLocked()
	}
	b := &bucket{key: key}
	b.elem = t.order.PushBack(b)
	t.pending[key] = b
	return b
}

// evictOldestLocked exports the oldest buffered trace early to stay within
// max_pending_traces. The data is not lost unless the export queue is also full.
func (t *tracer) evictOldestLocked() {
	front := t.order.Front()
	if front == nil {
		return
	}
	b := front.Value.(*bucket)
	t.stats.evicted++
	t.flushBucketLocked(b, t.now())
	t.removeBucketLocked(b)
}

func (t *tracer) removeBucketLocked(b *bucket) {
	if b.elem != nil {
		t.order.Remove(b.elem)
		b.elem = nil
	}
	delete(t.pending, b.key)
}

// flushBucketLocked exports every completion and orphan in b regardless of deadlines.
func (t *tracer) flushBucketLocked(b *bucket, now time.Time) {
	for _, pc := range b.comps {
		t.emitCompletionLocked(pc, now)
	}
	b.comps = nil
	if len(b.orphans) > 0 {
		usages := make([]usageRecord, 0, len(b.orphans))
		for _, o := range b.orphans {
			usages = append(usages, o.u)
		}
		t.emitOrphansLocked(b.key, usages)
	}
	b.orphans = nil
}

// sweep moves traces whose grace or orphan timeout elapsed into the export
// queue. It is driven by the single background loop (or directly by tests).
func (t *tracer) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range t.pending {
		keep := b.comps[:0]
		for _, pc := range b.comps {
			if !now.Before(pc.deadline) {
				t.emitCompletionLocked(pc, now)
			} else {
				keep = append(keep, pc)
			}
		}
		clear(b.comps[len(keep):])
		b.comps = keep
		var expired []usageRecord
		kept := b.orphans[:0]
		for _, o := range b.orphans {
			if !now.Before(o.expires) {
				expired = append(expired, o.u)
			} else {
				kept = append(kept, o)
			}
		}
		clear(b.orphans[len(kept):])
		b.orphans = kept
		if len(expired) > 0 {
			t.emitOrphansLocked(b.key, expired)
		}
		if len(b.comps) == 0 && len(b.orphans) == 0 {
			t.removeBucketLocked(b)
		}
	}
	t.pruneTombsLocked(now)
}

// flushPending exports every buffered trace immediately (quiesce/shutdown).
func (t *tracer) flushPending() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, b := range t.pending {
		t.flushBucketLocked(b, now)
		t.removeBucketLocked(b)
	}
}

func (t *tracer) emitCompletionLocked(pc *pendingCompletion, now time.Time) {
	usages := pc.usages
	if len(usages) == 0 && pc.c.Model == "" && pc.c.RequestedModel == "" && pc.c.Outcome == outcomeSucceeded {
		// Non-LLM endpoints (e.g. /v1/models listings) also emit completions;
		// they carry no model and no upstream attempt, so they are not traced.
		t.stats.skipped++
		return
	}
	sortAttempts(usages)
	tid, root := newTraceID(), newSpanID()
	exported := t.exportLocked(nonEmpty(pc.c.TraceID, pc.c.RequestID))
	t.emitTraceLocked(tid, buildRootSpan(&pc.c, usages, tid, root, now, t.cfg.CaptureErrorBody), usages, root, exported)
	if pc.c.TraceID == "" {
		return
	}
	start, end := pc.c.StartedAt, pc.c.CompletedAt
	if end.IsZero() {
		end = now
	}
	ts := &tombstone{key: pc.c.TraceID, trace: tid, root: root, start: start, end: end, attempts: len(usages), expires: now.Add(t.cfg.OrphanTimeout), exported: exported}
	t.tombs[ts.key] = append(t.tombs[ts.key], ts)
	t.tombQ = append(t.tombQ, ts)
	t.pruneTombsLocked(now)
}

func (t *tracer) emitOrphansLocked(key string, usages []usageRecord) {
	sortAttempts(usages)
	tid, root := newTraceID(), newSpanID()
	t.stats.syntheticRoots++
	t.emitTraceLocked(tid, buildSyntheticRootSpan(key, usages, tid, root), usages, root, t.exportLocked(key))
}

// emitTraceLocked hands one finalized trace to its consumers: the export
// queue (when sampled in) and the Traces page store. Both receive the same
// sanitized spans.
func (t *tracer) emitTraceLocked(tid traceID, rootSpan otlpSpan, usages []usageRecord, root spanID, exported bool) {
	ui := t.cfg.uiActive()
	if !exported && !ui {
		return
	}
	attempts := make([]otlpSpan, len(usages))
	for i := range usages {
		attempts[i] = t.attemptSpanLocked(&usages[i], tid, newSpanID(), root, i+1)
	}
	if exported {
		t.enqueueLocked(rootSpan)
		for i := range attempts {
			t.enqueueLocked(attempts[i])
		}
	}
	if ui {
		t.store.add(newUITrace(tid, &rootSpan, attempts))
	}
}

// attemptSpanLocked builds one attempt span. With export_account_labels it
// also carries cpa.account, resolved from the cached host.auth.list snapshot
// (or the auth type + short index fallback); the host is never called here.
func (t *tracer) attemptSpanLocked(u *usageRecord, tid traceID, sid, parent spanID, ordinal int) otlpSpan {
	span := buildAttemptSpan(u, tid, sid, parent, ordinal, t.cfg.CaptureErrorBody)
	if t.cfg.ExportAccountLabels {
		if label := accountLabel(t.accounts.snapshot(), u.AuthIndex, u.AuthType); label != "" {
			a := attrs(span.Attributes)
			a.str("cpa.account", label)
			span.Attributes = a
		}
	}
	return span
}

func (t *tracer) findTombLocked(key string, at time.Time) *tombstone {
	entries := t.tombs[key]
	for i := len(entries) - 1; i >= 0; i-- {
		ts := entries[i]
		if ts.dead {
			continue
		}
		if at.IsZero() || ts.start.IsZero() ||
			(!at.Before(ts.start.Add(-windowSlack)) && !at.After(ts.end.Add(windowSlack))) {
			return ts
		}
	}
	return nil
}

// pruneTombsLocked expires tombstones after orphan_timeout_ms and caps them at
// max_pending_traces entries.
func (t *tracer) pruneTombsLocked(now time.Time) {
	for len(t.tombQ) > 0 {
		head := t.tombQ[0]
		if now.Before(head.expires) && len(t.tombQ) <= t.cfg.MaxPendingTraces {
			break
		}
		t.tombQ[0] = nil
		t.tombQ = t.tombQ[1:]
		head.dead = true
		entries := t.tombs[head.key]
		kept := entries[:0]
		for _, ts := range entries {
			if !ts.dead {
				kept = append(kept, ts)
			}
		}
		clear(entries[len(kept):])
		if len(kept) == 0 {
			delete(t.tombs, head.key)
		} else {
			t.tombs[head.key] = kept
		}
	}
}

func (t *tracer) enqueueLocked(s otlpSpan) {
	if t.queue.push(s) {
		t.stats.queueDropped++
	}
}

func (t *tracer) queueLen() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.queue.len()
}

func (t *tracer) pendingLen() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

func (t *tracer) resourceLocked() otlpResource {
	var a attrs
	a.str("service.name", t.cfg.ServiceName)
	a.str("service.version", pluginVersion)
	a.str("telemetry.sdk.name", pluginID)
	a.str("telemetry.sdk.language", "go")
	a.str("telemetry.sdk.version", pluginVersion)
	return otlpResource{Attributes: a}
}

func encodeBatch(resource otlpResource, spans []otlpSpan) ([]byte, error) {
	return json.Marshal(otlpExportRequest{ResourceSpans: []otlpResourceSpans{{
		Resource:   resource,
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: pluginID, Version: pluginVersion}, Spans: spans}},
	}}})
}

// flushOnce exports at most one batch. It returns the number of spans taken
// from the queue (0 when empty) and the export error, if any. Failed batches
// are dropped and counted rather than re-queued, keeping memory bounded.
func (t *tracer) flushOnce(ctx context.Context) (int, error) {
	if !t.exp.acquire(ctx) {
		return 0, ctx.Err()
	}
	defer t.exp.release()
	t.mu.Lock()
	if !t.cfg.active() {
		t.mu.Unlock()
		return 0, nil
	}
	batch := t.queue.pop(t.cfg.MaxBatchSpans)
	resource := t.resourceLocked()
	t.mu.Unlock()
	if len(batch) == 0 {
		return 0, nil
	}
	body, err := encodeBatch(resource, batch)
	var rejected int64
	if err == nil {
		rejected, err = t.exp.send(ctx, body)
	}
	t.mu.Lock()
	if err != nil {
		t.stats.exportDropped += uint64(len(batch))
		t.lastExportErr = truncateItemError(sanitizeErrorText(err.Error()))
	} else {
		rejected = min(max(rejected, 0), int64(len(batch)))
		t.stats.rejected += uint64(rejected)
		t.stats.exported += uint64(int64(len(batch)) - rejected)
		t.lastExportErr, t.lastExportAt = "", t.now()
	}
	t.mu.Unlock()
	if err != nil {
		t.logLimited("export", "warn", fmt.Sprintf("llm-tracing: export of %d spans failed and was dropped: %v", len(batch), err))
	}
	return len(batch), err
}

// flushAll drains the queue until it is empty or ctx expires. Spans still
// queued when ctx expires are dropped and counted.
func (t *tracer) flushAll(ctx context.Context) {
	for ctx.Err() == nil {
		if n, _ := t.flushOnce(ctx); n == 0 {
			break
		}
	}
	if ctx.Err() != nil {
		t.mu.Lock()
		t.stats.exportDropped += uint64(t.queue.clear())
		t.mu.Unlock()
	}
}

// logLimited logs at most once per logInterval per key. Must not be called with mu held.
func (t *tracer) logLimited(key, level, message string) {
	t.mu.Lock()
	now := t.now()
	last, seen := t.lastLog[key]
	if seen && now.Sub(last) < logInterval {
		t.mu.Unlock()
		return
	}
	t.lastLog[key] = now
	logger := t.logger
	t.mu.Unlock()
	logger(level, message)
}

// reportStats logs drop counters accumulated since the last report, at most
// once per logInterval.
func (t *tracer) reportStats(now time.Time) {
	t.mu.Lock()
	if now.Sub(t.lastStat) < logInterval {
		t.mu.Unlock()
		return
	}
	cur, prev := t.stats, t.reported
	t.reported, t.lastStat = cur, now
	logger := t.logger
	t.mu.Unlock()
	queue, export, evicted, overflow, rejected := cur.queueDropped-prev.queueDropped, cur.exportDropped-prev.exportDropped,
		cur.evicted-prev.evicted, cur.overflow-prev.overflow, cur.rejected-prev.rejected
	if queue+export+overflow+rejected+evicted == 0 {
		return
	}
	logger("warn", fmt.Sprintf("llm-tracing: last minute: dropped_queue_full=%d dropped_export_failed=%d dropped_overflow=%d rejected_by_backend=%d evicted_early=%d exported=%d",
		queue, export, overflow, rejected, evicted, cur.exported-prev.exported))
}

func (t *tracer) snapshot() counters {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

func newTraceID() traceID {
	var id traceID
	for id == (traceID{}) {
		_, _ = rand.Read(id[:])
	}
	return id
}

func newSpanID() spanID {
	var id spanID
	for id == (spanID{}) {
		_, _ = rand.Read(id[:])
	}
	return id
}
