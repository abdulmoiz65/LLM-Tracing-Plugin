package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

var baseTime = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// fakeClock is a manually advanced clock; tests never sleep on the wall clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: baseTime.Add(time.Minute)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type capturedRequest struct {
	header http.Header
	body   []byte
}

// collector is a fake OTLP/HTTP endpoint. statuses scripts the response codes
// of successive requests (200 once exhausted).
type collector struct {
	mu       sync.Mutex
	requests []capturedRequest
	statuses []int
	headers  []http.Header
	server   *httptest.Server
}

func newCollector(t *testing.T, statuses ...int) *collector {
	t.Helper()
	c := &collector{statuses: statuses}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.requests = append(c.requests, capturedRequest{header: r.Header.Clone(), body: body})
		status := http.StatusOK
		var extra http.Header
		if len(c.statuses) > 0 {
			status = c.statuses[0]
			c.statuses = c.statuses[1:]
		}
		if len(c.headers) > 0 {
			extra = c.headers[0]
			c.headers = c.headers[1:]
		}
		c.mu.Unlock()
		for k, v := range extra {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *collector) endpoint() string { return c.server.URL + "/v1/traces" }

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func (c *collector) bodies() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, 0, len(c.requests))
	for _, r := range c.requests {
		out = append(out, r.body)
	}
	return out
}

// spans decodes every successfully delivered request.
func (c *collector) spans(t *testing.T) []otlpSpan {
	t.Helper()
	var out []otlpSpan
	for _, body := range c.bodies() {
		var req otlpExportRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode export body: %v", err)
		}
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}

func testConfig(endpoint string) config {
	cfg := defaultConfig()
	cfg.Endpoint = endpoint
	return cfg
}

func newTestTracer(t *testing.T, cfg config, clock *fakeClock) *tracer {
	t.Helper()
	tr := newTracer(cfg, clock.Now)
	tr.exp.sleep = func(context.Context, time.Duration) error { return nil }
	return tr
}

// exportNow flushes everything queued to the collector and returns all spans
// the collector has received so far.
func exportNow(t *testing.T, tr *tracer, c *collector) []otlpSpan {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr.flushAll(ctx)
	return c.spans(t)
}

func attr(s otlpSpan, key string) (otlpAnyValue, bool) {
	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return otlpAnyValue{}, false
}

func strAttr(t *testing.T, s otlpSpan, key string) string {
	t.Helper()
	v, ok := attr(s, key)
	if !ok || v.StringValue == nil {
		t.Fatalf("span %q: missing string attribute %s", s.Name, key)
	}
	return *v.StringValue
}

func intAttr(t *testing.T, s otlpSpan, key string) string {
	t.Helper()
	v, ok := attr(s, key)
	if !ok || v.IntValue == nil {
		t.Fatalf("span %q: missing int attribute %s", s.Name, key)
	}
	return *v.IntValue
}

func hasAttr(s otlpSpan, key string) bool {
	_, ok := attr(s, key)
	return ok
}

func rootsAndChildren(spans []otlpSpan) (roots, children []otlpSpan) {
	for _, s := range spans {
		if s.ParentSpanID == "" {
			roots = append(roots, s)
		} else {
			children = append(children, s)
		}
	}
	return roots, children
}

func completionAt(traceID string, start, end time.Time, outcome string, status int) requestCompletion {
	return requestCompletion{
		RequestID:      "req-" + traceID,
		TraceID:        traceID,
		SourceFormat:   "openai",
		Model:          "claude-sonnet-4-5",
		RequestedModel: "sonnet",
		Outcome:        outcome,
		StatusCode:     status,
		StartedAt:      start,
		CompletedAt:    end,
	}
}

func usageAt(traceID, requestID string, at time.Time, latency time.Duration) usageRecord {
	return usageRecord{
		RequestID:   requestID,
		TraceID:     traceID,
		Provider:    "claude",
		Model:       "claude-sonnet-4-5",
		Alias:       "sonnet",
		RequestedAt: at,
		Latency:     latency,
		Detail:      usageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
}
