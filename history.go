package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The request history is saved as JSON Lines, one file per UTC day
// (traces-YYYYMMDD.jsonl), in data_dir. Each line is one stored trace with
// the same sanitized data the page shows: masked client key, redacted error
// text, no prompts, responses, raw keys or account labels. A trace updated
// by a late usage record is written again; the last line for a trace wins.
//
// Writes never happen on the request path: the store hands encoded lines to
// enqueue (non-blocking, bounded), and the plugin's background loop calls
// flush once per tick and on quiesce/shutdown.

const (
	historyFilePrefix = "traces-"
	historyFileSuffix = ".jsonl"
	historyRecordV    = 1
	// maxPendingHistoryLines bounds lines waiting for the next flush.
	maxPendingHistoryLines = 20_000
	// maxHistoryLineBytes bounds one record when reading.
	maxHistoryLineBytes = 1 << 20
)

type historyFile struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	pending [][]byte
	dropped uint64

	// Guarded by ioMu (flush and prune run from one goroutine at a time).
	ioMu      sync.Mutex
	file      *os.File
	fileDay   string
	prunedDay string
	days      int

	statMu   sync.Mutex
	lastErr  string
	written  uint64
	loaded   int // records read from disk
	imported int // of those, not already in memory
	loading  bool
	skipped  int
}

func newHistoryFile(dir string, days int, now func() time.Time) *historyFile {
	if now == nil {
		now = time.Now
	}
	return &historyFile{dir: dir, days: days, now: now}
}

// enqueue is the store's persist hook: it never blocks and never does I/O.
func (h *historyFile) enqueue(line []byte) {
	h.mu.Lock()
	if len(h.pending) >= maxPendingHistoryLines {
		h.dropped++
	} else {
		h.pending = append(h.pending, line)
	}
	h.mu.Unlock()
}

func (h *historyFile) setErr(err error) {
	h.statMu.Lock()
	if err == nil {
		h.lastErr = ""
	} else {
		h.lastErr = err.Error()
	}
	h.statMu.Unlock()
}

// flush appends pending lines to today's file and prunes old files once a
// day.
func (h *historyFile) flush() {
	h.mu.Lock()
	lines := h.pending
	h.pending = nil
	h.mu.Unlock()

	h.ioMu.Lock()
	defer h.ioMu.Unlock()
	today := h.now().UTC().Format("20060102")
	if h.prunedDay != today {
		h.prunedDay = today
		if err := h.pruneLocked(); err != nil {
			h.setErr(err)
		}
	}
	if len(lines) == 0 {
		return
	}
	if h.file == nil || h.fileDay != today {
		h.closeLocked()
		if err := os.MkdirAll(h.dir, 0o700); err != nil {
			h.setErr(fmt.Errorf("create %s: %w", h.dir, err))
			h.requeue(lines)
			return
		}
		f, err := os.OpenFile(filepath.Join(h.dir, historyFilePrefix+today+historyFileSuffix), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			h.setErr(fmt.Errorf("open history file: %w", err))
			h.requeue(lines)
			return
		}
		h.file, h.fileDay = f, today
	}
	w := bufio.NewWriterSize(h.file, 64<<10)
	for _, line := range lines {
		_, _ = w.Write(line)
		_ = w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		h.setErr(fmt.Errorf("write history file: %w", err))
		h.closeLocked()
		return
	}
	h.statMu.Lock()
	h.written += uint64(len(lines))
	h.lastErr = ""
	h.statMu.Unlock()
}

// requeue puts lines back after a failed open, keeping the bound.
func (h *historyFile) requeue(lines [][]byte) {
	h.mu.Lock()
	room := maxPendingHistoryLines - len(h.pending)
	if room < len(lines) {
		h.dropped += uint64(len(lines) - max(room, 0))
		lines = lines[:max(room, 0)]
	}
	h.pending = append(lines, h.pending...)
	h.mu.Unlock()
}

func (h *historyFile) closeLocked() {
	if h.file != nil {
		_ = h.file.Close()
		h.file, h.fileDay = nil, ""
	}
}

// close flushes and closes the file.
func (h *historyFile) close() {
	h.flush()
	h.ioMu.Lock()
	h.closeLocked()
	h.ioMu.Unlock()
}

// historyFiles lists history files with their day, oldest first.
func historyFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		day, ok := strings.CutPrefix(name, historyFilePrefix)
		if !ok || e.IsDir() {
			continue
		}
		day, ok = strings.CutSuffix(day, historyFileSuffix)
		if !ok || len(day) != 8 {
			continue
		}
		if _, errDay := time.Parse("20060102", day); errDay != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// pruneLocked deletes files older than the history window.
func (h *historyFile) pruneLocked() error {
	names, err := historyFiles(h.dir)
	if err != nil {
		return err
	}
	oldest := h.now().UTC().AddDate(0, 0, -h.days).Format("20060102")
	var errs []error
	for _, name := range names {
		day := strings.TrimSuffix(strings.TrimPrefix(name, historyFilePrefix), historyFileSuffix)
		if day < oldest {
			if errRemove := os.Remove(filepath.Join(h.dir, name)); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				errs = append(errs, errRemove)
			}
		}
	}
	return errors.Join(errs...)
}

// load reads the history window from disk. The last record per trace wins.
// Malformed lines are skipped and counted.
func (h *historyFile) load() ([]*uiTrace, error) {
	names, err := historyFiles(h.dir)
	if err != nil {
		return nil, err
	}
	cutoff := h.now().Add(-time.Duration(h.days) * 24 * time.Hour).UnixNano()
	oldestDay := h.now().UTC().AddDate(0, 0, -h.days).Format("20060102")
	byID := map[traceID]*uiTrace{}
	skipped := 0
	for _, name := range names {
		day := strings.TrimSuffix(strings.TrimPrefix(name, historyFilePrefix), historyFileSuffix)
		if day < oldestDay {
			continue
		}
		f, errOpen := os.Open(filepath.Join(h.dir, name))
		if errOpen != nil {
			return nil, errOpen
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), maxHistoryLineBytes)
		for sc.Scan() {
			t, ok := decodeTrace(sc.Bytes())
			if !ok {
				skipped++
				continue
			}
			if t.start >= cutoff {
				byID[t.tid] = t
			}
		}
		errScan := sc.Err()
		_ = f.Close()
		if errScan != nil {
			skipped++ // a line over the limit ends this file; keep what was read
		}
	}
	out := make([]*uiTrace, 0, len(byID))
	for _, t := range byID {
		out = append(out, t)
	}
	h.statMu.Lock()
	h.skipped = skipped
	h.statMu.Unlock()
	return out, nil
}

type historyStatus struct {
	Enabled  bool   `json:"enabled"`
	Dir      string `json:"dir"`
	Days     int    `json:"days"`
	Loading  bool   `json:"loading"`
	Loaded   int    `json:"loaded_requests"`
	Imported int    `json:"imported_requests"`
	Skipped  int    `json:"skipped_lines"`
	Written  uint64 `json:"written_records"`
	Dropped  uint64 `json:"dropped_records"`
	Error    string `json:"last_error"`
}

func (h *historyFile) status() historyStatus {
	if h == nil {
		return historyStatus{}
	}
	h.mu.Lock()
	dropped := h.dropped
	h.mu.Unlock()
	h.statMu.Lock()
	defer h.statMu.Unlock()
	return historyStatus{Enabled: true, Dir: h.dir, Days: h.days, Loading: h.loading, Loaded: h.loaded, Imported: h.imported,
		Skipped: h.skipped, Written: h.written, Dropped: dropped, Error: h.lastErr}
}

// --- record encoding ------------------------------------------------------

type histAttr struct {
	K string  `json:"k"`
	T uint8   `json:"t,omitempty"`
	S string  `json:"s,omitempty"`
	N int64   `json:"n,omitempty"`
	F float64 `json:"f,omitempty"`
	B bool    `json:"b,omitempty"`
}

type histEvent struct {
	Name string `json:"n"`
	At   int64  `json:"at"`
}

type histSpan struct {
	ID     string      `json:"id"`
	Parent string      `json:"p,omitempty"`
	Name   string      `json:"name"`
	Kind   int         `json:"kind"`
	Start  int64       `json:"s"`
	End    int64       `json:"e"`
	Failed bool        `json:"f,omitempty"`
	Attrs  []histAttr  `json:"a,omitempty"`
	Events []histEvent `json:"ev,omitempty"`
}

type histUsage struct {
	Model    string   `json:"m,omitempty"`
	Provider string   `json:"p,omitempty"`
	AuthType string   `json:"a,omitempty"`
	Tok      [7]int64 `json:"t"` // totalInput, uncached, cacheRead, cacheWrite, output, reasoning, input
}

type histRecord struct {
	V             int         `json:"v"`
	TID           string      `json:"id"`
	CPATraceID    string      `json:"cpa,omitempty"`
	Start         int64       `json:"s"`
	End           int64       `json:"e"`
	Model         string      `json:"model,omitempty"`
	SourceFormat  string      `json:"src,omitempty"`
	Outcome       string      `json:"out,omitempty"`
	ClientKey     string      `json:"key,omitempty"`
	RootErr       string      `json:"err,omitempty"`
	LastFail      string      `json:"lfail,omitempty"`
	Stream        bool        `json:"stream,omitempty"`
	StatusCode    int         `json:"status,omitempty"`
	Attempts      int         `json:"att,omitempty"`
	Failed        int         `json:"fail,omitempty"`
	Tokens        [8]int64    `json:"tok"` // input, output, cached, totalInput, cacheRead, cacheWrite, reasoning, (reserved)
	Provider      string      `json:"prov,omitempty"`
	AuthIndex     string      `json:"ai,omitempty"`
	AuthType      string      `json:"at,omitempty"`
	ServingStart  int64       `json:"ss,omitempty"`
	ServingFailed bool        `json:"sf,omitempty"`
	HasServing    bool        `json:"hs,omitempty"`
	ServingModel  string      `json:"sm,omitempty"`
	ServingPrompt int64       `json:"sp,omitempty"`
	ServingOut    int64       `json:"so,omitempty"`
	Usage         []histUsage `json:"u,omitempty"`
	Spans         []histSpan  `json:"sp2,omitempty"`
}

// encodeTrace renders one history line. Called with store.mu held.
func encodeTrace(t *uiTrace) []byte {
	r := histRecord{
		V: historyRecordV, TID: t.tid.String(), CPATraceID: t.cpaTraceID, Start: t.start, End: t.end,
		Model: t.model, SourceFormat: t.sourceFormat, Outcome: t.outcome, ClientKey: t.clientKey,
		RootErr: t.rootErr, LastFail: t.lastFail, Stream: t.stream, StatusCode: t.statusCode,
		Attempts: t.attempts, Failed: t.failed,
		Tokens:   [8]int64{t.input, t.output, t.cached, t.totalInput, t.cacheRead, t.cacheWrite, t.reasoning},
		Provider: t.provider, AuthIndex: t.authIndex, AuthType: t.authType,
		ServingStart: t.servingStart, ServingFailed: t.servingFailed, HasServing: t.hasServing,
		ServingModel: t.servingModel, ServingPrompt: t.servingPrompt, ServingOut: t.servingOut,
	}
	for _, u := range t.usage {
		k := u.tok
		r.Usage = append(r.Usage, histUsage{Model: u.model, Provider: u.provider, AuthType: u.authType,
			Tok: [7]int64{k.totalInput, k.uncached, k.cacheRead, k.cacheWrite, k.output, k.reasoning, k.input}})
	}
	for _, s := range t.spans {
		hs := histSpan{ID: s.spanID, Parent: s.parentID, Name: s.name, Kind: s.kind, Start: s.start, End: s.end, Failed: s.failed}
		for _, a := range s.attrs {
			hs.Attrs = append(hs.Attrs, histAttr{K: a.key, T: a.kind, S: a.s, N: a.n, F: a.f, B: a.b})
		}
		for _, e := range s.events {
			hs.Events = append(hs.Events, histEvent{Name: e.name, At: e.at})
		}
		r.Spans = append(r.Spans, hs)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return line
}

// clampText bounds a string read back from disk.
func clampText(s string, limit int) string {
	if len(s) > limit {
		return truncateUTF8(s, limit)
	}
	return s
}

// decodeTrace parses and validates one history line; the file may have been
// edited or truncated, so every field is bounded.
func decodeTrace(line []byte) (*uiTrace, bool) {
	var r histRecord
	if err := json.Unmarshal(line, &r); err != nil || r.V != historyRecordV {
		return nil, false
	}
	tid, ok := parseTraceIDParam(r.TID)
	if !ok || r.Start <= 0 || r.End < 0 || r.Attempts < 0 || r.Failed < 0 {
		return nil, false
	}
	for _, n := range r.Tokens {
		if n < 0 {
			return nil, false
		}
	}
	str := func(s string) string { return clampText(s, maxStoredAttrBytes) }
	t := &uiTrace{
		tid: tid, cpaTraceID: str(r.CPATraceID), start: r.Start, end: max(r.End, r.Start),
		model: str(r.Model), sourceFormat: str(r.SourceFormat), outcome: nonEmpty(str(r.Outcome), "unknown"),
		clientKey: str(r.ClientKey), rootErr: clampText(r.RootErr, maxItemErrorBytes), lastFail: clampText(r.LastFail, maxItemErrorBytes),
		stream: r.Stream, statusCode: r.StatusCode, attempts: r.Attempts, failed: r.Failed,
		input: r.Tokens[0], output: r.Tokens[1], cached: r.Tokens[2], totalInput: r.Tokens[3],
		cacheRead: r.Tokens[4], cacheWrite: r.Tokens[5], reasoning: r.Tokens[6],
		provider: str(r.Provider), authIndex: str(r.AuthIndex), authType: str(r.AuthType),
		servingStart: r.ServingStart, servingFailed: r.ServingFailed, hasServing: r.HasServing,
		servingModel: str(r.ServingModel), servingPrompt: max(r.ServingPrompt, 0), servingOut: max(r.ServingOut, 0),
	}
	for i, u := range r.Usage {
		if i >= maxStoredUsage {
			break
		}
		valid := true
		for _, n := range u.Tok {
			valid = valid && n >= 0
		}
		if !valid {
			return nil, false
		}
		t.usage = append(t.usage, attemptUsage{model: str(u.Model), provider: str(u.Provider), authType: str(u.AuthType),
			tok: spanTokens{totalInput: u.Tok[0], uncached: u.Tok[1], cacheRead: u.Tok[2], cacheWrite: u.Tok[3],
				output: u.Tok[4], reasoning: u.Tok[5], input: u.Tok[6]}})
	}
	if len(r.Spans) > 0 && len(r.Spans) <= maxStoredAttempts+1 {
		t.hasSpans = true
		for _, hs := range r.Spans {
			s := uiSpan{spanID: str(hs.ID), parentID: str(hs.Parent), name: str(hs.Name), kind: hs.Kind,
				start: hs.Start, end: hs.End, failed: hs.Failed}
			for i, a := range hs.Attrs {
				if i >= 256 {
					break
				}
				s.attrs = append(s.attrs, uiAttr{key: str(a.K), kind: a.T, s: str(a.S), n: a.N, f: a.F, b: a.B})
			}
			for i, e := range hs.Events {
				if i >= 64 {
					break
				}
				s.events = append(s.events, uiEvent{name: str(e.Name), at: e.At})
			}
			t.spans = append(t.spans, s)
		}
	}
	return t, true
}
