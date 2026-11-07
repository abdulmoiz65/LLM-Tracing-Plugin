package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Fixtures below are shaped exactly like what CLIProxyAPI sends:
//   - plugin.register / plugin.reconfigure: rpcLifecycleRequest
//     {config_yaml: []byte (Base64), schema_version, multi_auth_providers}
//   - usage.handle: pluginapi.UsageRecord marshaled as-is (no json tags, so Go
//     field names; time.Time as RFC 3339; time.Duration as int64 nanoseconds;
//     http.Header as a map of string lists).
//   - request.complete: rpcRequestCompletion = embedded pluginapi.RequestCompletion
//     (Go field names, flattened) + host_callback_id.

const usageFixture = `{
  "RequestID": "9b6f5a52-3a0e-4d39-9a51-f2b8f0c1e001",
  "TraceID": "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
  "SelectionID": "sel-1",
  "Provider": "openai-compatible-mock",
  "BaseURL": "https://user:pw@upstream.example.com/v1?key=AIzaQUERYSECRET",
  "ExecutorType": "OpenAICompatExecutor",
  "Model": "mock-gpt-upstream",
  "Alias": "mock-alias",
  "APIKey": "e2e-client-key-0001",
  "IsNativeKey": true,
  "AccessProvider": "config-inline",
  "SessionID": "sess-1",
  "ParentSessionID": "",
  "AuthID": "someone@example.com.json",
  "AuthIndex": "3",
  "AuthType": "api_key",
  "Source": "someone@example.com",
  "ReasoningEffort": "high",
  "ServiceTier": "priority",
  "EffectiveServiceTier": "priority",
  "ResponseServiceTier": "default",
  "ResponseModel": "mock-gpt-upstream-2026",
  "Generate": true,
  "Stream": true,
  "RequestedAt": "2026-11-02T15:00:00.1+05:00",
  "Latency": 1500000000,
  "TTFT": 300000000,
  "Failed": false,
  "Failure": {"StatusCode": 0, "Body": ""},
  "Detail": {"InputTokens": 42, "OutputTokens": 7, "ReasoningTokens": 3, "CachedTokens": 0, "CacheReadTokens": 11, "CacheCreationTokens": 0, "TotalTokens": 49},
  "ResponseHeaders": {"X-Request-Id": ["abc"], "Set-Cookie": ["session=secret"]}
}`

const failedUsageFixture = `{
  "RequestID": "9b6f5a52-3a0e-4d39-9a51-f2b8f0c1e000",
  "TraceID": "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
  "Provider": "openai-compatible-mock",
  "Model": "mock-gpt-upstream",
  "Alias": "mock-alias",
  "APIKey": "e2e-client-key-0001",
  "Generate": true,
  "RequestedAt": "2026-11-02T10:00:00.05Z",
  "Latency": 20000000,
  "TTFT": 0,
  "Failed": true,
  "Failure": {"StatusCode": 429, "Body": "{\"error\":{\"message\":\"Incorrect API key provided: sk-should-be-redacted-123456\"}}"},
  "Detail": {"InputTokens": 0, "OutputTokens": 0, "ReasoningTokens": 0, "CachedTokens": 0, "CacheReadTokens": 0, "CacheCreationTokens": 0, "TotalTokens": 0},
  "ResponseHeaders": null
}`

const completionFixture = `{
  "RequestID": "c5f0e0a4-1111-4222-8333-944455556666",
  "TraceID": "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
  "SourceFormat": "openai",
  "Model": "mock-alias",
  "RequestedModel": "mock-alias",
  "Stream": true,
  "Outcome": "succeeded",
  "StatusCode": 200,
  "Error": "",
  "StartedAt": "2026-11-02T10:00:00Z",
  "CompletedAt": "2026-11-02T10:00:02Z",
  "Metadata": {"canonical_session_id": "x"},
  "host_callback_id": "cb-123"
}`

func registerFixture(yaml string, schema uint32) []byte {
	raw, _ := json.Marshal(map[string]any{
		"config_yaml":          base64.StdEncoding.EncodeToString([]byte(yaml)),
		"schema_version":       schema,
		"multi_auth_providers": true,
	})
	return raw
}

func newTestPlugin(t *testing.T, clock *fakeClock) (*pluginState, *[]string) {
	t.Helper()
	var logs []string
	p := newPluginState(clock.Now, func(level, msg string) { logs = append(logs, level+": "+msg) }, false)
	p.tracer.exp.sleep = func(context.Context, time.Duration) error { return nil }
	return p, &logs
}

func TestDispatchHostShapedPayloads(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	p, _ := newTestPlugin(t, clock)
	// Exercise the package-level dispatch the C ABI calls.
	saved := plug
	plug = p
	t.Cleanup(func() { plug = saved })

	yaml := "enabled: true\npriority: 0\nendpoint: " + c.endpoint() + "\nservice_name: cpa-e2e\nheaders:\n    Authorization: Basic cGs6c2s=\n"
	result, err := dispatch("plugin.register", registerFixture(yaml, 10))
	if err != nil {
		t.Fatal(err)
	}
	env := encodeEnvelope(result, nil)
	var reg struct {
		OK     bool `json:"ok"`
		Result struct {
			SchemaVersion uint32 `json:"schema_version"`
			Metadata      struct {
				Name, Version, Author, GitHubRepository string
				ConfigFields                            []struct{ Name, Type, Description string }
			} `json:"metadata"`
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(env, &reg); err != nil || !reg.OK {
		t.Fatalf("envelope: %s", env)
	}
	if reg.Result.SchemaVersion != 10 || reg.Result.Metadata.Name != "LLM Tracing" || reg.Result.Metadata.Version != "0.4.1" ||
		reg.Result.Metadata.Author != pluginAuthor || reg.Result.Metadata.GitHubRepository == "" {
		t.Fatalf("registration: %+v", reg.Result)
	}
	if !reg.Result.Capabilities["usage_plugin"] || !reg.Result.Capabilities["request_lifecycle_plugin"] ||
		!reg.Result.Capabilities["management_api"] || len(reg.Result.Capabilities) != 3 {
		t.Fatalf("capabilities: %v", reg.Result.Capabilities)
	}
	if len(reg.Result.Metadata.ConfigFields) < 10 || reg.Result.Metadata.ConfigFields[0].Name != "endpoint" {
		t.Fatalf("config fields: %+v", reg.Result.Metadata.ConfigFields)
	}

	for method, body := range map[string]string{"usage.handle": failedUsageFixture} {
		if _, err := dispatch(method, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dispatch("request.complete", []byte(completionFixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch("usage.handle", []byte(usageFixture)); err != nil {
		t.Fatal(err)
	}
	if env := encodeEnvelope(dispatch("usage.handle", []byte(usageFixture))); !strings.HasPrefix(string(env), `{"ok":true`) {
		t.Fatalf("usage.handle envelope: %s", env)
	}

	clock.Advance(3 * time.Second)
	p.tracer.sweep(clock.Now())
	spans := exportNow(t, p.tracer, c)
	body := string(c.bodies()[0])
	for _, leaked := range []string{"e2e-client-key-0001", "sk-should-be-redacted-123456", "AIzaQUERYSECRET", "user:pw", "session=secret", "someone@example.com"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("export leaked %q", leaked)
		}
	}
	if c.requests[0].header.Get("Authorization") != "Basic cGs6c2s=" {
		t.Fatal("configured header not sent")
	}
	if !strings.Contains(body, `"cpa-e2e"`) {
		t.Fatal("service name missing")
	}

	roots, children := rootsAndChildren(spans)
	// The duplicate usage.handle call produces a third attempt (distinct RequestID is not required by the host).
	if len(roots) != 1 || len(children) != 3 {
		t.Fatalf("want 1 root + 3 children, got %d + %d", len(roots), len(children))
	}
	root := roots[0]
	if root.Name != "cpa.request mock-alias" || strAttr(t, root, "cpa.outcome") != "succeeded" || intAttr(t, root, "http.response.status_code") != "200" ||
		strAttr(t, root, "cpa.trace_id") != "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" || strAttr(t, root, "cpa.source_format") != "openai" {
		t.Fatalf("root wrong: %+v", root)
	}
	var failed, ok otlpSpan
	for _, ch := range children {
		if ch.ParentSpanID != root.SpanID {
			t.Fatal("child not parented")
		}
		if intAttr(t, ch, "cpa.attempt") == "1" {
			failed = ch
		} else {
			ok = ch
		}
	}
	if failed.Status.Code != statusError || intAttr(t, failed, "http.response.status_code") != "429" || !strings.Contains(strAttr(t, failed, "cpa.error"), "sk-[REDACTED]") {
		t.Fatalf("failed attempt wrong: %+v", failed)
	}
	want := map[string]string{
		"gen_ai.system":             "openai-compatible-mock",
		"gen_ai.provider.name":      "openai-compatible-mock",
		"gen_ai.request.model":      "mock-gpt-upstream",
		"gen_ai.response.model":     "mock-gpt-upstream-2026",
		"cpa.alias":                 "mock-alias",
		"cpa.executor":              "OpenAICompatExecutor",
		"cpa.base_url":              "https://upstream.example.com/v1",
		"cpa.client_key":            maskClientKey("e2e-client-key-0001"),
		"cpa.access_provider":       "config-inline",
		"cpa.session_id":            "sess-1",
		"cpa.auth_index":            "3",
		"cpa.auth_type":             "api_key",
		"cpa.reasoning_effort":      "high",
		"cpa.service_tier":          "priority",
		"cpa.request_id":            "9b6f5a52-3a0e-4d39-9a51-f2b8f0c1e001",
		"langfuse.observation.type": "generation",
	}
	for k, v := range want {
		if got := strAttr(t, ok, k); got != v {
			t.Errorf("%s=%q want %q", k, got, v)
		}
	}
	for k, v := range map[string]string{"gen_ai.usage.input_tokens": "42", "gen_ai.usage.output_tokens": "7", "cpa.usage.total_tokens": "49",
		"cpa.usage.reasoning_tokens": "3", "cpa.usage.cache_read_tokens": "11", "cpa.ttft_ms": "300"} {
		if got := intAttr(t, ok, k); got != v {
			t.Errorf("%s=%q want %q", k, got, v)
		}
	}
	// RequestedAt "2026-11-02T15:00:00.1+05:00" == 10:00:00.1Z; latency 1.5s.
	if ok.StartTimeUnixNano != unixNano(baseTime.Add(100*time.Millisecond)) || ok.EndTimeUnixNano != unixNano(baseTime.Add(1600*time.Millisecond)) {
		t.Fatalf("attempt times wrong: %s..%s", ok.StartTimeUnixNano, ok.EndTimeUnixNano)
	}
	if len(ok.Events) != 1 || ok.Events[0].TimeUnixNano != unixNano(baseTime.Add(400*time.Millisecond)) {
		t.Fatalf("first_token event wrong: %+v", ok.Events)
	}
	if strAttr(t, root, "langfuse.user.id") != maskClientKey("e2e-client-key-0001") || intAttr(t, root, "cpa.usage.input_tokens") != "84" {
		t.Fatal("root identity/rollup wrong")
	}
}

func TestDispatchSchemaNegotiationAndErrors(t *testing.T) {
	clock := newFakeClock()
	p, logs := newTestPlugin(t, clock)
	res, err := p.dispatch("plugin.register", registerFixture("enabled: true\npriority: 0\n", 7))
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["schema_version"].(uint32) != 7 {
		t.Fatal("must answer with the older host schema version")
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "no-op") {
		t.Fatalf("no-op not logged once: %v", *logs)
	}
	_, _ = p.dispatch("plugin.reconfigure", registerFixture("enabled: true\n", 10))
	if len(*logs) != 1 {
		t.Fatalf("no-op logged repeatedly: %v", *logs)
	}
	// No endpoint: observation calls are accepted and ignored.
	if _, err := p.dispatch("usage.handle", []byte(usageFixture)); err != nil || p.tracer.queueLen() != 0 {
		t.Fatalf("no-op usage.handle: err=%v queue=%d", err, p.tracer.queueLen())
	}
	if _, err := p.dispatch("usage.handle", []byte("{not json")); err == nil {
		t.Fatal("malformed usage must error")
	}
	if _, err := p.dispatch("scheduler.pick", nil); err == nil {
		t.Fatal("unknown method must error")
	}
	if _, err := p.dispatch("plugin.register", registerFixture("endpoint: [x", 10)); err == nil {
		t.Fatal("malformed config must error")
	}
	if env := string(encodeEnvelope(nil, errTest("boom"))); !strings.Contains(env, `"ok":false`) || !strings.Contains(env, `"boom"`) {
		t.Fatalf("error envelope: %s", env)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestShutdownFlushesPendingAndQueued(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	p, _ := newTestPlugin(t, clock)
	if _, err := p.dispatch("plugin.register", registerFixture("endpoint: "+c.endpoint()+"\ncompletion_grace_ms: 60000\n", 10)); err != nil {
		t.Fatal(err)
	}
	_, _ = p.dispatch("request.complete", []byte(completionFixture))
	_, _ = p.dispatch("usage.handle", []byte(usageFixture))
	_, _ = p.dispatch("usage.handle", []byte(strings.Replace(usageFixture, `"TraceID": "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"`, `"TraceID": ""`, 1)))
	if c.count() != 0 {
		t.Fatal("nothing should be exported before shutdown")
	}
	if _, err := p.dispatch("plugin.shutdown", nil); err != nil {
		t.Fatal(err)
	}
	if got := len(c.spans(t)); got != 3 {
		t.Fatalf("shutdown flushed %d spans, want 3", got)
	}
	if p.tracer.pendingLen() != 0 || p.tracer.queueLen() != 0 {
		t.Fatal("buffers not drained")
	}
}

func TestQuiesceFlushesAndLoopStops(t *testing.T) {
	c := newCollector(t)
	clock := newFakeClock()
	p, _ := newTestPlugin(t, clock)
	p.autoLoop = true
	if _, err := p.dispatch("plugin.register", registerFixture("endpoint: "+c.endpoint()+"\n"+ownDataDir(t), 10)); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	if !running {
		t.Fatal("loop should start when an endpoint is configured")
	}
	_, _ = p.dispatch("request.complete", []byte(completionFixture))
	_, _ = p.dispatch("plugin.quiesce", nil)
	if got := len(c.spans(t)); got != 1 {
		t.Fatalf("quiesce flushed %d spans", got)
	}
	p.shutdown()
	p.mu.Lock()
	running = p.running
	p.mu.Unlock()
	if running {
		t.Fatal("loop still marked running after shutdown")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("loop goroutine did not exit")
	}
}
