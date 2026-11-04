package main

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// hostAuthListFixture is shaped exactly like CLIProxyAPI's host.auth.list
// result (internal/pluginhost/auth_callbacks.go rpcHostAuthListResponse with
// pluginapi.HostAuthFileEntry json tags). For API-key credentials the host's
// "account" field holds the raw upstream key (coreauth.Auth.AccountInfo), and
// a label may be anything the user configured.
const hostAuthListFixture = `{"files":[
 {"id":"claude-alice@example.com.json","auth_index":"a72c126e9f00b1c2","name":"claude-alice@example.com.json","type":"claude","provider":"claude",
  "label":"alice@example.com","status":"active","source":"file","path":"C:\\auths\\claude-alice@example.com.json","size":512,
  "modtime":"2026-11-01T10:00:00Z","email":"alice@example.com","account_type":"oauth","account":"alice@example.com","success":3,
  "recent_requests":[{"time":"10:00","success":1,"failed":0}]},
 {"id":"codex-bob.json","auth_index":"b0b0b0b0c1c1c1c1","name":"codex-bob.json","type":"codex","provider":"codex","label":"","status":"active",
  "source":"file","email":"","account_type":"oauth","account":""},
 {"id":"apikey-file.json","auth_index":"cccccccc11111111","name":"claude-key.json","type":"claude","provider":"claude",
  "label":"sk-ant-api03-SHOULDNOTLEAK0123456789abcdef","status":"active","source":"file",
  "account_type":"apikey","account":"sk-ant-api03-SHOULDNOTLEAK0123456789abcdef"},
 {"id":"runtime","auth_index":"dddddddd22222222","name":"0123456789abcdefABCDEF0123456789abcdef","type":"gemini","runtime_only":true,"source":"memory"},
 {"id":"","auth_index":"","name":"orphan.json"}
]}`

func fixtureList(t *testing.T) func() ([]hostAuthEntry, error) {
	t.Helper()
	return func() ([]hostAuthEntry, error) { return decodeHostAuthList(json.RawMessage(hostAuthListFixture)) }
}

func TestAccountLabelsFromHostAuthList(t *testing.T) {
	r := newAccountResolver(fixtureList(t))
	r.refresh(baseTime)
	labels := r.snapshot()
	if labels["a72c126e9f00b1c2"] != "alice@example.com" {
		t.Fatalf("oauth label: %v", labels)
	}
	if labels["b0b0b0b0c1c1c1c1"] != "codex-bob" {
		t.Fatalf("file name fallback (without .json): %q", labels["b0b0b0b0c1c1c1c1"])
	}
	if labels["cccccccc11111111"] != "claude-key" {
		t.Fatalf("api-key entry must not use the key-like label: %q", labels["cccccccc11111111"])
	}
	if _, ok := labels["dddddddd22222222"]; ok {
		t.Fatalf("token-like name must be skipped: %v", labels)
	}
	if len(labels) != 3 {
		t.Fatalf("labels: %v", labels)
	}
	for _, l := range labels {
		if strings.Contains(l, "SHOULDNOTLEAK") || strings.Contains(l, "sk-ant") {
			t.Fatalf("raw key in label: %q", l)
		}
	}
	if got := accountLabel(labels, "a72c126e9f00b1c2", "oauth"); got != "alice@example.com" {
		t.Fatal(got)
	}
	// Config API keys are not in host.auth.list: auth type + short index.
	if got := accountLabel(labels, "1f2e3d4c5b6a7980", "apikey"); got != "apikey · 1f2e3d4c" {
		t.Fatal(got)
	}
	if accountLabel(nil, "", "oauth") != "" || accountLabel(nil, "abc", "") != "credential · abc" {
		t.Fatal("fallback edge cases")
	}
	st := r.status(true)
	if !st.Enabled || st.KnownCredentials != 3 || st.LastError != "" || st.LastRefreshAt != baseTime.UTC().Format(time.RFC3339) {
		t.Fatalf("status: %+v", st)
	}
	if long := safeLabel(strings.Repeat("é", 100) + "@x"); len(long) > maxAccountLabelBytes || !strings.HasSuffix(long, "...") {
		t.Fatalf("label not bounded: %d", len(long))
	}
	if safeLabel("Bearer abc.def") != "" || safeLabel("a\nb") != "" || safeLabel("x-9f8e7d6c5b4a39281706f5e4d3c2b1a0") != "" {
		t.Fatal("secret, token-like value or control characters must be rejected")
	}
	if safeLabel("gemini-my-project-name-2026-prod") == "" || safeLabel("averyveryverylongusername2026@example.com") == "" {
		t.Fatal("ordinary file names and e-mails must be kept")
	}
}

func TestAccountCacheTTLAndFailures(t *testing.T) {
	calls := 0
	fail := false
	list := fixtureList(t)
	r := newAccountResolver(func() ([]hostAuthEntry, error) {
		calls++
		if fail {
			return nil, errors.New("host callback host.auth.list failed: Bearer secret-token-123")
		}
		return list()
	})
	r.refresh(baseTime)
	r.refresh(baseTime.Add(30 * time.Second))
	if calls != 1 {
		t.Fatalf("cache must be reused within the TTL: %d calls", calls)
	}
	fail = true
	r.refresh(baseTime.Add(accountCacheTTL))
	if calls != 2 || len(r.snapshot()) != 3 {
		t.Fatalf("failed refresh must keep the previous labels: calls=%d labels=%v", calls, r.snapshot())
	}
	if st := r.status(true); !strings.Contains(st.LastError, "failed") || strings.Contains(st.LastError, "secret-token-123") {
		t.Fatalf("last error: %q", st.LastError)
	}
	r.refresh(baseTime.Add(accountCacheTTL + 30*time.Second))
	if calls != 2 {
		t.Fatal("a failure is retried only after the TTL")
	}
	fail = false
	r.refresh(baseTime.Add(2*accountCacheTTL + time.Second))
	if calls != 3 || r.status(true).LastError != "" {
		t.Fatal("recovery after the TTL")
	}

	// Callback unavailable (no host, or cgo disabled): fallback labels only.
	down := newAccountResolver(func() ([]hostAuthEntry, error) { return nil, errors.New("host callbacks require cgo") })
	down.refresh(baseTime)
	if got := accountLabel(down.snapshot(), "a72c126e9f00b1c2", "oauth"); got != "oauth · a72c126e" {
		t.Fatalf("unavailable callback: %q", got)
	}
	if _, err := decodeHostAuthList(json.RawMessage(`[1,2]`)); err == nil {
		t.Fatal("malformed response must fail")
	}
}

// A refresh in progress must not block or duplicate concurrent callers.
func TestAccountRefreshSingleFlight(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	r := newAccountResolver(func() ([]hostAuthEntry, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		close(entered)
		<-release
		return []hostAuthEntry{{AuthIndex: "x1", Email: "x@example.com"}}, nil
	})
	done := make(chan struct{})
	go func() {
		r.refresh(baseTime)
		close(done)
	}()
	<-entered
	r.refresh(baseTime) // returns immediately with the old (empty) snapshot
	if len(r.snapshot()) != 0 {
		t.Fatal("snapshot changed before the fetch finished")
	}
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 || r.snapshot()["x1"] != "x@example.com" {
		t.Fatalf("calls=%d labels=%v", calls, r.snapshot())
	}
}

// retryAcrossAccounts records a 429 on credential A followed by a success on
// credential B.
func retryAcrossAccounts(tr *tracer, clock *fakeClock) {
	tr.addCompletion(completionAt("acct", baseTime, baseTime.Add(3*time.Second), outcomeSucceeded, 200))
	a := usageAt("acct", "a1", baseTime.Add(100*time.Millisecond), 200*time.Millisecond)
	a.AuthIndex, a.AuthType = "a72c126e9f00b1c2", "oauth"
	a.Failed, a.Failure = true, usageFailure{StatusCode: 429}
	b := usageAt("acct", "a2", baseTime.Add(500*time.Millisecond), time.Second)
	b.Provider = "codex"
	b.AuthIndex, b.AuthType = "b0b0b0b0c1c1c1c1", "oauth"
	tr.addUsage(b)
	tr.addUsage(a)
	finalize(tr, clock)
}

func TestProviderAndAccountInAPI(t *testing.T) {
	clock := newFakeClock()
	p := uiPlugin(t, clock)
	calls := 0
	list := fixtureList(t)
	p.tracer.accounts.list = func() ([]hostAuthEntry, error) { calls++; return list() }
	retryAcrossAccounts(p.tracer, clock)

	_, _, body := mgmtGet(t, p, "/traces", nil)
	items := decodeTraces(t, body)
	if len(items) != 1 || items[0].Provider != "codex" || items[0].Account != "codex-bob" {
		t.Fatalf("serving attempt provider/account: %+v", items)
	}
	_, _, body = mgmtGet(t, p, "/trace", map[string]string{"id": items[0].TraceID})
	var out struct {
		Trace traceDetailJSON `json:"trace"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range out.Trace.Spans {
		if s.Kind == "client" {
			got = append(got, s.Status+":"+s.Provider+":"+s.Account)
		}
		if _, ok := s.Attributes["cpa.account"]; ok {
			t.Fatal("stored spans must not keep cpa.account")
		}
	}
	if strings.Join(got, ",") != "error:claude:alice@example.com,unset:codex:codex-bob" {
		t.Fatalf("per-attempt provider/account: %v", got)
	}
	if calls != 1 {
		t.Fatalf("host.auth.list must be cached across requests: %d calls", calls)
	}

	// Provider filter (exact, case-insensitive).
	_, _, body = mgmtGet(t, p, "/traces", map[string]string{"provider": "CODEX"})
	if len(decodeTraces(t, body)) != 1 {
		t.Fatal("provider filter should match")
	}
	_, _, body = mgmtGet(t, p, "/traces", map[string]string{"provider": "claude"})
	if len(decodeTraces(t, body)) != 0 {
		t.Fatal("provider filter matches the serving attempt only")
	}

	// Summary exposes the label cache state and the provider list.
	_, _, body = mgmtGet(t, p, "/summary", nil)
	var s summaryJSON
	if err := json.Unmarshal(body, &s); err != nil || !s.AccountLabels.Enabled || s.AccountLabels.KnownCredentials != 3 || strings.Join(s.Providers, ",") != "codex" {
		t.Fatalf("summary: %s", body)
	}
}

func TestAccountLabelsCanBeHiddenOnPage(t *testing.T) {
	clock := newFakeClock()
	p, _ := newTestPlugin(t, clock)
	if _, err := p.dispatch("plugin.register", registerFixture("ui_show_account_labels: false\n", 10)); err != nil {
		t.Fatal(err)
	}
	p.tracer.accounts.list = func() ([]hostAuthEntry, error) {
		t.Fatal("host.auth.list must not be called when labels are off")
		return nil, nil
	}
	retryAcrossAccounts(p.tracer, clock)
	_, _, body := mgmtGet(t, p, "/traces", nil)
	if items := decodeTraces(t, body); items[0].Account != "oauth · b0b0b0b0" {
		t.Fatalf("fallback label: %+v", items[0])
	}
	_, _, body = mgmtGet(t, p, "/summary", nil)
	if strings.Contains(string(body), `"account_labels":{"enabled":true`) {
		t.Fatalf("labels reported enabled: %s", body)
	}
}

func TestAccountLabelsExportedOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		c := newCollector(t)
		clock := newFakeClock()
		cfg := testConfig(c.endpoint())
		cfg.ExportAccountLabels = enabled
		tr := newTestTracer(t, cfg, clock)
		tr.accounts.list = fixtureList(t)
		tr.accounts.refresh(clock.Now()) // done by the background loop in production
		retryAcrossAccounts(tr, clock)
		spans := exportNow(t, tr, c)
		raw := strings.Join(func() []string {
			var out []string
			for _, b := range c.bodies() {
				out = append(out, string(b))
			}
			return out
		}(), "\n")
		roots, children := rootsAndChildren(spans)
		if len(roots) != 1 || len(children) != 2 || hasAttr(roots[0], "cpa.account") {
			t.Fatalf("enabled=%v: shape or root label", enabled)
		}
		if !enabled {
			for _, ch := range children {
				if hasAttr(ch, "cpa.account") {
					t.Fatal("cpa.account exported by default")
				}
			}
			for _, leak := range []string{"alice@example.com", "codex-bob"} {
				if strings.Contains(raw, leak) {
					t.Fatalf("label %q exported by default", leak)
				}
			}
			continue
		}
		var labels []string
		for _, ch := range children {
			labels = append(labels, strAttr(t, ch, "cpa.account"))
		}
		if strings.Join(labels, ",") != "alice@example.com,codex-bob" && strings.Join(labels, ",") != "codex-bob,alice@example.com" {
			t.Fatalf("exported labels: %v", labels)
		}
	}
}

func TestParseAccountLabelConfig(t *testing.T) {
	cfg, warnings, _ := parseConfig([]byte("enabled: true\n"))
	if !cfg.UIShowAccountLabels || cfg.ExportAccountLabels || len(warnings) != 0 {
		t.Fatalf("defaults: %+v", cfg)
	}
	cfg, warnings, _ = parseConfig([]byte("ui_show_account_labels: \"false\"\nexport_account_labels: true\n"))
	if cfg.UIShowAccountLabels || !cfg.ExportAccountLabels || len(warnings) != 0 {
		t.Fatalf("parsed: %+v %v", cfg, warnings)
	}
	names := map[string]bool{}
	for _, f := range configFields() {
		names[f.Name] = true
	}
	if !names["ui_show_account_labels"] || !names["export_account_labels"] {
		t.Fatal("account label config fields missing from registration")
	}
}
