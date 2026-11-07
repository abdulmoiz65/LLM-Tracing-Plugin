package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// mgmtGet calls the management handler like the host does for an
// authenticated GET under apiBase and returns status, headers and body.
func mgmtGet(t *testing.T, p *pluginState, endpoint string, query map[string]string) (int, map[string][]string, []byte) {
	t.Helper()
	q := map[string][]string{}
	for k, v := range query {
		q[k] = []string{v}
	}
	resp := p.handleManagement(managementRequest{Method: http.MethodGet, Path: apiBase + endpoint, Query: q})
	return resp.StatusCode, resp.Headers, resp.Body
}

func uiPlugin(t *testing.T, clock *fakeClock) *pluginState {
	t.Helper()
	p, _ := newTestPlugin(t, clock)
	if _, err := p.dispatch("plugin.register", registerFixture("enabled: true\nui_max_traces: 100\n", 10)); err != nil {
		t.Fatal(err)
	}
	return p
}

func decodeTraces(t *testing.T, body []byte) []traceItemJSON {
	t.Helper()
	var out struct {
		Traces []traceItemJSON `json:"traces"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Traces == nil {
		t.Fatalf("decode traces: %v %s", err, body)
	}
	return out.Traces
}

func TestManagementRegisterShape(t *testing.T) {
	clock := newFakeClock()
	p, _ := newTestPlugin(t, clock)
	result, err := p.dispatch("management.register", []byte(`{"Plugin":{"Name":"LLM Tracing"},"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/llm-tracing"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Decode the envelope the way the host does (rpcManagementRegistrationResponse
	// with pluginapi.ManagementRoute / ResourceRoute: Go field names).
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Routes []struct {
				Method, Path, Menu, Description string
			} `json:"routes"`
			Resources []struct {
				Path, Menu, Description string
			} `json:"resources"`
		} `json:"result"`
	}
	raw := encodeEnvelope(result, nil)
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %s", raw)
	}
	var routes []string
	for _, r := range env.Result.Routes {
		if r.Method != "GET" || r.Menu != "" {
			t.Fatalf("route must be a plain GET without menu: %+v", r)
		}
		routes = append(routes, r.Path)
	}
	if strings.Join(routes, ",") != "/v0/management/plugins/llm-tracing/summary,/v0/management/plugins/llm-tracing/traces,/v0/management/plugins/llm-tracing/trace,/v0/management/plugins/llm-tracing/sessions,/v0/management/plugins/llm-tracing/insights,/v0/management/plugins/llm-tracing/whatif" {
		t.Fatalf("routes: %v", routes)
	}
	res := env.Result.Resources
	if len(res) != 4 || res[3].Path != "/logo.png" || res[3].Menu != "" || res[0].Path != "/dashboard" || res[0].Menu != "LLM Tracing" || res[0].Description != "Requests, sessions, cost, insights and tokens" ||
		res[1].Path != "/dashboard.css" || res[2].Path != "/dashboard.js" || res[1].Menu != "" || res[2].Menu != "" {
		t.Fatalf("resources: %+v", res)
	}
	if !strings.Contains(string(raw), `"Path":"/dashboard.css"}`) {
		t.Fatalf("empty Menu/Description must be omitted: %s", raw)
	}
}

// The host sends rpcManagementRequest: embedded pluginapi.ManagementRequest
// (Method, Path, Headers http.Header, Query url.Values, Body []byte as Base64)
// plus host_callback_id, and decodes the result into pluginapi.ManagementResponse.
func TestManagementHandleHostShapedRoundTrip(t *testing.T) {
	clock := newFakeClock()
	p := uiPlugin(t, clock)
	saved := plug
	plug = p
	t.Cleanup(func() { plug = saved })
	_, _ = dispatch("request.complete", []byte(completionFixture))
	_, _ = dispatch("usage.handle", []byte(usageFixture))
	finalize(p.tracer, clock)

	request := `{"Method":"GET","Path":"/v0/management/plugins/llm-tracing/traces",` +
		`"Headers":{"Authorization":["Bearer mgmt-key"],"Accept":["application/json"]},` +
		`"Query":{"limit":["5"],"status":["ok"]},"Body":null,"host_callback_id":"cb-42"}`
	result, err := dispatch("management.handle", []byte(request))
	if err != nil {
		t.Fatal(err)
	}
	raw := encodeEnvelope(result, nil)
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int
			Headers    map[string][]string
			Body       []byte
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %s", raw)
	}
	if !strings.Contains(string(raw), `"Body":"`+base64.StdEncoding.EncodeToString(env.Result.Body)+`"`) {
		t.Fatal("body must travel as Base64 ([]byte)")
	}
	h := env.Result.Headers
	if env.Result.StatusCode != 200 || h["Content-Type"][0] != "application/json; charset=utf-8" || h["Cache-Control"][0] != "no-store" || h["X-Content-Type-Options"][0] != "nosniff" {
		t.Fatalf("response: %d %v", env.Result.StatusCode, h)
	}
	traces := decodeTraces(t, env.Result.Body)
	if len(traces) != 1 || traces[0].CPATraceID != "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" || traces[0].RequestedModel != "mock-alias" ||
		traces[0].Outcome != "succeeded" || traces[0].StatusCode != 200 || traces[0].InputTokens != 42 || traces[0].CachedTokens != 11 ||
		traces[0].SourceFormat != "openai" || !traces[0].Stream || traces[0].DurationMS != 2000 || traces[0].StartedAt != "2026-11-02T10:00:00Z" {
		t.Fatalf("item: %+v", traces[0])
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(traces[0].TraceID) || traces[0].ClientKey != maskClientKey("e2e-client-key-0001") {
		t.Fatalf("ids: %+v", traces[0])
	}

	// A body-less request decodes, and a malformed one is an RPC error.
	if _, err := dispatch("management.handle", []byte(`{"Method":"GET","Path":"/v0/management/plugins/llm-tracing/summary"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch("management.handle", []byte(`{not json`)); err == nil {
		t.Fatal("malformed management request must error")
	}
}

func TestTraceListFiltersAndLimit(t *testing.T) {
	clock := newFakeClock()
	p := uiPlugin(t, clock)
	tr := p.tracer
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("f%02d", i)
		start := baseTime.Add(time.Duration(i) * time.Second)
		c := completionAt(id, start, start.Add(time.Duration(i%3)*6*time.Second), outcomeSucceeded, 200) // 0s, 6s, 12s
		if i%10 == 0 {
			c.Outcome, c.StatusCode = "failed", 500
		}
		if i%2 == 0 {
			c.RequestedModel = "GPT-5-Codex"
		}
		tr.addCompletion(c)
	}
	finalize(tr, clock)

	cases := []struct {
		query map[string]string
		want  int
	}{
		{nil, 50},
		{map[string]string{"limit": "500"}, 60},
		{map[string]string{"limit": "99999"}, 60},
		{map[string]string{"limit": "0"}, 1},
		{map[string]string{"limit": "-3"}, 1},
		{map[string]string{"limit": "100", "status": "error"}, 6},
		{map[string]string{"limit": "100", "status": "OK"}, 54},
		{map[string]string{"limit": "100", "status": "all"}, 60},
		{map[string]string{"limit": "100", "model": "codex"}, 30},
		{map[string]string{"limit": "100", "model": "  SONNET "}, 30},
		{map[string]string{"limit": "100", "min_ms": "10000"}, 20},
		{map[string]string{"limit": "100", "min_ms": "-5"}, 60},
		{map[string]string{"limit": "100", "min_ms": "10000", "status": "error", "model": "gpt"}, 2}, // f20, f50
	}
	for _, tc := range cases {
		status, _, body := mgmtGet(t, p, "/traces", tc.query)
		if status != 200 {
			t.Fatalf("%v: status %d %s", tc.query, status, body)
		}
		if got := len(decodeTraces(t, body)); got != tc.want {
			t.Fatalf("%v: got %d traces, want %d", tc.query, got, tc.want)
		}
	}
	_, _, body := mgmtGet(t, p, "/traces", map[string]string{"limit": "3"})
	items := decodeTraces(t, body)
	if items[0].CPATraceID != "f59" || items[2].CPATraceID != "f57" {
		t.Fatalf("not newest first: %s %s", items[0].CPATraceID, items[2].CPATraceID)
	}
	for _, bad := range []map[string]string{{"limit": "ten"}, {"status": "broken"}, {"min_ms": "slow"}, {"min_ms": "NaN"}} {
		status, _, body := mgmtGet(t, p, "/traces", bad)
		var e map[string]string
		if status != 400 || json.Unmarshal(body, &e) != nil || e["error"] == "" {
			t.Fatalf("%v: want 400 with error, got %d %s", bad, status, body)
		}
	}
}

func TestTraceDetailStatuses(t *testing.T) {
	clock := newFakeClock()
	p := uiPlugin(t, clock)
	tr := p.tracer
	tr.addCompletion(completionAt("d", baseTime, baseTime.Add(2*time.Second), "failed", 502))
	bad := usageAt("d", "a1", baseTime.Add(100*time.Millisecond), 200*time.Millisecond)
	bad.Failed, bad.Failure = true, usageFailure{StatusCode: 503, Body: "overloaded"}
	good := usageAt("d", "a2", baseTime.Add(400*time.Millisecond), time.Second)
	good.TTFT = 250 * time.Millisecond
	good.Stream = true
	tr.addUsage(good)
	tr.addUsage(bad)
	finalize(tr, clock)
	_, _, body := mgmtGet(t, p, "/traces", nil)
	id := decodeTraces(t, body)[0].TraceID

	status, headers, body := mgmtGet(t, p, "/trace", map[string]string{"id": strings.ToUpper(id)})
	if status != 200 || headers["Content-Type"][0] != contentTypeJSON {
		t.Fatalf("detail: %d %s", status, body)
	}
	var out struct {
		Trace struct {
			traceItemJSON
			Spans []struct {
				SpanID        string  `json:"span_id"`
				ParentSpanID  string  `json:"parent_span_id"`
				Name          string  `json:"name"`
				Kind          string  `json:"kind"`
				Status        string  `json:"status"`
				StartOffsetMS float64 `json:"start_offset_ms"`
				DurationMS    float64 `json:"duration_ms"`
				Attributes    map[string]any
				Events        []struct {
					Name     string
					OffsetMS float64 `json:"offset_ms"`
				}
			} `json:"spans"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	// encoding/json matches the remaining fields case-insensitively.
	tr0 := out.Trace
	if tr0.TraceID != id || tr0.AttemptCount != 2 || tr0.FailedAttempts != 1 || tr0.Error != "overloaded" || len(tr0.Spans) != 3 {
		t.Fatalf("trace: %+v", tr0.traceItemJSON)
	}
	root, first, second := tr0.Spans[0], tr0.Spans[1], tr0.Spans[2]
	if root.Kind != "server" || root.ParentSpanID != "" || root.StartOffsetMS != 0 || root.DurationMS != 2000 || root.Status != "error" {
		t.Fatalf("root: %+v", root)
	}
	if first.Kind != "client" || first.ParentSpanID != root.SpanID || first.StartOffsetMS != 100 || first.DurationMS != 200 || first.Status != "error" ||
		first.Attributes["http.response.status_code"] != float64(503) || first.Attributes["cpa.error"] != "overloaded" {
		t.Fatalf("failed attempt: %+v", first)
	}
	if second.Status != "unset" || second.StartOffsetMS != 400 || second.Attributes["cpa.stream"] != true || second.Attributes["gen_ai.provider.name"] != "claude" ||
		len(second.Events) != 1 || second.Events[0].Name != "first_token" || second.Events[0].OffsetMS != 650 {
		t.Fatalf("ok attempt: %+v", second)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(second.SpanID) || second.SpanID == first.SpanID || root.SpanID == "" || first.Events == nil {
		t.Fatalf("span ids / events: %+v", first)
	}
	for _, s := range tr0.Spans {
		for k := range s.Attributes {
			if strings.HasPrefix(k, "langfuse.") {
				t.Fatalf("langfuse mapping attribute stored: %s", k)
			}
		}
	}

	for _, badID := range []string{"", "xyz", strings.Repeat("a", 31), strings.Repeat("g", 32), strings.Repeat("0", 32), id + "00"} {
		status, _, body := mgmtGet(t, p, "/trace", map[string]string{"id": badID})
		if status != 400 || !strings.Contains(string(body), `"error"`) {
			t.Fatalf("id %q: want 400, got %d %s", badID, status, body)
		}
	}
	status, _, body = mgmtGet(t, p, "/trace", map[string]string{"id": strings.Repeat("ab", 16)})
	if status != 404 || string(body) != `{"error":"trace not found"}` {
		t.Fatalf("missing trace: %d %s", status, body)
	}
	if status, _, _ := mgmtGet(t, p, "/nope", nil); status != 404 {
		t.Fatalf("unknown endpoint: %d", status)
	}
	if resp := p.handleManagement(managementRequest{Method: "POST", Path: apiBase + "/traces"}); resp.StatusCode != 405 {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
}

func TestUIDisabledHidesAPI(t *testing.T) {
	clock := newFakeClock()
	p, _ := newTestPlugin(t, clock)
	if _, err := p.dispatch("plugin.register", registerFixture("enabled: true\nui_enabled: false\nendpoint: http://127.0.0.1:1/v1/traces\n", 10)); err != nil {
		t.Fatal(err)
	}
	p.tracer.addCompletion(completionAt("x", baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200))
	finalize(p.tracer, clock)
	if p.tracer.store.len() != 0 {
		t.Fatal("page disabled: nothing may be stored")
	}
	for _, endpoint := range []string{"/summary", "/traces", "/trace"} {
		status, _, body := mgmtGet(t, p, endpoint, nil)
		if status != 404 || !strings.Contains(string(body), "ui_enabled") {
			t.Fatalf("%s: %d %s", endpoint, status, body)
		}
	}
}

func TestResourcesServeNoTraceDataWithSecurityHeaders(t *testing.T) {
	clock := newFakeClock()
	p := uiPlugin(t, clock)
	c := completionAt("cpa-secret-trace-id", baseTime, baseTime.Add(time.Second), outcomeSucceeded, 200)
	c.RequestedModel = "distinctive-model-name"
	p.tracer.addCompletion(c)
	finalize(p.tracer, clock)
	if p.tracer.store.len() != 1 {
		t.Fatal("setup: trace not stored")
	}
	wantType := map[string]string{
		"dashboard":     "text/html; charset=utf-8",
		"dashboard.css": "text/css; charset=utf-8",
		"dashboard.js":  "text/javascript; charset=utf-8",
	}
	for name, ct := range wantType {
		resp := p.handleManagement(managementRequest{Method: "GET", Path: "/v0/resource/plugins/llm-tracing/" + name})
		h := resp.Headers
		if resp.StatusCode != 200 || h["Content-Type"][0] != ct || h["X-Content-Type-Options"][0] != "nosniff" || h["Referrer-Policy"][0] != "no-referrer" || h["Cache-Control"][0] != "no-store" {
			t.Fatalf("%s: %d %v", name, resp.StatusCode, h)
		}
		csp := h["Content-Security-Policy"][0]
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "connect-src 'self'", "object-src 'none'", "frame-ancestors 'self'"} {
			if !strings.Contains(csp, want) {
				t.Fatalf("%s: CSP %q lacks %q", name, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe") {
			t.Fatalf("%s: CSP allows unsafe sources: %q", name, csp)
		}
		body := string(resp.Body)
		if len(body) == 0 || strings.Contains(body, "distinctive-model-name") || strings.Contains(body, "cpa-secret-trace-id") {
			t.Fatalf("%s must be static and contain no trace data", name)
		}
	}
	// Any plugin ID prefix works (the ID is the library file name).
	if resp := p.handleManagement(managementRequest{Method: "GET", Path: "/v0/resource/plugins/renamed/dashboard"}); resp.StatusCode != 200 {
		t.Fatalf("renamed plugin: %d", resp.StatusCode)
	}
	// Unauthenticated resource paths never reach the trace API.
	for _, path := range []string{"/v0/resource/plugins/llm-tracing/traces", "/v0/resource/plugins/llm-tracing/summary", "/v0/resource/plugins/llm-tracing/x/dashboard", "/v0/resource/plugins/llm-tracing"} {
		resp := p.handleManagement(managementRequest{Method: "GET", Path: path})
		if resp.StatusCode != 404 || strings.Contains(string(resp.Body), "distinctive-model-name") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, resp.Body)
		}
	}
}

func TestHostileModelNameStaysJSONString(t *testing.T) {
	clock := newFakeClock()
	p := uiPlugin(t, clock)
	hostile := `<img src=x onerror=alert(1)>"</script>&`
	c := completionAt("xss", baseTime, baseTime.Add(time.Second), "failed", 500)
	c.RequestedModel = hostile
	c.Error = `<script>alert("err")</script>`
	p.tracer.addCompletion(c)
	finalize(p.tracer, clock)
	for _, endpoint := range []string{"/traces", "/summary"} {
		status, h, body := mgmtGet(t, p, endpoint, nil)
		if status != 200 || h["Content-Type"][0] != contentTypeJSON {
			t.Fatalf("%s: %d", endpoint, status)
		}
		if strings.ContainsAny(string(body), "<>") {
			t.Fatalf("%s: raw markup characters in JSON body: %s", endpoint, body)
		}
	}
	_, _, body := mgmtGet(t, p, "/traces", nil)
	items := decodeTraces(t, body)
	if items[0].RequestedModel != hostile || items[0].Error != `<script>alert("err")</script>` {
		t.Fatalf("value must round-trip as a plain string: %+v", items[0])
	}
	_, _, detail := mgmtGet(t, p, "/trace", map[string]string{"id": items[0].TraceID})
	if strings.ContainsAny(string(detail), "<>") {
		t.Fatalf("detail: raw markup characters: %s", detail)
	}
}

func TestDashboardAssetsAreStaticAndSafe(t *testing.T) {
	js := string(dashboardJS)
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "new Function", "document.write", "setAttribute('style'", "localStorage", "http://", "https://"} {
		if strings.Contains(js, banned) {
			t.Fatalf("dashboard.js must not use %q", banned)
		}
	}
	if !strings.Contains(js, "credentials: 'omit'") || !strings.Contains(js, "cache: 'no-store'") {
		t.Fatal("dashboard.js fetch must omit credentials and bypass the cache")
	}
	// The only external URL allowed is the author link (an <a href> the
	// user clicks, never something the page loads). Remove that exact
	// element before checking for any other external URL.
	const authorLink = `<a class="byline" href="https://github.com/abdulmoiz65" target="_blank" rel="noopener noreferrer">by Abdul Moiz</a>`
	if strings.Count(string(dashboardHTML), authorLink) != 1 {
		t.Fatal("author link changed: keep it a plain link with rel=noopener noreferrer")
	}
	html := strings.Replace(string(dashboardHTML), authorLink, "", 1)
	if !strings.Contains(html, `<img class="brand-logo" src="./logo.png"`) {
		t.Fatal("the header logo must be the plugin's own resource")
	}
	if regexp.MustCompile(`<script(\s[^>]*)?>\s*[^<\s]`).MatchString(html) || !strings.Contains(html, `<script src="./dashboard.js" defer></script>`) {
		t.Fatal("dashboard.html must not contain inline script")
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=|\sstyle\s*=|<form|javascript:|https?://`).MatchString(html) {
		t.Fatal("dashboard.html must not contain inline handlers, inline styles, forms or external URLs")
	}
	if regexp.MustCompile(`https?://|@import|url\(`).MatchString(string(dashboardCSS)) {
		t.Fatal("dashboard.css must not load external resources")
	}
}

// The Management Center overlays a toolbar on the page's top-right corner,
// so the page must reserve that area and keep its controls left-aligned.
func TestDashboardReservesHostToolbarCorner(t *testing.T) {
	html, css := string(dashboardHTML), string(dashboardCSS)
	if strings.Contains(html, "top-actions") || !strings.Contains(html, `<header class="masthead">`) ||
		!regexp.MustCompile(`(?s)<header class="masthead">.*id="connbar".*id="disconnect".*</header>`).MatchString(html) {
		t.Fatal("connection controls must live in the left-aligned masthead")
	}
	m := regexp.MustCompile(`--host-safe-right:\s*(\d+)px`).FindStringSubmatch(css)
	if n := 0; m == nil || func() bool { _, err := fmt.Sscan(m[1], &n); return err != nil }() || n < 240 {
		t.Fatalf("masthead must reserve at least 240px on the right: %v", m)
	}
	for _, want := range []string{"padding-right: var(--host-safe-right)", "min-height: calc(var(--host-safe-top)", "scroll-margin-top", "justify-content: flex-start"} {
		if !strings.Contains(css, want) {
			t.Fatalf("dashboard.css lacks %q", want)
		}
	}
	if strings.Contains(css, "justify-content: space-between") || strings.Contains(css, "justify-content: flex-end") {
		t.Fatal("no control may be pushed to the right edge")
	}
	for _, col := range []string{">Provider<", ">Account<", ">Cache<", ">Tokens in / out<"} {
		if !strings.Contains(html, col) {
			t.Fatalf("table column %s missing", col)
		}
	}
}
