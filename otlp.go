package main

import (
	"encoding/hex"
	"strconv"
	"time"
)

// OTLP/HTTP JSON encoding of opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest.
// Per the OTLP JSON mapping: field names are lowerCamelCase, traceId/spanId are
// lowercase hex (not Base64), 64-bit integers (timestamps, intValue) are
// decimal strings, and enums (kind, status.code) are integers.

const (
	spanKindServer = 2
	spanKindClient = 3

	statusUnset = 0
	statusError = 2
)

type otlpExportRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Events            []otlpEvent    `json:"events,omitempty"`
	Status            otlpStatus     `json:"status"`
}

type otlpEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpAnyValue is a oneof: exactly one pointer is set.
type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

// otlpPartialSuccess is the optional body of a successful export response.
type otlpExportResponse struct {
	PartialSuccess *struct {
		RejectedSpans jsonInt64 `json:"rejectedSpans"`
		ErrorMessage  string    `json:"errorMessage"`
	} `json:"partialSuccess"`
}

// jsonInt64 decodes an int64 encoded either as a JSON number or a string.
type jsonInt64 int64

func (n *jsonInt64) UnmarshalJSON(raw []byte) error {
	s := string(raw)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	*n = jsonInt64(v)
	return nil
}

func unixNano(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

func hexID(b []byte) string { return hex.EncodeToString(b) }

// attrs builds OTLP attribute lists, skipping empty strings.
type attrs []otlpKeyValue

func (a *attrs) str(key, value string) {
	if value == "" {
		return
	}
	v := value
	*a = append(*a, otlpKeyValue{Key: key, Value: otlpAnyValue{StringValue: &v}})
}

func (a *attrs) integer(key string, value int64) {
	v := strconv.FormatInt(value, 10)
	*a = append(*a, otlpKeyValue{Key: key, Value: otlpAnyValue{IntValue: &v}})
}

func (a *attrs) nonZero(key string, value int64) {
	if value != 0 {
		a.integer(key, value)
	}
}

func (a *attrs) boolean(key string, value bool) {
	v := value
	*a = append(*a, otlpKeyValue{Key: key, Value: otlpAnyValue{BoolValue: &v}})
}

func (a *attrs) double(key string, value float64) {
	v := value
	*a = append(*a, otlpKeyValue{Key: key, Value: otlpAnyValue{DoubleValue: &v}})
}
