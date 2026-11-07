package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fullScenarios = []scenario{
	{Name: "nonstream-success", Kind: "success", Model: "mock-gpt"},
	{Name: "stream-success", Kind: "success", Model: "mock-gpt", Stream: true},
	{Name: "retry-429-then-ok", Kind: "retry", Model: "mock-retry"},
	{Name: "all-fail", Kind: "fail", Model: "mock-fail"},
	{Name: "alias-success", Kind: "success", Model: "mock-alias"},
	{Name: "stream-retry-429-then-ok", Kind: "retry", Model: "mock-retry", Stream: true},
}

// fixture describes a synthetic plugin API that satisfies the contract; tests mutate it.
type fixture struct {
	scenarios []scenario
	noExport  bool
	traces    []map[string]any // newest first
	detail    map[string]any
	summary   map[string]any
	status    map[string]int    // probe name -> HTTP status override
	bodies    map[string]string // probe name -> body override
	headers   map[string]string // probe name -> raw extra header lines
}

// IDs follow synthetic() in main_test.go: traceId %032x(i+1), root %016x(1000+i), attempt %016x(2000+i*10+j).
func newFixture(scenarios []scenario, noExport bool) *fixture {
	f := &fixture{scenarios: scenarios, noExport: noExport, status: map[string]int{}, bodies: map[string]string{}, headers: map[string]string{}}
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	var in, out, read int64
	errs, retried := 0, 0
	for i := len(scenarios) - 1; i >= 0; i-- {
		s := scenarios[i]
		a, fa := expectedAttempts(s.Kind)
		code, ti, to, cr, errMsg, provider := 200, int64(42), int64(7), int64(30), "", providerOK
		hit := expectedHitRate
		if s.Kind == "fail" {
			code, ti, to, cr, errMsg, provider, hit = 500, 0, 0, 0, "upstream 500", providerBroken, 0
			errs++
		}
		read += cr
		if a > 1 {
			retried++
		}
		in += ti
		out += to
		f.traces = append(f.traces, map[string]any{
			"trace_id": fmt.Sprintf("%032x", i+1), "cpa_trace_id": "x", "started_at": t0.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			"duration_ms": 5.5, "requested_model": s.Model, "source_format": "openai", "stream": s.Stream, "outcome": "ok",
			"status_code": code, "attempt_count": a, "failed_attempts": fa, "input_tokens": ti, "output_tokens": to,
			"cached_tokens": cr, "total_input_tokens": ti, "cache_read_tokens": cr, "cache_creation_tokens": 0, "cache_hit_rate": hit,
			"provider": provider, "account": "apikey · a72c126e", "client_key": "sha256:abcd", "error": errMsg,
		})
		if s.Name == probeRetryScenario {
			d := map[string]any{}
			for k, v := range f.traces[len(f.traces)-1] {
				d[k] = v
			}
			root := fmt.Sprintf("%016x", 1000+i)
			d["spans"] = []any{
				map[string]any{"span_id": root, "parent_span_id": "", "name": "cpa.request", "kind": "server", "start_offset_ms": 0, "duration_ms": 5, "status": "unset", "attributes": map[string]any{}, "events": []any{}},
				map[string]any{"span_id": fmt.Sprintf("%016x", 2000+i*10), "parent_span_id": root, "name": "cpa.attempt", "kind": "client", "start_offset_ms": 1, "duration_ms": 1, "status": "error",
					"provider": providerLimited, "account": "apikey · cd4fe3ce", "tokens": map[string]any{"total_input_tokens": 0},
					"attributes": map[string]any{attrHTTPStatus: 429}, "events": []any{}},
				map[string]any{"span_id": fmt.Sprintf("%016x", 2000+i*10+1), "parent_span_id": root, "name": "cpa.attempt", "kind": "client", "start_offset_ms": 2, "duration_ms": 1, "status": "unset",
					"provider": providerOK, "account": "apikey · a72c126e",
					"tokens":     map[string]any{"input_tokens": 42, "total_input_tokens": 42, "uncached_input_tokens": 12, "cache_read_tokens": 30, "cache_creation_tokens": 0, "cache_hit_rate": expectedHitRate, "output_tokens": 7},
					"attributes": map[string]any{attrHTTPStatus: 200}, "events": []any{}},
			}
			f.detail = d
		}
	}
	exported := 17
	enabled := true
	if noExport {
		exported, enabled = 0, false
	}
	n := len(scenarios)
	f.summary = map[string]any{
		"version": "0.4.1", "window_seconds": 3600, "requests": n, "errors": errs, "error_rate": float64(errs) / float64(n),
		"avg_latency_ms": 5.5, "p95_latency_ms": 9.0, "input_tokens": in, "output_tokens": out, "retried_requests": retried,
		"total_input_tokens": in, "cache_read_tokens": read, "cache_hit_rate": expectedHitRate,
		"providers":      []string{providerBroken, providerOK},
		"account_labels": map[string]any{"enabled": true, "known_credentials": oauthFixtureCredentials, "last_refresh_at": "2026-11-04T10:00:00Z", "last_error": ""},
		"stored_traces":  n, "capacity": 1000,
		"exporter": map[string]any{"enabled": enabled, "endpoint_host": "127.0.0.1:14318", "exported_spans": exported, "dropped_spans": 0, "last_error": "", "last_export_at": ""},
	}
	return f
}

func (f *fixture) filter(pred func(map[string]any) bool) string {
	var out []map[string]any
	for _, t := range f.traces {
		if pred(t) {
			out = append(out, t)
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"traces": out})
	return string(b)
}

func (f *fixture) write(t *testing.T, dir string) {
	t.Helper()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	sc := []string{}
	for i, s := range f.scenarios {
		s.StatsFile = fmt.Sprintf("stats_%d.json", i+1)
		b, _ := json.Marshal(s)
		sc = append(sc, string(b))
	}
	must(os.WriteFile(filepath.Join(dir, "scenarios.jsonl"), []byte(strings.Join(sc, "\n")+"\n"), 0o644))
	jsonOf := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	all := func(map[string]any) bool { return true }
	type pr struct{ name, auth, path, ctype, body string }
	probes := []pr{
		{probeSummary, "right", apiPrefix + "/summary", "application/json", jsonOf(f.summary)},
		{probeTraces, "right", apiPrefix + "/traces?limit=50", "application/json", f.filter(all)},
		{probeStatusError, "right", apiPrefix + "/traces?status=error", "application/json", f.filter(func(m map[string]any) bool { return m["status_code"] == 500 })},
	}
	if !f.noExport {
		unauth := `{"error":"missing management key"}`
		for _, r := range probesAuthRoutes {
			probes = append(probes, pr{r + "_noauth", "none", "/x/" + r, "application/json", unauth}, pr{r + "_wrongkey", "wrong", "/x/" + r, "application/json", unauth})
		}
		probes = append(probes,
			pr{probeStatusOK, "right", apiPrefix + "/traces?status=ok", "application/json", f.filter(func(m map[string]any) bool { return m["status_code"] != 500 })},
			pr{probeModelRetry, "right", apiPrefix + "/traces?model=mock-retry", "application/json", f.filter(func(m map[string]any) bool { return strings.Contains(m["requested_model"].(string), "mock-retry") })},
			pr{probeMinMsHuge, "right", apiPrefix + "/traces?min_ms=600000", "application/json", f.filter(func(map[string]any) bool { return false })},
			pr{probeLimit1, "right", apiPrefix + "/traces?limit=1", "application/json", jsonOf(map[string]any{"traces": f.traces[:1]})},
			pr{"traces_limit_negative", "right", apiPrefix + "/traces?limit=-5", "application/json", f.filter(all)},
			pr{"traces_limit_huge", "right", apiPrefix + "/traces?limit=99999", "application/json", f.filter(all)},
			pr{"traces_status_bogus", "right", apiPrefix + "/traces?status=bogus", "application/json", `{"error":"bad status"}`},
			pr{probeTrace, "right", apiPrefix + "/trace?id=x", "application/json", jsonOf(map[string]any{"trace": f.detail})},
			pr{probeTraceMalformed, "right", apiPrefix + "/trace?id=zz", "application/json", `{"error":"invalid id"}`},
			pr{probeTraceUnknown, "right", apiPrefix + "/trace?id=0", "application/json", `{"error":"trace not found"}`},
			pr{probeV8Summary, "right", "/v8/x", "text/plain", "404 page not found"},
			pr{probeResDashboard, "none", apiResourcePrefix + "/dashboard", "text/html; charset=utf-8", "<!doctype html><title>Traces</title>"},
			pr{probeResCSS, "none", apiResourcePrefix + "/dashboard.css", "text/css; charset=utf-8", "body{}"},
			pr{probeResJS, "none", apiResourcePrefix + "/dashboard.js", "text/javascript; charset=utf-8", "fetch('summary')"},
		)
	}
	defStatus := map[string]int{"traces_status_bogus": 400, probeTraceMalformed: 400, probeTraceUnknown: 404, probeV8Summary: 404}
	for _, p := range probes {
		status := 200
		if strings.HasSuffix(p.name, "_noauth") || strings.HasSuffix(p.name, "_wrongkey") {
			status = 401
		}
		if s, ok := defStatus[p.name]; ok {
			status = s
		}
		if s, ok := f.status[p.name]; ok {
			status = s
		}
		body := p.body
		if b, ok := f.bodies[p.name]; ok {
			body = b
		}
		hdr := fmt.Sprintf("HTTP/1.1 %d X\r\nContent-Type: %s\r\n", status, p.ctype)
		if p.name == probeResDashboard {
			hdr += "Content-Security-Policy: default-src 'self'\r\nX-Content-Type-Options: nosniff\r\n"
		}
		hdr += f.headers[p.name] + "\r\n"
		bodyFile, hdrFile := "api_"+p.name+".json", "api_"+p.name+".headers.txt"
		must(os.WriteFile(filepath.Join(dir, bodyFile), []byte(body), 0o644))
		must(os.WriteFile(filepath.Join(dir, hdrFile), []byte(hdr), 0o644))
		b, _ := json.Marshal(probe{Name: p.name, Auth: p.auth, Path: p.path, Status: status, BodyFile: bodyFile, HeadersFile: hdrFile})
		lines = append(lines, string(b))
	}
	must(os.WriteFile(filepath.Join(dir, "api_probes.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	if !f.noExport {
		// synthetic() emits 2 attempts for "fail"; only root order and retry span ids are cross-checked.
		must(os.WriteFile(filepath.Join(dir, "dump.json"), synthetic(f.scenarios, ""), 0o644))
	}
}

func runFixture(t *testing.T, f *fixture, before, after int) int {
	t.Helper()
	dir := t.TempDir()
	f.write(t, dir)
	in := apiInput{runDir: dir, noExport: f.noExport, receiverBefore: before, receiverAfter: after}
	var err error
	in.scenarios, err = readScenarios(filepath.Join(dir, "scenarios.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.noExport {
		in.otlpIDs, in.otlpSpanIDs, err = otlpRootsInOrder(filepath.Join(dir, "dump.json"))
		if err != nil {
			t.Fatal(err)
		}
	}
	c := &checker{}
	verifyAPI(c, in)
	return c.failures
}

func TestVerifyAPIFullFixturePasses(t *testing.T) {
	if n := runFixture(t, newFixture(fullScenarios, false), 0, 0); n != 0 {
		t.Fatalf("valid fixture: %d failures", n)
	}
}

func TestVerifyAPINoExportFixturePasses(t *testing.T) {
	sc := []scenario{fullScenarios[0], fullScenarios[3]}
	if n := runFixture(t, newFixture(sc, true), 3, 3); n != 0 {
		t.Fatalf("valid no-export fixture: %d failures", n)
	}
	if n := runFixture(t, newFixture(sc, true), 3, 4); n == 0 {
		t.Fatal("receiver payload during no-export run was not detected")
	}
	f := newFixture(sc, true)
	f.summary["exporter"].(map[string]any)["enabled"] = true
	if n := runFixture(t, f, 3, 3); n == 0 {
		t.Fatal("exporter.enabled=true with empty endpoint was not detected")
	}
}

func TestVerifyAPIDetectsContractViolations(t *testing.T) {
	cases := map[string]func(f *fixture){
		"leaked client key": func(f *fixture) { f.traces[0]["client_key"] = "e2e-client-key-0001" },
		"leaked mgmt key in resource": func(f *fixture) {
			f.bodies[probeResJS] = "const k='" + mgmtKey + "'"
		},
		"leaked upstream secret": func(f *fixture) { f.traces[2]["error"] = "sk-should-be-redacted-123456" },
		"wrong order":            func(f *fixture) { f.traces[0], f.traces[1] = f.traces[1], f.traces[0] },
		"wrong attempt count":    func(f *fixture) { f.traces[2]["attempt_count"] = 3 },
		"wrong trace id":         func(f *fixture) { f.traces[0]["trace_id"] = strings.Repeat("ab", 16) },
		"extra models trace": func(f *fixture) {
			f.traces = append(f.traces, map[string]any{"trace_id": strings.Repeat("cd", 16), "requested_model": "", "started_at": "2000-01-01T00:00:00Z"})
		},
		"tokens missing":        func(f *fixture) { f.traces[1]["input_tokens"] = 0 },
		"cached double counted": func(f *fixture) { f.traces[1]["total_input_tokens"] = 72 },
		"hit rate wrong":        func(f *fixture) { f.traces[1]["cache_hit_rate"] = 0.4167 },
		"provider missing":      func(f *fixture) { f.traces[0]["provider"] = "" },
		"raw account label":     func(f *fixture) { f.traces[0]["account"] = "mock-ok" },
		"same account twice": func(f *fixture) {
			f.detail["spans"].([]any)[1].(map[string]any)["account"] = "apikey · a72c126e"
		},
		"attempt account exported attr": func(f *fixture) {
			f.detail["spans"].([]any)[2].(map[string]any)["attributes"].(map[string]any)["cpa.account"] = "x"
		},
		"host auth list failed": func(f *fixture) {
			f.summary["account_labels"].(map[string]any)["last_error"] = "host callback failed"
		},
		"summary total input":    func(f *fixture) { f.summary["total_input_tokens"] = 0 },
		"noauth allowed":         func(f *fixture) { f.status[probeSummary+"_noauth"] = 200 },
		"bad param 500":          func(f *fixture) { f.status["traces_limit_huge"] = 500 },
		"unknown id 200":         func(f *fixture) { f.status[probeTraceUnknown] = 200 },
		"summary retried":        func(f *fixture) { f.summary["retried_requests"] = 2 },
		"summary version":        func(f *fixture) { f.summary["version"] = "0.1.0" },
		"exporter disabled":      func(f *fixture) { f.summary["exporter"].(map[string]any)["enabled"] = false },
		"dashboard has trace id": func(f *fixture) { f.bodies[probeResDashboard] = "<html>" + fmt.Sprintf("%032x", 1) },
		"dashboard model name":   func(f *fixture) { f.bodies[probeResDashboard] = "<html>mock-gpt" },
		"first attempt ok": func(f *fixture) {
			f.detail["spans"].([]any)[1].(map[string]any)["status"] = "ok"
		},
		"child parent wrong": func(f *fixture) {
			f.detail["spans"].([]any)[2].(map[string]any)["parent_span_id"] = "ffffffffffffffff"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(fullScenarios, false)
			mutate(f)
			if n := runFixture(t, f, 0, 0); n == 0 {
				t.Fatalf("%s: violation not detected", name)
			}
		})
	}
	t.Run("missing nosniff", func(t *testing.T) {
		dir := t.TempDir()
		f := newFixture(fullScenarios, false)
		f.write(t, dir)
		hp := filepath.Join(dir, "api_"+probeResDashboard+".headers.txt")
		if err := os.WriteFile(hp, []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		in := apiInput{runDir: dir}
		in.scenarios, _ = readScenarios(filepath.Join(dir, "scenarios.jsonl"))
		in.otlpIDs, in.otlpSpanIDs, _ = otlpRootsInOrder(filepath.Join(dir, "dump.json"))
		c := &checker{}
		verifyAPI(c, in)
		if c.failures == 0 {
			t.Fatal("missing CSP/nosniff not detected")
		}
	})
}

func TestAPITraceIDFor(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(fullScenarios, false)
	f.write(t, dir)
	id, err := apiTraceIDFor(dir, probeRetryScenario)
	if err != nil || id != fmt.Sprintf("%032x", 3) {
		t.Fatalf("apiTraceIDFor = %q, %v", id, err)
	}
}

func TestParseHeadersLastBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.txt")
	_ = os.WriteFile(p, []byte("HTTP/1.1 100 Continue\r\nX-A: 1\r\n\r\nHTTP/1.1 200 OK\r\nContent-Type: text/html\r\nX-Content-Type-Options: nosniff\r\n\r\n"), 0o644)
	h := parseHeaders(p)
	if h["x-a"] != "" || h["content-type"] != "text/html" || h["x-content-type-options"] != "nosniff" {
		t.Fatalf("headers = %v", h)
	}
}
