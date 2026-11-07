package main

// API-mode checks for the llm-tracing v0.2.0 built-in Traces page / management API.
// run.sh captures every HTTP probe into <run>/api_probes.jsonl plus one body and one header
// file per probe; this file only reads those artifacts.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Contract constants. Adjust here if the plugin's API changes.
const (
	apiPrefix          = "/v0/management/plugins/llm-tracing"
	apiResourcePrefix  = "/v0/resource/plugins/llm-tracing"
	apiExpectedVersion = "0.4.0"
	apiWindowSeconds   = 3600
	apiClientKeyPrefix = "sha256:"
	apiTraceNotFound   = "trace not found"
	apiSpanKindServer  = "server"
	apiSpanKindClient  = "client"
	apiSpanStatusError = "error"
	apiOutcomeAttr     = attrHTTPStatus // attribute checked on the 429 attempt
	apiMinExported     = 17             // 6 root spans + 11 attempt spans in the full scenario set
	mgmtKey            = "e2e-mgmt-key-0001"

	// Probe names written by run.sh (api_probes.jsonl "name" field).
	probeSummary         = "summary"
	probeTraces          = "traces"
	probeTrace           = "trace_retry"
	probeTraceMalformed  = "trace_malformed"
	probeTraceUnknown    = "trace_unknown"
	probeStatusError     = "traces_status_error"
	probeStatusOK        = "traces_status_ok"
	probeModelRetry      = "traces_model_retry"
	probeLimit1          = "traces_limit1"
	probeMinMsHuge       = "traces_minms_huge"
	probeV8Summary       = "v8_summary"
	probeResDashboard    = "res_dashboard"
	probeResCSS          = "res_css"
	probeResJS           = "res_js"
	probeModelRetryQuery = "mock-retry"
	probeRetryScenario   = "retry-429-then-ok"

	// Serving providers (CPA names openai-compatibility entries openai-compatible-<name>).
	providerOK      = "openai-compatible-mockok"
	providerLimited = "openai-compatible-mocklimited"
	providerBroken  = "openai-compatible-mockbroken"
	// expectedHitRate = cached / total input = 30/42, rounded to 4 decimals.
	expectedHitRate = 0.7143
	// oauthFixtureCredentials: run.sh writes one (disabled) OAuth auth file; config API keys are
	// not file-backed, so host.auth.list returns exactly that one entry.
	oauthFixtureCredentials = 1
)

// accountFallbackRe: config API-key credentials are not in host.auth.list, so their label is
// "<auth type> · <first 8 hex of the auth index>".
var accountFallbackRe = regexp.MustCompile(`^apikey · [0-9a-f]{8}$`)

// Probes that must not return 5xx (garbage query parameters).
var probesBadParams = []string{"traces_limit_negative", "traces_limit_huge", "traces_status_bogus"}

// Authenticated routes: run.sh sends <name>_noauth, <name>_wrongkey and <name> for each.
var probesAuthRoutes = []string{probeSummary, probeTraces, probeTrace}

// expectedAttempts returns attempt_count / failed_attempts per scenario kind under config.e2e.yaml:
// success: one call on mock-ok; retry: mock-429 then mock-ok; fail: 2 credentials x 2 rounds
// (routing.retry.request-retry=1), all failing.
func expectedAttempts(kind string) (int, int) {
	switch kind {
	case "retry":
		return 2, 1
	case "fail":
		return 4, 4
	}
	return 1, 0
}

type probe struct {
	Name        string `json:"name"`
	Auth        string `json:"auth"` // none | wrong | right
	Path        string `json:"path"`
	Status      int    `json:"status"`
	BodyFile    string `json:"body_file"`
	HeadersFile string `json:"headers_file"`

	body    []byte
	headers map[string]string
}

type apiSpanTokens struct {
	InputTokens         int64   `json:"input_tokens"`
	TotalInputTokens    int64   `json:"total_input_tokens"`
	UncachedInputTokens int64   `json:"uncached_input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	OutputTokens        int64   `json:"output_tokens"`
}

type apiSpan struct {
	SpanID        string         `json:"span_id"`
	ParentSpanID  string         `json:"parent_span_id"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	StartOffsetMs float64        `json:"start_offset_ms"`
	DurationMs    float64        `json:"duration_ms"`
	Status        string         `json:"status"`
	Provider      string         `json:"provider"`
	Account       string         `json:"account"`
	Tokens        *apiSpanTokens `json:"tokens"`
	Attributes    map[string]any `json:"attributes"`
	Events        []any          `json:"events"`
}

type apiTrace struct {
	TraceID        string    `json:"trace_id"`
	CPATraceID     string    `json:"cpa_trace_id"`
	StartedAt      string    `json:"started_at"`
	DurationMs     float64   `json:"duration_ms"`
	RequestedModel string    `json:"requested_model"`
	SourceFormat   string    `json:"source_format"`
	Stream         bool      `json:"stream"`
	Outcome        string    `json:"outcome"`
	StatusCode     int       `json:"status_code"`
	AttemptCount   int       `json:"attempt_count"`
	FailedAttempts int       `json:"failed_attempts"`
	InputTokens    int64     `json:"input_tokens"`
	OutputTokens   int64     `json:"output_tokens"`
	CachedTokens   int64     `json:"cached_tokens"`
	TotalInput     int64     `json:"total_input_tokens"`
	CacheRead      int64     `json:"cache_read_tokens"`
	CacheCreation  int64     `json:"cache_creation_tokens"`
	CacheHitRate   float64   `json:"cache_hit_rate"`
	Provider       string    `json:"provider"`
	Account        string    `json:"account"`
	ClientKey      string    `json:"client_key"`
	Error          string    `json:"error"`
	Spans          []apiSpan `json:"spans"`
}

type apiSummary struct {
	Version         string   `json:"version"`
	WindowSeconds   int      `json:"window_seconds"`
	Requests        int      `json:"requests"`
	Errors          int      `json:"errors"`
	ErrorRate       float64  `json:"error_rate"`
	AvgLatencyMs    float64  `json:"avg_latency_ms"`
	P95LatencyMs    float64  `json:"p95_latency_ms"`
	InputTokens     int64    `json:"input_tokens"`
	TotalInput      int64    `json:"total_input_tokens"`
	CacheRead       int64    `json:"cache_read_tokens"`
	CacheHitRate    float64  `json:"cache_hit_rate"`
	OutputTokens    int64    `json:"output_tokens"`
	RetriedRequests int      `json:"retried_requests"`
	Providers       []string `json:"providers"`
	AccountLabels   struct {
		Enabled          bool   `json:"enabled"`
		KnownCredentials int    `json:"known_credentials"`
		LastRefreshAt    string `json:"last_refresh_at"`
		LastError        string `json:"last_error"`
	} `json:"account_labels"`
	StoredTraces int `json:"stored_traces"`
	Capacity     int `json:"capacity"`
	Exporter     struct {
		Enabled       bool   `json:"enabled"`
		EndpointHost  string `json:"endpoint_host"`
		ExportedSpans int64  `json:"exported_spans"`
		DroppedSpans  int64  `json:"dropped_spans"`
		LastError     string `json:"last_error"`
		LastExportAt  string `json:"last_export_at"`
	} `json:"exporter"`
}

// apiInput is everything verifyAPI needs; main fills it from the run directory.
type apiInput struct {
	runDir    string
	scenarios []scenario
	// otlpIDs[i] is the OTLP traceId for scenarios[i] (nil in no-export mode).
	otlpIDs []string
	// otlpSpanIDs maps traceId -> set of exported spanIds (nil in no-export mode).
	otlpSpanIDs map[string]map[string]bool
	// upstream[i] lists the mock upstream keys called for scenarios[i] (optional).
	upstream [][]string
	// noExport: second CPA run with endpoint "" (UI works without an OTLP backend).
	noExport bool
	// receiverBefore/After: payload counts at the OTLP receiver around the no-export run.
	receiverBefore, receiverAfter int
}

func readProbes(runDir string) (map[string]*probe, error) {
	f, err := os.Open(filepath.Join(runDir, "api_probes.jsonl"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			fmt.Printf("close probes: %v\n", errClose)
		}
	}()
	out := map[string]*probe{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		p := &probe{}
		if errUnmarshal := json.Unmarshal([]byte(line), p); errUnmarshal != nil {
			return nil, fmt.Errorf("parse probe %q: %w", line, errUnmarshal)
		}
		p.body, _ = os.ReadFile(filepath.Join(runDir, p.BodyFile))
		p.headers = parseHeaders(filepath.Join(runDir, p.HeadersFile))
		out[p.Name] = p
	}
	return out, sc.Err()
}

// parseHeaders reads a curl -D dump; keys are lower-cased, the last response block wins.
func parseHeaders(path string) map[string]string {
	h := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return h
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "HTTP/") {
			h = map[string]string{}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return h
}

func decodeTraces(p *probe) ([]apiTrace, error) {
	var resp struct {
		Traces *[]apiTrace `json:"traces"`
	}
	if err := json.Unmarshal(p.body, &resp); err != nil {
		return nil, err
	}
	if resp.Traces == nil {
		return nil, fmt.Errorf("missing \"traces\" array")
	}
	return *resp.Traces, nil
}

// matchScenario finds the scenario a trace belongs to: (model, stream) is unique per scenario.
func matchScenario(scenarios []scenario, t apiTrace) int {
	for i, s := range scenarios {
		if s.Model == t.RequestedModel && s.Stream == t.Stream {
			return i
		}
	}
	return -1
}

func attrString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprint(x)
	}
	return fmt.Sprint(v)
}

func traceIDs(ts []apiTrace) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.TraceID)
	}
	return out
}

// verifyAPI runs all API-mode assertions and returns nothing; failures are counted on c.
func verifyAPI(c *checker, in apiInput) {
	mode := "full"
	if in.noExport {
		mode = "no-export (endpoint \"\")"
	}
	fmt.Printf("== Traces API (%s) ==\n", mode)
	probes, err := readProbes(in.runDir)
	if err != nil {
		c.check(false, "read api probes: %v", err)
		return
	}
	get := func(name string) *probe {
		p := probes[name]
		if p == nil {
			c.check(false, "probe %s was captured", name)
		}
		return p
	}

	// --- authentication ---
	if !in.noExport {
		for _, r := range probesAuthRoutes {
			if p := get(r + "_noauth"); p != nil {
				c.check(p.Status == 401, "%s without key -> 401 (got %d)", p.Path, p.Status)
			}
			if p := get(r + "_wrongkey"); p != nil {
				c.check(p.Status == 401 || p.Status == 403, "%s with wrong key -> 401/403 (got %d)", p.Path, p.Status)
			}
		}
	}
	for _, name := range []string{probeSummary, probeTraces} {
		if p := get(name); p != nil {
			c.check(p.Status == 200, "%s with key -> 200 (got %d)", p.Path, p.Status)
			c.check(strings.Contains(p.headers["content-type"], "application/json"), "%s Content-Type is JSON (%q)", name, p.headers["content-type"])
		}
	}

	// --- /traces ---
	n := len(in.scenarios)
	var traces []apiTrace
	byScenario := make([]*apiTrace, n)
	if p := get(probeTraces); p != nil {
		traces, err = decodeTraces(p)
		c.check(err == nil, "/traces decodes (%v)", err)
	}
	c.check(len(traces) == n, "/traces lists exactly the %d scenario requests (got %d: %v)", n, len(traces), traceIDs(traces))
	var prevStart time.Time
	for i := range traces {
		t := &traces[i]
		fmt.Printf("  trace %s model=%s stream=%v outcome=%s status=%d attempts=%d failed=%d tokens=%d/%d total_in=%d cache_read=%d hit=%.4f provider=%s account=%q dur=%.1fms key=%s cpa_trace_id=%s src=%s err=%q\n",
			t.TraceID, t.RequestedModel, t.Stream, t.Outcome, t.StatusCode, t.AttemptCount, t.FailedAttempts,
			t.InputTokens, t.OutputTokens, t.TotalInput, t.CacheRead, t.CacheHitRate, t.Provider, t.Account, t.DurationMs, t.ClientKey, t.CPATraceID, t.SourceFormat, t.Error)
		c.check(traceIDRe.MatchString(t.TraceID), "trace_id %q is 32 lowercase hex", t.TraceID)
		started, errTime := time.Parse(time.RFC3339Nano, t.StartedAt)
		c.check(errTime == nil, "trace %s started_at %q is RFC3339Nano", t.TraceID, t.StartedAt)
		if i > 0 && errTime == nil {
			c.check(!started.After(prevStart), "trace %s is not newer than its predecessor (newest first)", t.TraceID)
		}
		prevStart = started
		si := matchScenario(in.scenarios, *t)
		if si < 0 {
			c.check(false, "trace %s (model=%q stream=%v) matches a scenario", t.TraceID, t.RequestedModel, t.Stream)
			continue
		}
		if byScenario[si] != nil {
			c.check(false, "scenario %s matched by more than one trace", in.scenarios[si].Name)
		}
		byScenario[si] = t
	}
	// Newest first: position i holds scenario n-1-i.
	if len(traces) == n {
		order := true
		for i := range traces {
			if matchScenario(in.scenarios, traces[i]) != n-1-i {
				order = false
			}
		}
		c.check(order, "/traces order is newest first (reverse scenario order)")
	}
	for i, s := range in.scenarios {
		t := byScenario[i]
		if t == nil {
			c.check(false, "%s: present in /traces", s.Name)
			continue
		}
		name := s.Name
		if in.otlpIDs != nil && i < len(in.otlpIDs) {
			c.check(t.TraceID == in.otlpIDs[i], "%s: trace_id %s equals exported OTLP traceId %s", name, t.TraceID, in.otlpIDs[i])
		}
		wantA, wantF := expectedAttempts(s.Kind)
		c.check(t.AttemptCount == wantA && t.FailedAttempts == wantF, "%s: attempt_count/failed_attempts = %d/%d (got %d/%d)", name, wantA, wantF, t.AttemptCount, t.FailedAttempts)
		if in.upstream != nil && i < len(in.upstream) {
			c.check(t.AttemptCount == len(in.upstream[i]), "%s: attempt_count equals observed upstream calls %v", name, in.upstream[i])
		}
		c.check(t.DurationMs > 0, "%s: duration_ms > 0 (%.2f)", name, t.DurationMs)
		c.check(strings.HasPrefix(t.ClientKey, apiClientKeyPrefix) && !strings.Contains(t.ClientKey, "e2e-client-key-0001"), "%s: client_key is a %s fingerprint (%q)", name, apiClientKeyPrefix, t.ClientKey)
		c.check(accountFallbackRe.MatchString(t.Account), "%s: account %q is the API-key fallback label (auth type + short index)", name, t.Account)
		if s.Kind == "fail" {
			c.check(t.StatusCode == 500, "%s: status_code 500 (got %d)", name, t.StatusCode)
			c.check(t.Provider == providerBroken, "%s: provider of the last failed attempt %q (got %q)", name, providerBroken, t.Provider)
			c.check(t.TotalInput == 0 && t.CacheRead == 0 && t.CacheHitRate == 0, "%s: no tokens on failed attempts (total_in=%d cache_read=%d)", name, t.TotalInput, t.CacheRead)
		} else {
			c.check(t.StatusCode == 200, "%s: status_code 200 (got %d)", name, t.StatusCode)
			c.check(t.InputTokens == expectedInputToks && t.OutputTokens == expectedOutputToks, "%s: tokens %d/%d (got %d/%d)", name, expectedInputToks, expectedOutputToks, t.InputTokens, t.OutputTokens)
			c.check(t.TotalInput == expectedTotalInputToks && t.CacheRead == expectedCachedToks && t.CacheCreation == 0 && t.CachedTokens == expectedCachedToks,
				"%s: total_input_tokens=%d cache_read=%d cache_creation=0 cached=%d (got %d/%d/%d/%d)", name, expectedTotalInputToks, expectedCachedToks, expectedCachedToks,
				t.TotalInput, t.CacheRead, t.CacheCreation, t.CachedTokens)
			c.check(t.CacheHitRate == expectedHitRate, "%s: cache_hit_rate=%.4f (got %v)", name, expectedHitRate, t.CacheHitRate)
			c.check(t.Provider == providerOK, "%s: serving provider %q (got %q)", name, providerOK, t.Provider)
		}
	}

	idOf := func(kind string) map[string]bool {
		m := map[string]bool{}
		for i, s := range in.scenarios {
			if (kind == "" || s.Kind == kind || (kind == "ok" && s.Kind != "fail")) && byScenario[i] != nil {
				m[byScenario[i].TraceID] = true
			}
		}
		return m
	}
	filterCheck := func(probeName, desc string, want map[string]bool) {
		p := get(probeName)
		if p == nil {
			return
		}
		got, errDecode := decodeTraces(p)
		ok := p.Status == 200 && errDecode == nil && len(got) == len(want)
		for _, t := range got {
			ok = ok && want[t.TraceID]
		}
		c.check(ok, "%s %s returns exactly %d traces (status %d, got %v)", p.Path, desc, len(want), p.Status, traceIDs(got))
	}
	filterCheck(probeStatusError, "(status=error)", idOf("fail"))
	if !in.noExport {
		filterCheck(probeStatusOK, "(status=ok)", idOf("ok"))
		retryIDs := map[string]bool{}
		for i, s := range in.scenarios {
			if strings.Contains(s.Model, probeModelRetryQuery) && byScenario[i] != nil {
				retryIDs[byScenario[i].TraceID] = true
			}
		}
		filterCheck(probeModelRetry, "(model filter)", retryIDs)
		filterCheck(probeMinMsHuge, "(min_ms beyond every duration)", map[string]bool{})
		if p := get(probeLimit1); p != nil {
			got, errDecode := decodeTraces(p)
			c.check(p.Status == 200 && errDecode == nil && len(got) == 1, "%s returns 1 trace (status %d, got %d)", p.Path, p.Status, len(got))
			if len(got) == 1 && len(traces) > 0 {
				c.check(got[0].TraceID == traces[0].TraceID, "limit=1 returns the newest trace")
			}
		}
		for _, name := range probesBadParams {
			if p := get(name); p != nil {
				c.check(p.Status > 0 && p.Status < 500, "%s does not fail with 5xx (got %d)", p.Path, p.Status)
				if p.Status == 200 {
					_, errDecode := decodeTraces(p)
					c.check(errDecode == nil, "%s 200 body is a traces list (%v)", p.Path, errDecode)
				}
			}
		}
	}

	// --- /trace?id= ---
	if !in.noExport {
		verifyTraceDetail(c, in, get, byScenario)
	}

	// --- /summary ---
	verifySummary(c, in, get, traces)

	// --- v8 mirror (CPA only routes plugin management under /v0/management) ---
	if p := probes[probeV8Summary]; p != nil {
		if p.Status == 200 {
			var s apiSummary
			c.check(json.Unmarshal(p.body, &s) == nil && s.Version == apiExpectedVersion, "%s mirror returns the summary", p.Path)
		} else {
			fmt.Printf("  INFO  %s -> %d (CPA has no /v8 mirror for plugin management routes)\n", p.Path, p.Status)
		}
	}

	// --- resources ---
	if !in.noExport {
		verifyResources(c, in, get, traces)
	}

	// --- secrets ---
	verifyNoSecrets(c, in.runDir)

	// --- no-export: the receiver must not get anything ---
	if in.noExport {
		c.check(in.receiverAfter == in.receiverBefore, "OTLP receiver got no payloads during the endpoint=\"\" run (before=%d after=%d)", in.receiverBefore, in.receiverAfter)
	}
}

func verifyTraceDetail(c *checker, in apiInput, get func(string) *probe, byScenario []*apiTrace) {
	var want *apiTrace
	for i, s := range in.scenarios {
		if s.Name == probeRetryScenario {
			want = byScenario[i]
		}
	}
	if p := get(probeTraceMalformed); p != nil {
		c.check(p.Status == 400, "%s -> 400 (got %d)", p.Path, p.Status)
	}
	if p := get(probeTraceUnknown); p != nil {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(p.body, &body)
		c.check(p.Status == 404 && body.Error == apiTraceNotFound, "%s -> 404 {\"error\":%q} (got %d %s)", p.Path, apiTraceNotFound, p.Status, strings.TrimSpace(string(p.body)))
	}
	p := get(probeTrace)
	if p == nil {
		return
	}
	c.check(p.Status == 200, "%s with key -> 200 (got %d)", p.Path, p.Status)
	var resp struct {
		Trace *apiTrace `json:"trace"`
	}
	if err := json.Unmarshal(p.body, &resp); err != nil || resp.Trace == nil {
		c.check(false, "/trace body has a \"trace\" object (%v)", err)
		return
	}
	t := resp.Trace
	if want != nil {
		c.check(t.TraceID == want.TraceID, "/trace returns the requested trace %s (got %s)", want.TraceID, t.TraceID)
		c.check(t.AttemptCount == want.AttemptCount && t.FailedAttempts == want.FailedAttempts, "/trace summary fields match the /traces row")
	}
	var roots, clients []apiSpan
	for _, s := range t.Spans {
		fmt.Printf("    span %s parent=%s kind=%s name=%s status=%s off=%.1fms dur=%.1fms http=%s\n",
			s.SpanID, s.ParentSpanID, s.Kind, s.Name, s.Status, s.StartOffsetMs, s.DurationMs, attrString(s.Attributes[apiOutcomeAttr]))
		c.check(spanIDRe.MatchString(s.SpanID), "span_id %q is 16 lowercase hex", s.SpanID)
		if in.otlpSpanIDs != nil {
			c.check(in.otlpSpanIDs[t.TraceID][s.SpanID], "span %s was also exported via OTLP", s.SpanID)
		}
		switch s.Kind {
		case apiSpanKindServer:
			roots = append(roots, s)
		case apiSpanKindClient:
			clients = append(clients, s)
		default:
			c.check(false, "span %s kind %q is server|client", s.SpanID, s.Kind)
		}
	}
	c.check(len(roots) == 1 && len(clients) == 2, "retry trace has 1 server + 2 client spans (got %d + %d)", len(roots), len(clients))
	if len(roots) != 1 || len(clients) != 2 {
		return
	}
	c.check(roots[0].ParentSpanID == "", "server span has no parent (%q)", roots[0].ParentSpanID)
	for _, s := range clients {
		c.check(s.ParentSpanID == roots[0].SpanID, "client span %s parent_span_id equals root %s", s.SpanID, roots[0].SpanID)
	}
	sort.SliceStable(clients, func(i, j int) bool { return clients[i].StartOffsetMs < clients[j].StartOffsetMs })
	first, second := clients[0], clients[1]
	c.check(first.Status == apiSpanStatusError, "first attempt status %q (got %q)", apiSpanStatusError, first.Status)
	c.check(attrString(first.Attributes[apiOutcomeAttr]) == "429", "first attempt %s=429 (got %q)", apiOutcomeAttr, attrString(first.Attributes[apiOutcomeAttr]))
	c.check(second.Status != apiSpanStatusError, "second attempt status not error (got %q)", second.Status)
	fmt.Printf("    attempts: #1 provider=%s account=%q  #2 provider=%s account=%q\n", first.Provider, first.Account, second.Provider, second.Account)
	c.check(first.Provider == providerLimited && second.Provider == providerOK, "attempt providers %s then %s (got %q, %q)", providerLimited, providerOK, first.Provider, second.Provider)
	c.check(accountFallbackRe.MatchString(first.Account) && accountFallbackRe.MatchString(second.Account) && first.Account != second.Account,
		"each attempt names its own credential (got %q, %q)", first.Account, second.Account)
	if want != nil {
		c.check(t.Account == second.Account && t.Provider == second.Provider, "trace account/provider are the serving (second) attempt's (got %q %q)", t.Account, t.Provider)
	}
	for _, s := range clients {
		_, has := s.Attributes["cpa.account"]
		c.check(!has, "attempt %s attributes carry no cpa.account", s.SpanID)
	}
	if tk := second.Tokens; tk != nil {
		c.check(tk.InputTokens == expectedInputToks && tk.TotalInputTokens == expectedTotalInputToks && tk.CacheReadTokens == expectedCachedToks &&
			tk.UncachedInputTokens == expectedTotalInputToks-expectedCachedToks && tk.CacheCreationTokens == 0 && tk.OutputTokens == expectedOutputToks && tk.CacheHitRate == expectedHitRate,
			"second attempt token breakdown 42 = 12 uncached + 30 cache read, 7 out (got %+v)", *tk)
	} else {
		c.check(false, "second attempt has a tokens breakdown")
	}
	if tk := first.Tokens; tk != nil {
		c.check(tk.TotalInputTokens == 0, "failed attempt has no input tokens (got %+v)", *tk)
	} else {
		c.check(false, "first attempt has a tokens breakdown")
	}
}

func verifySummary(c *checker, in apiInput, get func(string) *probe, traces []apiTrace) {
	p := get(probeSummary)
	if p == nil {
		return
	}
	var s apiSummary
	if err := json.Unmarshal(p.body, &s); err != nil {
		c.check(false, "/summary decodes (%v)", err)
		return
	}
	fmt.Printf("  summary: %s\n", strings.TrimSpace(string(p.body)))
	n := len(in.scenarios)
	wantErrors, wantRetried := 0, 0
	var wantIn, wantOut, wantTotalIn, wantRead int64
	for _, sc := range in.scenarios {
		a, _ := expectedAttempts(sc.Kind)
		if sc.Kind == "fail" {
			wantErrors++
		} else {
			wantIn += expectedInputToks
			wantOut += expectedOutputToks
			wantTotalIn += expectedTotalInputToks
			wantRead += expectedCachedToks
		}
		// "Retried" = the request needed more than one upstream attempt, regardless of outcome.
		if a > 1 {
			wantRetried++
		}
	}
	c.check(s.Version == apiExpectedVersion, "summary version %q (got %q)", apiExpectedVersion, s.Version)
	c.check(s.WindowSeconds == apiWindowSeconds, "summary window_seconds %d (got %d)", apiWindowSeconds, s.WindowSeconds)
	c.check(s.Requests == n, "summary requests=%d (got %d)", n, s.Requests)
	c.check(s.Errors == wantErrors, "summary errors=%d (got %d)", wantErrors, s.Errors)
	if n > 0 {
		c.check(math.Abs(s.ErrorRate-float64(wantErrors)/float64(n)) < 0.01, "summary error_rate=%.3f (got %.3f)", float64(wantErrors)/float64(n), s.ErrorRate)
	}
	c.check(s.RetriedRequests == wantRetried, "summary retried_requests=%d (every request with >1 attempt; got %d)", wantRetried, s.RetriedRequests)
	c.check(s.StoredTraces == n, "summary stored_traces=%d (got %d)", n, s.StoredTraces)
	c.check(s.Capacity >= s.StoredTraces && s.Capacity > 0, "summary capacity %d >= stored_traces", s.Capacity)
	c.check(s.InputTokens == wantIn && s.OutputTokens == wantOut, "summary tokens %d/%d (got %d/%d)", wantIn, wantOut, s.InputTokens, s.OutputTokens)
	c.check(s.AvgLatencyMs > 0 && s.P95LatencyMs > 0, "summary avg/p95 latency > 0 (%.1f / %.1f)", s.AvgLatencyMs, s.P95LatencyMs)
	var sumIn int64
	for _, t := range traces {
		sumIn += t.InputTokens
	}
	c.check(sumIn == s.InputTokens, "summary input_tokens equals sum over /traces (%d)", sumIn)
	c.check(s.TotalInput == wantTotalIn && s.CacheRead == wantRead, "summary total_input_tokens=%d cache_read_tokens=%d (got %d/%d)", wantTotalIn, wantRead, s.TotalInput, s.CacheRead)
	if wantTotalIn > 0 {
		c.check(s.CacheHitRate == expectedHitRate, "summary cache_hit_rate=%.4f (got %v)", expectedHitRate, s.CacheHitRate)
	}
	providers := strings.Join(s.Providers, ",")
	c.check(strings.Contains(providers, providerOK) && !strings.Contains(providers, providerLimited), "summary providers list serving providers only (%q)", providers)
	c.check(s.AccountLabels.Enabled && s.AccountLabels.LastError == "" && s.AccountLabels.LastRefreshAt != "",
		"account labels enabled and host.auth.list succeeded (%+v)", s.AccountLabels)
	c.check(s.AccountLabels.KnownCredentials == oauthFixtureCredentials, "host.auth.list returned the %d auth file (known_credentials=%d)", oauthFixtureCredentials, s.AccountLabels.KnownCredentials)
	if in.noExport {
		c.check(!s.Exporter.Enabled, "exporter.enabled=false with endpoint \"\"")
		c.check(s.Exporter.ExportedSpans == 0, "exporter.exported_spans=0 (got %d)", s.Exporter.ExportedSpans)
		return
	}
	c.check(s.Exporter.Enabled, "exporter.enabled=true")
	c.check(s.Exporter.ExportedSpans >= apiMinExported, "exporter.exported_spans >= %d (got %d)", apiMinExported, s.Exporter.ExportedSpans)
	c.check(s.Exporter.DroppedSpans == 0, "exporter.dropped_spans=0 (got %d)", s.Exporter.DroppedSpans)
	c.check(s.Exporter.LastError == "", "exporter.last_error empty (got %q)", s.Exporter.LastError)
	c.check(strings.Contains(s.Exporter.EndpointHost, "127.0.0.1"), "exporter.endpoint_host names the receiver (%q)", s.Exporter.EndpointHost)
}

func verifyResources(c *checker, in apiInput, get func(string) *probe, traces []apiTrace) {
	needles := traceIDs(traces)
	for _, s := range in.scenarios {
		needles = append(needles, s.Model)
	}
	for _, r := range []struct{ name, ctype string }{{probeResDashboard, "text/html"}, {probeResCSS, "text/css"}, {probeResJS, "javascript"}} {
		p := get(r.name)
		if p == nil {
			continue
		}
		ct := p.headers["content-type"]
		c.check(p.Status == 200, "%s (no auth) -> 200 (got %d)", p.Path, p.Status)
		c.check(strings.Contains(ct, r.ctype), "%s Content-Type contains %q (got %q)", p.Path, r.ctype, ct)
		if r.name == probeResDashboard {
			c.check(p.headers["content-security-policy"] != "", "%s sets Content-Security-Policy (%q)", p.Path, p.headers["content-security-policy"])
			c.check(strings.EqualFold(p.headers["x-content-type-options"], "nosniff"), "%s sets X-Content-Type-Options: nosniff (got %q)", p.Path, p.headers["x-content-type-options"])
		}
		leaked := []string{}
		for _, nd := range needles {
			if nd != "" && strings.Contains(string(p.body), nd) {
				leaked = append(leaked, nd)
			}
		}
		c.check(len(leaked) == 0, "%s contains no trace ids or model names from the run (found %v)", p.Path, leaked)
	}
}

// verifyNoSecrets scans every api_* artifact (bodies and headers) for raw secrets.
func verifyNoSecrets(c *checker, runDir string) {
	files, _ := filepath.Glob(filepath.Join(runDir, "api_*"))
	for _, secret := range forbiddenStrings {
		var hits []string
		for _, f := range files {
			data, _ := os.ReadFile(f)
			if strings.Contains(string(data), secret) {
				hits = append(hits, filepath.Base(f))
			}
		}
		c.check(len(hits) == 0, "secret %q appears in no api_* response (%d files scanned, hits %v)", secret, len(files), hits)
	}
}

// runAPI is the -api entry point; it returns the process exit code.
func runAPI(runDir, dumpPath string, noExport bool, receiverBefore, receiverAfter string) int {
	scenarios, err := readScenarios(filepath.Join(runDir, "scenarios.jsonl"))
	if err != nil {
		fmt.Printf("cannot read scenarios: %v\n", err)
		return 2
	}
	in := apiInput{runDir: runDir, scenarios: scenarios, noExport: noExport, upstream: scenarioUpstreamKeys(runDir, scenarios)}
	c := &checker{}
	if noExport {
		in.receiverBefore = dumpPayloadCount(receiverBefore)
		in.receiverAfter = dumpPayloadCount(receiverAfter)
		c.check(in.receiverBefore >= 0 && in.receiverAfter >= 0, "receiver dumps readable (%s, %s)", receiverBefore, receiverAfter)
	} else {
		in.otlpIDs, in.otlpSpanIDs, err = otlpRootsInOrder(dumpPath)
		c.check(err == nil && len(in.otlpIDs) == len(scenarios), "OTLP dump has one root per scenario for cross-checking (%d, err=%v)", len(in.otlpIDs), err)
	}
	verifyAPI(c, in)
	if c.failures > 0 {
		fmt.Printf("\nAPI RESULT: FAIL (%d failed checks)\n", c.failures)
		return 1
	}
	fmt.Println("\nAPI RESULT: PASS")
	return 0
}

// otlpRootsInOrder returns root traceIds ordered by root start time plus the exported span ids per trace.
func otlpRootsInOrder(dumpPath string) ([]string, map[string]map[string]bool, error) {
	spans, _, err := loadSpans(dumpPath)
	if err != nil {
		return nil, nil, err
	}
	type root struct {
		id    string
		start int64
	}
	var roots []root
	ids := map[string]map[string]bool{}
	for _, s := range spans {
		if ids[s.TraceID] == nil {
			ids[s.TraceID] = map[string]bool{}
		}
		ids[s.TraceID][s.SpanID] = true
		if strings.HasPrefix(s.Name, rootSpanPrefix) && s.ParentSpanID == "" {
			roots = append(roots, root{s.TraceID, s.Start.int()})
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].start < roots[j].start })
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, r.id)
	}
	return out, ids, nil
}

// dumpPayloadCount returns the number of OTLP payloads in a receiver dump (-1 if unreadable).
func dumpPayloadCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	var payloads []json.RawMessage
	if json.Unmarshal(data, &payloads) != nil {
		return -1
	}
	return len(payloads)
}

// scenarioUpstreamKeys returns the mock upstream keys called by each scenario, from stats snapshots.
func scenarioUpstreamKeys(runDir string, scenarios []scenario) [][]string {
	prev := readStats(filepath.Join(runDir, "stats_0_baseline.json"))
	out := make([][]string, 0, len(scenarios))
	for _, s := range scenarios {
		cur := readStats(filepath.Join(runDir, s.StatsFile))
		var keys []string
		for _, l := range upstreamCalls(prev, cur) {
			keys = append(keys, keyOf(l))
		}
		out = append(out, keys)
		prev = cur
	}
	return out
}

// apiTraceIDFor prints the /traces trace_id belonging to a scenario (used by run.sh to build /trace?id=).
func apiTraceIDFor(runDir, scenarioName string) (string, error) {
	scenarios, err := readScenarios(filepath.Join(runDir, "scenarios.jsonl"))
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(filepath.Join(runDir, "api_traces.json"))
	if err != nil {
		return "", err
	}
	traces, err := decodeTraces(&probe{body: body})
	if err != nil {
		return "", err
	}
	for _, t := range traces {
		if i := matchScenario(scenarios, t); i >= 0 && scenarios[i].Name == scenarioName {
			return t.TraceID, nil
		}
	}
	return "", fmt.Errorf("no trace for scenario %s", scenarioName)
}
