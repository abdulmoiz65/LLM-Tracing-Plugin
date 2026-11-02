package main

import "time"

// The host serializes these structs (sdk/pluginapi in CLIProxyAPI) without
// json tags, so every field arrives under its Go field name. time.Time values
// arrive as RFC 3339 strings and time.Duration values as int64 nanoseconds.
// Only the fields this plugin uses are declared; the rest are ignored.

// lifecycleRequest is the payload of plugin.register and plugin.reconfigure.
// ConfigYAML is a []byte on the host side, so it arrives Base64-encoded.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// usageRecord mirrors pluginapi.UsageRecord: one record per upstream attempt.
type usageRecord struct {
	RequestID            string
	TraceID              string
	SelectionID          string
	Provider             string
	BaseURL              string
	ExecutorType         string
	Model                string
	Alias                string
	APIKey               string
	IsNativeKey          bool
	AccessProvider       string
	SessionID            string
	ParentSessionID      string
	AuthIndex            string
	AuthType             string
	ReasoningEffort      string
	ServiceTier          string
	EffectiveServiceTier string
	ResponseServiceTier  string
	ResponseModel        string
	Generate             bool
	Stream               bool
	RequestedAt          time.Time
	Latency              time.Duration
	TTFT                 time.Duration
	Failed               bool
	Failure              usageFailure
	Detail               usageDetail
}

// usageFailure mirrors pluginapi.UsageFailure.
type usageFailure struct {
	StatusCode int
	Body       string
}

// usageDetail mirrors pluginapi.UsageDetail.
type usageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// requestCompletion mirrors pluginapi.RequestCompletion (embedded in the host's
// rpcRequestCompletion next to host_callback_id): one event per inbound request.
type requestCompletion struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        string
	StatusCode     int
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time
}

const outcomeSucceeded = "succeeded"
