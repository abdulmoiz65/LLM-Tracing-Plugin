package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestExporterRetriesOn503ThenSucceeds(t *testing.T) {
	c := newCollector(t, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusOK)
	c.headers = []http.Header{nil, {"Retry-After": {"30"}}}
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	var sleeps []time.Duration
	tr.exp.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }

	tr.addUsage(usageAt("", "u1", baseTime, time.Second))
	n, err := tr.flushOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("flush n=%d err=%v", n, err)
	}
	if c.count() != 3 {
		t.Fatalf("want 3 attempts, got %d", c.count())
	}
	if len(sleeps) != 2 || sleeps[0] != baseBackoff || sleeps[1] != maxRetryAfter {
		t.Fatalf("backoff wrong (Retry-After must be capped): %v", sleeps)
	}
	if s := tr.snapshot(); s.exported != 1 || s.exportDropped != 0 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestExporterGivesUpAfterMaxRetries(t *testing.T) {
	c := newCollector(t, 503, 503, 503, 503)
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig(c.endpoint()), clock)
	var logs []string
	tr.logger = func(_, m string) { logs = append(logs, m) }
	tr.addUsage(usageAt("", "u1", baseTime, time.Second))
	if _, err := tr.flushOnce(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if c.count() != 1+maxExportRetries || tr.snapshot().exportDropped != 1 {
		t.Fatalf("attempts=%d dropped=%d", c.count(), tr.snapshot().exportDropped)
	}
	// A second failure within a minute is not logged again.
	tr.addUsage(usageAt("", "u2", baseTime, time.Second))
	_, _ = tr.flushOnce(context.Background())
	if len(logs) != 1 {
		t.Fatalf("export failures not rate limited: %d logs", len(logs))
	}
}

func TestExporterDoesNotRetry400AndNeverLogsHeaderValues(t *testing.T) {
	c := newCollector(t, http.StatusBadRequest)
	clock := newFakeClock()
	cfg := testConfig(c.endpoint() + "?token=querysecret")
	cfg.Headers = map[string]string{"Authorization": "Basic c2VjcmV0OnNlY3JldA==", "x-honeycomb-team": "hc-secret"}
	tr := newTestTracer(t, cfg, clock)
	var logs []string
	tr.logger = func(_, m string) { logs = append(logs, m) }
	tr.addUsage(usageAt("", "u1", baseTime, time.Second))
	_, err := tr.flushOnce(context.Background())
	if err == nil || c.count() != 1 {
		t.Fatalf("400 must not be retried: attempts=%d err=%v", c.count(), err)
	}
	got := c.requests[0].header
	if got.Get("Authorization") != "Basic c2VjcmV0OnNlY3JldA==" || got.Get("X-Honeycomb-Team") != "hc-secret" || got.Get("Content-Type") != "application/json" {
		t.Fatalf("headers not sent: %v", got)
	}
	joined := strings.Join(logs, "\n") + err.Error()
	for _, secret := range []string{"c2VjcmV0", "hc-secret", "querysecret"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("log leaked %q: %s", secret, joined)
		}
	}
}

func TestExporterNetworkErrorRetried(t *testing.T) {
	clock := newFakeClock()
	tr := newTestTracer(t, testConfig("http://127.0.0.1:1/v1/traces"), clock)
	calls := 0
	tr.exp.sleep = func(context.Context, time.Duration) error { calls++; return nil }
	tr.addUsage(usageAt("", "u1", baseTime, time.Second))
	if _, err := tr.flushOnce(context.Background()); err == nil {
		t.Fatal("expected network error")
	}
	if calls != maxExportRetries {
		t.Fatalf("network errors should be retried %d times, slept %d", maxExportRetries, calls)
	}
}

func TestExporterPartialSuccessCounted(t *testing.T) {
	clock := newFakeClock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":"1","errorMessage":"bad span"}}`))
	}))
	defer srv.Close()
	tr := newTestTracer(t, testConfig(srv.URL), clock)
	tr.addUsage(usageAt("", "u1", baseTime, time.Second))
	tr.addUsage(usageAt("", "u2", baseTime, time.Second))
	_, _ = tr.flushOnce(context.Background())
	if s := tr.snapshot(); s.rejected != 1 || s.exported != 1 {
		t.Fatalf("partial success not counted: %+v", s)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if parseRetryAfter("3", now) != 3*time.Second || parseRetryAfter("", now) != 0 || parseRetryAfter("junk", now) != 0 {
		t.Fatal("delta-seconds parsing wrong")
	}
	if got := parseRetryAfter(now.Add(2*time.Second).Format(http.TimeFormat), now); got != 2*time.Second {
		t.Fatalf("http-date parsing: %v", got)
	}
}

var (
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)
	digit = regexp.MustCompile(`^[0-9]+$`)
)

// TestOTLPJSONShape validates the wire encoding against the OTLP/JSON mapping
// by decoding into generic maps (independent of the encoder's own structs).
func TestOTLPJSONShape(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	cfg := testConfig(c.endpoint())
	cfg.ServiceName = "svc"
	tr := newTestTracer(t, cfg, clock)
	failed := usageAt("trace-j", "u1", baseTime.Add(10*time.Millisecond), 100*time.Millisecond)
	failed.Failed, failed.Failure = true, usageFailure{StatusCode: 500, Body: "boom"}
	ok := usageAt("trace-j", "u2", baseTime.Add(200*time.Millisecond), time.Second)
	ok.TTFT = 100 * time.Millisecond
	tr.addUsage(failed)
	tr.addUsage(ok)
	tr.addCompletion(completionAt("trace-j", baseTime, baseTime.Add(2*time.Second), outcomeSucceeded, 200))
	clock.Advance(3 * time.Second)
	tr.sweep(clock.Now())
	exportNow(t, tr, c)
	if c.requests[0].header.Get("Content-Type") != "application/json" {
		t.Fatal("content type")
	}

	var doc map[string]any
	if err := json.Unmarshal(c.bodies()[0], &doc); err != nil {
		t.Fatal(err)
	}
	resourceSpans := doc["resourceSpans"].([]any)
	rs := resourceSpans[0].(map[string]any)
	resAttrs := attrMap(t, rs["resource"].(map[string]any)["attributes"])
	if resAttrs["service.name"] != "svc" || resAttrs["telemetry.sdk.name"] != "llm-tracing" || resAttrs["service.version"] != pluginVersion {
		t.Fatalf("resource attributes: %v", resAttrs)
	}
	ss := rs["scopeSpans"].([]any)[0].(map[string]any)
	if ss["scope"].(map[string]any)["name"] != "llm-tracing" {
		t.Fatal("scope name")
	}
	spans := ss["spans"].([]any)
	if len(spans) != 3 {
		t.Fatalf("want 3 spans, got %d", len(spans))
	}
	allowed := map[string]bool{"traceId": true, "spanId": true, "parentSpanId": true, "name": true, "kind": true,
		"startTimeUnixNano": true, "endTimeUnixNano": true, "attributes": true, "events": true, "status": true}
	for _, raw := range spans {
		s := raw.(map[string]any)
		for k := range s {
			if !allowed[k] {
				t.Fatalf("unexpected span field %q", k)
			}
		}
		if !hex32.MatchString(s["traceId"].(string)) || !hex16.MatchString(s["spanId"].(string)) {
			t.Fatalf("ids not lowercase hex: %v %v", s["traceId"], s["spanId"])
		}
		if p, ok := s["parentSpanId"]; ok && !hex16.MatchString(p.(string)) {
			t.Fatalf("parent id: %v", p)
		}
		for _, k := range []string{"startTimeUnixNano", "endTimeUnixNano"} {
			if v, ok := s[k].(string); !ok || !digit.MatchString(v) {
				t.Fatalf("%s must be a decimal string, got %T %v", k, s[k], s[k])
			}
		}
		if _, ok := s["kind"].(float64); !ok {
			t.Fatalf("kind must be an integer enum")
		}
		status := s["status"].(map[string]any)
		if code, ok := status["code"]; ok {
			if _, isNum := code.(float64); !isNum {
				t.Fatalf("status.code must be an integer")
			}
		}
		for _, a := range s["attributes"].([]any) {
			kv := a.(map[string]any)
			if _, ok := kv["key"].(string); !ok {
				t.Fatal("attribute key")
			}
			value := kv["value"].(map[string]any)
			if len(value) != 1 {
				t.Fatalf("AnyValue must have exactly one field: %v", value)
			}
			for vk, vv := range value {
				switch vk {
				case "stringValue":
					_, ok := vv.(string)
					assertTrue(t, ok, "stringValue type")
				case "intValue":
					str, ok := vv.(string)
					assertTrue(t, ok && regexp.MustCompile(`^-?[0-9]+$`).MatchString(str), "intValue must be a decimal string")
				case "boolValue":
					_, ok := vv.(bool)
					assertTrue(t, ok, "boolValue type")
				case "doubleValue":
					_, ok := vv.(float64)
					assertTrue(t, ok, "doubleValue type")
				default:
					t.Fatalf("unknown AnyValue field %q", vk)
				}
			}
		}
		if events, ok := s["events"].([]any); ok {
			ev := events[0].(map[string]any)
			if !digit.MatchString(ev["timeUnixNano"].(string)) || ev["name"] != "first_token" {
				t.Fatalf("event shape: %v", ev)
			}
		}
	}
}

func assertTrue(t *testing.T, ok bool, msg string) {
	t.Helper()
	if !ok {
		t.Fatal(msg)
	}
}

func attrMap(t *testing.T, raw any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, a := range raw.([]any) {
		kv := a.(map[string]any)
		for _, v := range kv["value"].(map[string]any) {
			out[kv["key"].(string)] = v
		}
	}
	return out
}
