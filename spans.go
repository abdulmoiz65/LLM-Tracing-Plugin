package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

type traceID [16]byte
type spanID [8]byte

func (id traceID) String() string { return hexID(id[:]) }
func (id spanID) String() string  { return hexID(id[:]) }
func (id spanID) isZero() bool    { return id == spanID{} }

// tokenTotals accumulates per-attempt token counters for the root span.
type tokenTotals struct {
	input, output, reasoning, cached, cacheRead, cacheCreation, total, totalInput int64
}

func (t *tokenTotals) add(u *usageRecord) {
	d := u.Detail
	t.totalInput += normalizeInput(d, u.Provider, u.ExecutorType).total
	t.input += d.InputTokens
	t.output += d.OutputTokens
	t.reasoning += d.ReasoningTokens
	t.cached += d.CachedTokens
	t.cacheRead += d.CacheReadTokens
	t.cacheCreation += d.CacheCreationTokens
	t.total += d.TotalTokens
}

// put writes rolled-up totals under cpa.usage.* (not gen_ai.usage.*), so
// backends that sum generation usage per trace do not double count the root.
func (t tokenTotals) put(a *attrs) {
	a.integer("cpa.usage.input_tokens", t.input)
	a.integer("cpa.usage.total_input_tokens", t.totalInput)
	a.integer("cpa.usage.output_tokens", t.output)
	a.integer("cpa.usage.total_tokens", t.total)
	a.nonZero("cpa.usage.reasoning_tokens", t.reasoning)
	a.nonZero("cpa.usage.cached_tokens", t.cached)
	a.nonZero("cpa.usage.cache_read_tokens", t.cacheRead)
	a.nonZero("cpa.usage.cache_creation_tokens", t.cacheCreation)
}

// sortAttempts orders attempts by start time (ties by RequestID) so ordinals are stable.
func sortAttempts(usages []usageRecord) {
	sort.SliceStable(usages, func(i, j int) bool {
		a, b := usages[i].RequestedAt, usages[j].RequestedAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return usages[i].RequestID < usages[j].RequestID
	})
}

func attemptEnd(u *usageRecord) time.Time {
	if u.Latency <= 0 {
		return u.RequestedAt
	}
	return u.RequestedAt.Add(u.Latency)
}

// rootIdentity collects trace-level identity shared by all attempts.
func rootIdentity(a *attrs, usages []usageRecord) {
	for i := range usages {
		if s := usages[i].SessionID; s != "" {
			a.str("session.id", s)
			a.str("langfuse.session.id", s)
			break
		}
	}
	for i := range usages {
		if k := maskClientKey(usages[i].APIKey); k != "" {
			a.str("cpa.client_key", k)
			a.str("langfuse.user.id", k)
			break
		}
	}
}

func requestSpanName(model string) string {
	if model = strings.TrimSpace(model); model != "" {
		return "cpa.request " + model
	}
	return "cpa.request"
}

// buildRootSpan renders one RequestCompletion (one inbound request) as a SERVER span.
func buildRootSpan(c *requestCompletion, usages []usageRecord, tid traceID, sid spanID, now time.Time, captureErrors bool) otlpSpan {
	start, end := c.StartedAt, c.CompletedAt
	var totals tokenTotals
	var firstStart, lastEnd time.Time
	for i := range usages {
		totals.add(&usages[i])
		if s := usages[i].RequestedAt; !s.IsZero() && (firstStart.IsZero() || s.Before(firstStart)) {
			firstStart = s
		}
		if e := attemptEnd(&usages[i]); e.After(lastEnd) {
			lastEnd = e
		}
	}
	if start.IsZero() {
		start = firstStart
	}
	if start.IsZero() {
		start = now
	}
	if end.IsZero() || end.Before(start) {
		end = lastEnd
	}
	if end.Before(start) {
		end = start
	}
	model := c.RequestedModel
	if model == "" {
		model = c.Model
	}
	name := requestSpanName(model)

	var a attrs
	a.str("cpa.trace_id", c.TraceID)
	a.str("cpa.request_id", c.RequestID)
	a.str("cpa.outcome", c.Outcome)
	if c.StatusCode > 0 {
		a.integer("http.response.status_code", int64(c.StatusCode))
	}
	a.str("gen_ai.request.model", c.RequestedModel)
	a.str("cpa.model", c.Model)
	a.str("cpa.source_format", c.SourceFormat)
	a.boolean("cpa.stream", c.Stream)
	a.integer("cpa.attempt_count", int64(len(usages)))
	totals.put(&a)
	a.str("langfuse.trace.name", name)
	rootIdentity(&a, usages)

	span := otlpSpan{
		TraceID:           tid.String(),
		SpanID:            sid.String(),
		Name:              name,
		Kind:              spanKindServer,
		StartTimeUnixNano: unixNano(start),
		EndTimeUnixNano:   unixNano(end),
	}
	if c.Outcome != outcomeSucceeded {
		errorType := c.Outcome
		if c.StatusCode >= 400 {
			errorType = strconv.Itoa(c.StatusCode)
		}
		if errorType == "" {
			errorType = "unknown"
		}
		a.str("error.type", errorType)
		message := "request " + nonEmpty(c.Outcome, "failed")
		if captureErrors {
			if text := sanitizeErrorText(c.Error); text != "" {
				a.str("cpa.error", text)
				message = text
			}
		}
		a.str("langfuse.observation.level", "ERROR")
		span.Status = otlpStatus{Code: statusError, Message: message}
	}
	span.Attributes = a
	return span
}

// buildSyntheticRootSpan stands in for a RequestCompletion that never arrived.
func buildSyntheticRootSpan(cpaTraceID string, usages []usageRecord, tid traceID, sid spanID) otlpSpan {
	var totals tokenTotals
	var start, end time.Time
	for i := range usages {
		totals.add(&usages[i])
		if s := usages[i].RequestedAt; start.IsZero() || s.Before(start) {
			start = s
		}
		if e := attemptEnd(&usages[i]); e.After(end) {
			end = e
		}
	}
	if end.Before(start) {
		end = start
	}
	model := ""
	if len(usages) > 0 {
		model = nonEmpty(usages[0].Alias, usages[0].Model)
	}
	name := requestSpanName(model)
	var a attrs
	a.str("cpa.trace_id", cpaTraceID)
	a.str("cpa.outcome", "unknown")
	a.boolean("cpa.root_synthesized", true)
	a.str("gen_ai.request.model", model)
	a.integer("cpa.attempt_count", int64(len(usages)))
	totals.put(&a)
	a.str("langfuse.trace.name", name)
	rootIdentity(&a, usages)
	span := otlpSpan{
		TraceID:           tid.String(),
		SpanID:            sid.String(),
		Name:              name,
		Kind:              spanKindServer,
		StartTimeUnixNano: unixNano(start),
		EndTimeUnixNano:   unixNano(end),
	}
	if n := len(usages); n > 0 && usages[n-1].Failed {
		a.str("error.type", attemptErrorType(&usages[n-1]))
		span.Status = otlpStatus{Code: statusError, Message: "last attempt failed"}
	}
	span.Attributes = a
	return span
}

func attemptErrorType(u *usageRecord) string {
	if u.Failure.StatusCode > 0 {
		return strconv.Itoa(u.Failure.StatusCode)
	}
	return "error"
}

// buildAttemptSpan renders one UsageRecord (one upstream attempt) as a CLIENT span.
// A zero parent makes it the root of its own trace.
func buildAttemptSpan(u *usageRecord, tid traceID, sid, parent spanID, ordinal int, captureErrors bool) otlpSpan {
	provider := strings.TrimSpace(u.Provider)
	name := "cpa.attempt"
	if provider != "" {
		name += " " + provider
	}
	start := u.RequestedAt
	end := attemptEnd(u)

	var a attrs
	a.str("gen_ai.system", provider)
	a.str("gen_ai.provider.name", provider)
	a.str("gen_ai.request.model", u.Model)
	a.str("gen_ai.response.model", u.ResponseModel)
	a.integer("gen_ai.usage.input_tokens", u.Detail.InputTokens)
	a.integer("gen_ai.usage.output_tokens", u.Detail.OutputTokens)
	// gen_ai.usage.input_tokens stays the provider-reported value (v0.1.0
	// semantics; for Claude it excludes cache reads/writes). The normalized
	// prompt size, cache included exactly once, is cpa.usage.total_input_tokens.
	a.integer("cpa.usage.total_input_tokens", normalizeInput(u.Detail, u.Provider, u.ExecutorType).total)
	a.integer("cpa.usage.total_tokens", u.Detail.TotalTokens)
	a.nonZero("cpa.usage.reasoning_tokens", u.Detail.ReasoningTokens)
	a.nonZero("cpa.usage.cached_tokens", u.Detail.CachedTokens)
	a.nonZero("cpa.usage.cache_read_tokens", u.Detail.CacheReadTokens)
	a.nonZero("cpa.usage.cache_creation_tokens", u.Detail.CacheCreationTokens)
	a.str("cpa.trace_id", u.TraceID)
	a.str("cpa.request_id", u.RequestID)
	a.integer("cpa.attempt", int64(ordinal))
	a.str("cpa.alias", u.Alias)
	a.str("cpa.executor", u.ExecutorType)
	a.str("cpa.base_url", sanitizeBaseURL(u.BaseURL))
	a.str("cpa.auth_index", u.AuthIndex)
	a.str("cpa.auth_type", u.AuthType)
	a.str("cpa.client_key", maskClientKey(u.APIKey))
	a.str("cpa.access_provider", u.AccessProvider)
	a.str("cpa.session_id", u.SessionID)
	a.str("cpa.parent_session_id", u.ParentSessionID)
	a.str("cpa.reasoning_effort", u.ReasoningEffort)
	a.str("cpa.service_tier", u.ServiceTier)
	a.str("cpa.effective_service_tier", u.EffectiveServiceTier)
	a.str("cpa.response_service_tier", u.ResponseServiceTier)
	a.boolean("cpa.stream", u.Stream)
	if u.TTFT > 0 {
		a.integer("cpa.ttft_ms", u.TTFT.Milliseconds())
	}
	a.str("langfuse.observation.type", "generation")

	span := otlpSpan{
		TraceID:           tid.String(),
		SpanID:            sid.String(),
		Name:              name,
		Kind:              spanKindClient,
		StartTimeUnixNano: unixNano(start),
		EndTimeUnixNano:   unixNano(end),
	}
	if !parent.isZero() {
		span.ParentSpanID = parent.String()
	}
	if u.TTFT > 0 {
		span.Events = []otlpEvent{{TimeUnixNano: unixNano(start.Add(u.TTFT)), Name: "first_token"}}
	}
	if u.Failed {
		if u.Failure.StatusCode > 0 {
			a.integer("http.response.status_code", int64(u.Failure.StatusCode))
		}
		a.str("error.type", attemptErrorType(u))
		message := "upstream attempt failed"
		if u.Failure.StatusCode > 0 {
			message = "upstream status " + strconv.Itoa(u.Failure.StatusCode)
		}
		if captureErrors {
			if text := sanitizeErrorText(u.Failure.Body); text != "" {
				a.str("cpa.error", text)
				message = text
			}
		}
		a.str("langfuse.observation.level", "ERROR")
		a.str("langfuse.observation.status_message", message)
		span.Status = otlpStatus{Code: statusError, Message: message}
	}
	span.Attributes = a
	return span
}

func nonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
