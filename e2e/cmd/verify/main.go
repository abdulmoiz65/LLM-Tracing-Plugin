// Command verify checks the e2e harness results: scenario HTTP outcomes, mock upstream call
// patterns, and (unless -harness-only) the OTLP/HTTP JSON traces captured by otlpreceiver.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Expected names emitted by the llm-tracing plugin. Adjust here if the plugin changes.
const (
	rootSpanPrefix    = "cpa.request"
	attemptSpanPrefix = "cpa.attempt"

	attrOutcome        = "cpa.outcome"
	attrAttempt        = "cpa.attempt"
	attrRequestModel   = "gen_ai.request.model"
	attrResponseModel  = "gen_ai.response.model"
	attrInputTokens    = "gen_ai.usage.input_tokens"
	attrOutputTokens   = "gen_ai.usage.output_tokens"
	attrHTTPStatus     = "http.response.status_code"
	attrClientKey      = "cpa.client_key"
	attrProvider       = "gen_ai.system" // display only
	attrProviderAlt    = "cpa.provider"  // display only
	attrServiceName    = "service.name"  // resource attribute
	expectedService    = "cpa-e2e"
	expectedInputToks  = 42
	expectedOutputToks = 7

	statusUnset = 0
	statusOK    = 1
	statusError = 2
)

// Strings that must never appear anywhere in the trace dump.
var forbiddenStrings = []string{"e2e-client-key-0001", "sk-should-be-redacted-123456"}

var (
	traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDRe  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// scenario is one line of run/scenarios.jsonl written by run.sh.
type scenario struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // success | retry | fail
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	Status    int    `json:"status"`
	BodyFile  string `json:"body_file"`
	StatsFile string `json:"stats_file"`
}

type mockStats struct {
	ByKey map[string]int `json:"by_key"`
	Log   []string       `json:"log"`
}

// OTLP JSON structures (only what we need).
type anyValue struct {
	StringValue *string         `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue *float64        `json:"doubleValue"`
	BoolValue   *bool           `json:"boolValue"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type otlpStatus struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

type otlpSpan struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId"`
	Name         string     `json:"name"`
	Start        jsonNumStr `json:"startTimeUnixNano"`
	End          jsonNumStr `json:"endTimeUnixNano"`
	Attributes   []keyValue `json:"attributes"`
	Status       otlpStatus `json:"status"`
}

type exportRequest struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []keyValue `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []otlpSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// jsonNumStr accepts either a JSON string or number holding an integer.
type jsonNumStr string

func (j *jsonNumStr) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	*j = jsonNumStr(s)
	return nil
}

func (j jsonNumStr) int() int64 {
	v, _ := strconv.ParseInt(string(j), 10, 64)
	return v
}

type span struct {
	otlpSpan
	resource map[string]string
	attrs    map[string]string
	code     int
}

func (a anyValue) String() string {
	switch {
	case a.StringValue != nil:
		return *a.StringValue
	case len(a.IntValue) > 0:
		return strings.Trim(string(a.IntValue), `"`)
	case a.DoubleValue != nil:
		return strconv.FormatFloat(*a.DoubleValue, 'f', -1, 64)
	case a.BoolValue != nil:
		return strconv.FormatBool(*a.BoolValue)
	}
	return ""
}

func attrMap(kvs []keyValue) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value.String()
	}
	return m
}

func statusCode(raw json.RawMessage) int {
	s := strings.Trim(string(raw), `"`)
	switch strings.ToUpper(s) {
	case "", "STATUS_CODE_UNSET":
		return statusUnset
	case "STATUS_CODE_OK":
		return statusOK
	case "STATUS_CODE_ERROR":
		return statusError
	}
	v, _ := strconv.Atoi(s)
	return v
}

type checker struct {
	failures int
}

func (c *checker) check(ok bool, format string, args ...any) {
	if ok {
		fmt.Printf("  PASS  %s\n", fmt.Sprintf(format, args...))
		return
	}
	c.failures++
	fmt.Printf("  FAIL  %s\n", fmt.Sprintf(format, args...))
}

func readScenarios(path string) ([]scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []scenario
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s scenario
		if errUnmarshal := json.Unmarshal([]byte(line), &s); errUnmarshal != nil {
			return nil, fmt.Errorf("parse scenario line %q: %w", line, errUnmarshal)
		}
		out = append(out, s)
	}
	return out, nil
}

func readStats(path string) mockStats {
	var st mockStats
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.ByKey == nil {
		st.ByKey = map[string]int{}
	}
	return st
}

// upstreamCalls returns the mock chat-completion log lines added between two stats snapshots.
func upstreamCalls(prev, cur mockStats) []string {
	if len(cur.Log) < len(prev.Log) {
		return nil
	}
	var out []string
	for _, l := range cur.Log[len(prev.Log):] {
		if strings.Contains(l, "/chat/completions") {
			out = append(out, l)
		}
	}
	return out
}

func keyOf(logLine string) string {
	for _, f := range strings.Fields(logLine) {
		if strings.HasPrefix(f, "key=") {
			return strings.TrimPrefix(f, "key=")
		}
	}
	return ""
}

func verifyScenarios(c *checker, runDir string, scenarios []scenario) {
	fmt.Println("== Scenario HTTP results and upstream call pattern ==")
	prev := readStats(filepath.Join(runDir, "stats_0_baseline.json"))
	for _, s := range scenarios {
		body, _ := os.ReadFile(filepath.Join(runDir, s.BodyFile))
		cur := readStats(filepath.Join(runDir, s.StatsFile))
		calls := upstreamCalls(prev, cur)
		keys := make([]string, 0, len(calls))
		for _, l := range calls {
			keys = append(keys, keyOf(l))
		}
		prev = cur
		snippet := strings.ReplaceAll(string(body), "\n", " ")
		if len(snippet) > 220 {
			snippet = snippet[:220] + "..."
		}
		fmt.Printf("- %s (kind=%s model=%s stream=%v): HTTP %d, upstream keys in order: %v\n    body: %s\n", s.Name, s.Kind, s.Model, s.Stream, s.Status, keys, snippet)
		switch s.Kind {
		case "success":
			c.check(s.Status == 200, "%s: HTTP 200", s.Name)
			c.check(strings.Contains(string(body), "mockupstream"), "%s: body contains mock content", s.Name)
			c.check(len(keys) == 1 && keys[0] == "mock-ok", "%s: exactly one upstream call on mock-ok", s.Name)
		case "retry":
			c.check(s.Status == 200, "%s: HTTP 200 after retry", s.Name)
			c.check(strings.Contains(string(body), "mockupstream"), "%s: body contains mock content", s.Name)
			c.check(len(keys) >= 2 && keys[0] == "mock-429" && keys[len(keys)-1] == "mock-ok", "%s: first upstream call mock-429, last mock-ok (within one client request)", s.Name)
		case "fail":
			c.check(s.Status >= 400, "%s: client receives an error status (got %d)", s.Name, s.Status)
			okSeen := false
			for _, k := range keys {
				if k == "mock-ok" {
					okSeen = true
				}
			}
			c.check(len(keys) >= 1 && !okSeen, "%s: all upstream attempts failed (%d attempts, none on mock-ok)", s.Name, len(keys))
			c.check(!strings.Contains(string(body), "sk-should-be-redacted"), "%s: (info) client body does not echo the upstream fake secret", s.Name)
		}
	}
}

func loadSpans(dumpPath string) ([]span, []byte, error) {
	raw, err := os.ReadFile(dumpPath)
	if err != nil {
		return nil, nil, err
	}
	var payloads []json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &payloads); errUnmarshal != nil {
		return nil, raw, fmt.Errorf("dump is not a JSON array: %w", errUnmarshal)
	}
	var spans []span
	for i, p := range payloads {
		var req exportRequest
		if errReq := json.Unmarshal(p, &req); errReq != nil {
			return nil, raw, fmt.Errorf("payload %d: %w", i, errReq)
		}
		for _, rs := range req.ResourceSpans {
			res := attrMap(rs.Resource.Attributes)
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					spans = append(spans, span{otlpSpan: sp, resource: res, attrs: attrMap(sp.Attributes), code: statusCode(sp.Status.Code)})
				}
			}
		}
	}
	return spans, raw, nil
}

type trace struct {
	id       string
	root     *span
	attempts []*span
	others   []*span
}

func latencyMs(s *span) float64 {
	return float64(s.End.int()-s.Start.int()) / 1e6
}

func codeName(c int) string {
	switch c {
	case statusOK:
		return "OK"
	case statusError:
		return "ERROR"
	}
	return "UNSET"
}

func printSpan(prefix string, s *span) {
	provider := s.attrs[attrProviderAlt]
	if provider == "" {
		provider = s.attrs[attrProvider]
	}
	fmt.Printf("%s%s [%s] span=%s status=%s http=%s provider=%s req_model=%s resp_model=%s outcome=%s attempt=%s tokens=%s/%s client_key=%s latency=%.1fms\n",
		prefix, s.Name, codeName(s.code), s.SpanID, s.Status.Message, s.attrs[attrHTTPStatus], provider,
		s.attrs[attrRequestModel], s.attrs[attrResponseModel], s.attrs[attrOutcome], s.attrs[attrAttempt],
		s.attrs[attrInputTokens], s.attrs[attrOutputTokens], s.attrs[attrClientKey], latencyMs(s))
}

func verifyTraces(c *checker, dumpPath string, scenarios []scenario) {
	fmt.Println("== Traces ==")
	spans, raw, err := loadSpans(dumpPath)
	if err != nil {
		c.check(false, "load dump %s: %v", dumpPath, err)
		return
	}
	c.check(len(spans) > 0, "dump contains spans (%d)", len(spans))

	for _, forbidden := range forbiddenStrings {
		c.check(!strings.Contains(string(raw), forbidden), "secret %q appears nowhere in the dump", forbidden)
	}

	spanFailuresBefore := c.failures
	byTrace := map[string]*trace{}
	order := []string{}
	for i := range spans {
		s := &spans[i]
		c.quiet(traceIDRe.MatchString(s.TraceID) && s.TraceID != strings.Repeat("0", 32), "span %q traceId %q is 32 lowercase hex", s.Name, s.TraceID)
		c.quiet(spanIDRe.MatchString(s.SpanID) && s.SpanID != strings.Repeat("0", 16), "span %q spanId %q is 16 lowercase hex", s.Name, s.SpanID)
		c.quiet(s.resource[attrServiceName] == expectedService, "span %q resource service.name=%q", s.Name, s.resource[attrServiceName])
		t := byTrace[s.TraceID]
		if t == nil {
			t = &trace{id: s.TraceID}
			byTrace[s.TraceID] = t
			order = append(order, s.TraceID)
		}
		switch {
		case strings.HasPrefix(s.Name, rootSpanPrefix) && s.ParentSpanID == "":
			if t.root != nil {
				c.check(false, "trace %s has more than one root span", s.TraceID)
			}
			t.root = s
		case strings.HasPrefix(s.Name, attemptSpanPrefix):
			t.attempts = append(t.attempts, s)
		default:
			t.others = append(t.others, s)
		}
	}

	c.check(c.failures == spanFailuresBefore, "all %d spans: valid 32-hex traceId, 16-hex spanId, resource service.name=%q", len(spans), expectedService)

	traces := make([]*trace, 0, len(order))
	for _, id := range order {
		t := byTrace[id]
		c.check(t.root != nil, "trace %s has a root span named %s*", id, rootSpanPrefix)
		if t.root == nil {
			continue
		}
		sort.Slice(t.attempts, func(i, j int) bool { return t.attempts[i].Start.int() < t.attempts[j].Start.int() })
		traces = append(traces, t)
	}
	sort.Slice(traces, func(i, j int) bool { return traces[i].root.Start.int() < traces[j].root.Start.int() })

	fmt.Println("-- trace trees (ordered by root start time) --")
	for _, t := range traces {
		fmt.Printf("trace %s\n", t.id)
		printSpan("  ", t.root)
		for _, a := range t.attempts {
			printSpan("    └─ ", a)
		}
		for _, o := range t.others {
			printSpan("    ?  ", o)
		}
	}

	c.check(len(traces) == len(scenarios), "one trace per scenario request (traces=%d scenarios=%d)", len(traces), len(scenarios))
	n := len(traces)
	if len(scenarios) < n {
		n = len(scenarios)
	}
	for i := 0; i < n; i++ {
		s, t := scenarios[i], traces[i]
		name := s.Name
		if rm := t.root.attrs[attrRequestModel]; rm != "" {
			c.check(rm == s.Model, "%s: root %s=%q matches requested model %q", name, attrRequestModel, rm, s.Model)
		}
		c.check(len(t.attempts) >= 1, "%s: has at least one attempt span (%d)", name, len(t.attempts))
		for _, a := range t.attempts {
			c.quiet(a.ParentSpanID == t.root.SpanID, "%s: attempt %s parentSpanId=%s equals root spanId=%s", name, a.SpanID, a.ParentSpanID, t.root.SpanID)
		}
		for _, o := range t.others {
			c.check(o.ParentSpanID != "", "%s: extra span %q has a parent", name, o.Name)
		}
		if len(t.attempts) == 0 {
			continue
		}
		last := t.attempts[len(t.attempts)-1]
		switch s.Kind {
		case "success":
			c.check(t.root.code != statusError, "%s: root status not ERROR", name)
			c.check(last.code != statusError, "%s: last attempt not ERROR", name)
		case "retry":
			first := t.attempts[0]
			c.check(len(t.attempts) >= 2, "%s: >=2 attempt spans (%d)", name, len(t.attempts))
			c.check(first.code == statusError, "%s: first attempt status ERROR (code=%d)", name, first.code)
			c.check(first.attrs[attrHTTPStatus] == "429", "%s: first attempt %s=429 (got %q)", name, attrHTTPStatus, first.attrs[attrHTTPStatus])
			// OTel spec: instrumentation leaves successful spans UNSET; only failures are ERROR.
			c.check(last.code != statusError, "%s: last attempt not ERROR (code=%d)", name, last.code)
			c.check(t.root.code != statusError, "%s: root status not ERROR", name)
		case "fail":
			c.check(t.root.code == statusError, "%s: root status ERROR (code=%d)", name, t.root.code)
			for _, a := range t.attempts {
				c.check(a.code == statusError, "%s: attempt %s status ERROR", name, a.SpanID)
			}
		}
		if s.Kind == "success" || s.Kind == "retry" {
			c.check(last.attrs[attrInputTokens] == strconv.Itoa(expectedInputToks), "%s: successful attempt %s=%d (got %q)", name, attrInputTokens, expectedInputToks, last.attrs[attrInputTokens])
			c.check(last.attrs[attrOutputTokens] == strconv.Itoa(expectedOutputToks), "%s: successful attempt %s=%d (got %q)", name, attrOutputTokens, expectedOutputToks, last.attrs[attrOutputTokens])
		}
		if ck, ok := t.root.attrs[attrClientKey]; ok {
			c.check(ck != "" && !strings.Contains(ck, "e2e-client-key-0001"), "%s: root %s is masked (%q)", name, attrClientKey, ck)
		} else {
			c.check(false, "%s: root has %s attribute", name, attrClientKey)
		}
	}
}

func main() {
	runDir := flag.String("run", "run", "run directory produced by run.sh")
	dump := flag.String("dump", "", "receiver dump JSON (default <run>/dump.json)")
	harnessOnly := flag.Bool("harness-only", false, "skip trace assertions (no plugin loaded)")
	countRoots := flag.Bool("count-roots", false, "print the number of root spans in the dump and exit")
	flag.Parse()
	if *dump == "" {
		*dump = filepath.Join(*runDir, "dump.json")
	}
	if *countRoots {
		spans, _, errLoad := loadSpans(*dump)
		if errLoad != nil {
			fmt.Println(0)
			return
		}
		n := 0
		for _, s := range spans {
			if strings.HasPrefix(s.Name, rootSpanPrefix) && s.ParentSpanID == "" {
				n++
			}
		}
		fmt.Println(n)
		return
	}

	c := &checker{}
	scenarios, err := readScenarios(filepath.Join(*runDir, "scenarios.jsonl"))
	if err != nil {
		fmt.Printf("cannot read scenarios: %v\n", err)
		os.Exit(2)
	}
	verifyScenarios(c, *runDir, scenarios)
	printUsageQueue(filepath.Join(*runDir, "usage_queue.json"))
	if *harnessOnly {
		fmt.Println("== Traces: skipped (harness-only mode, no plugin) ==")
	} else {
		verifyTraces(c, *dump, scenarios)
	}
	if c.failures > 0 {
		fmt.Printf("\nRESULT: FAIL (%d failed checks)\n", c.failures)
		os.Exit(1)
	}
	fmt.Println("\nRESULT: PASS")
}

// quiet records a check but only prints it on failure (used for per-span checks).
func (c *checker) quiet(ok bool, format string, args ...any) {
	if !ok {
		c.check(false, format, args...)
	}
}

// printUsageQueue prints CPA's own per-attempt usage records (management usage queue), for reference.
func printUsageQueue(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var records []map[string]any
	if errUnmarshal := json.Unmarshal(data, &records); errUnmarshal != nil {
		fmt.Printf("== CPA usage queue: unreadable (%v): %.200s ==\n", errUnmarshal, string(data))
		return
	}
	fmt.Printf("== CPA usage queue (info): %d records ==\n", len(records))
	for _, r := range records {
		tokens, _ := r["tokens"].(map[string]any)
		fail, _ := json.Marshal(r["fail"])
		fmt.Printf("  provider=%v model=%v alias=%v failed=%v stream=%v auth_index=%v in=%v out=%v latency_ms=%v request_id=%v execution_id=%v fail=%s\n",
			r["provider"], r["model"], r["alias"], r["failed"], r["stream"], r["auth_index"],
			tokens["input_tokens"], tokens["output_tokens"], r["latency_ms"], r["request_id"], r["execution_id"], fail)
	}
}
