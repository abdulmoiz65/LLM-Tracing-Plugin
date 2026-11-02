package main

import "encoding/json"

// envelope mirrors sdk/pluginabi.Envelope.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// encodeEnvelope wraps a dispatch result in the RPC envelope returned to the host.
func encodeEnvelope(value any, err error) []byte {
	output := envelope{OK: err == nil}
	if err != nil {
		output.Error = &envelopeError{Code: "plugin_error", Message: err.Error()}
	} else {
		raw, errJSON := json.Marshal(value)
		if errJSON != nil {
			output.OK = false
			output.Error = &envelopeError{Code: "encoding_error", Message: "unable to encode plugin response"}
		} else {
			output.Result = raw
		}
	}
	raw, _ := json.Marshal(output)
	return raw
}

// hostLog writes one line to the proxy log via host.log; errors are ignored
// (for example during shutdown, when the host has closed callbacks).
func hostLog(level, message string) {
	_, _ = callHost("host.log", map[string]any{"level": level, "message": message})
}
