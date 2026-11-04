package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// synthetic builds an OTLP JSON export the way the plugin is expected to emit it.
func synthetic(scenarios []scenario, leak string) []byte {
	return syntheticWith(scenarios, leak, nil)
}

// syntheticWith also appends extra attributes to every attempt span.
func syntheticWith(scenarios []scenario, leak string, extra []map[string]any) []byte {
	type kv = map[string]any
	str := func(k, v string) kv { return kv{"key": k, "value": kv{"stringValue": v}} }
	num := func(k string, v int) kv { return kv{"key": k, "value": kv{"intValue": fmt.Sprint(v)}} }
	var spans []any
	t0 := int64(1_700_000_000_000_000_000)
	for i, s := range scenarios {
		traceID := fmt.Sprintf("%032x", i+1)
		rootID := fmt.Sprintf("%016x", 1000+i)
		start := t0 + int64(i)*1e9
		rootCode := 1
		type att struct{ code, http int }
		var atts []att
		switch s.Kind {
		case "success":
			atts = []att{{1, 200}}
		case "retry":
			atts = []att{{2, 429}, {1, 200}}
		case "fail":
			atts = []att{{2, 429}, {2, 500}}
			rootCode = 2
		}
		rootAttrs := []any{str(attrRequestModel, s.Model), str(attrClientKey, "e2e-...0001"+leak), str(attrOutcome, "x")}
		if s.Kind != "fail" {
			rootAttrs = append(rootAttrs, num(attrTotalInput, expectedTotalInputToks))
		}
		spans = append(spans, kv{"traceId": traceID, "spanId": rootID, "name": rootSpanPrefix + " chat", "startTimeUnixNano": fmt.Sprint(start), "endTimeUnixNano": fmt.Sprint(start + 5e6), "attributes": rootAttrs, "status": kv{"code": rootCode}})
		for j, a := range atts {
			attrs := []any{num(attrHTTPStatus, a.http), num(attrAttempt, j+1)}
			if a.code == 1 {
				attrs = append(attrs, num(attrInputTokens, 42), num(attrOutputTokens, 7), num(attrTotalInput, expectedTotalInputToks), num(attrCacheRead, expectedCachedToks))
			}
			for _, e := range extra {
				attrs = append(attrs, e)
			}
			spans = append(spans, kv{"traceId": traceID, "spanId": fmt.Sprintf("%016x", 2000+i*10+j), "parentSpanId": rootID, "name": attemptSpanPrefix, "startTimeUnixNano": fmt.Sprint(start + int64(j+1)*1e6), "endTimeUnixNano": fmt.Sprint(start + int64(j+2)*1e6), "attributes": attrs, "status": kv{"code": a.code}})
		}
	}
	payload := kv{"resourceSpans": []any{kv{"resource": kv{"attributes": []any{str(attrServiceName, expectedService)}}, "scopeSpans": []any{kv{"spans": spans}}}}}
	b, _ := json.Marshal([]any{payload})
	return b
}

func TestVerifyTracesSynthetic(t *testing.T) {
	scenarios := []scenario{
		{Name: "a", Kind: "success", Model: "mock-gpt"},
		{Name: "b", Kind: "retry", Model: "mock-retry"},
		{Name: "c", Kind: "fail", Model: "mock-fail"},
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		leak string
		ok   bool
	}{{"", true}, {"e2e-client-key-0001", false}} {
		path := filepath.Join(dir, "dump.json")
		if err := os.WriteFile(path, synthetic(scenarios, tc.leak), 0o644); err != nil {
			t.Fatal(err)
		}
		c := &checker{}
		verifyTraces(c, path, scenarios)
		if (c.failures == 0) != tc.ok {
			t.Fatalf("leak=%q: failures=%d, want ok=%v", tc.leak, c.failures, tc.ok)
		}
	}
	// An exported account label (export_account_labels defaults to false) must fail the run.
	path := filepath.Join(dir, "dump.json")
	label := map[string]any{"key": attrAccount, "value": map[string]any{"stringValue": "apikey · 0123abcd"}}
	if err := os.WriteFile(path, syntheticWith(scenarios, "", []map[string]any{label}), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &checker{}
	verifyTraces(c, path, scenarios)
	if c.failures == 0 {
		t.Fatal("exported cpa.account was not detected")
	}
}
