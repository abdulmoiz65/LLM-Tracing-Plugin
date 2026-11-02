// Command mockupstream is a fake OpenAI-compatible upstream used by the e2e harness.
//
// Behavior is keyed by the upstream API key the proxy sends (Authorization: Bearer <key>):
//
//	mock-ok  -> 200 (non-stream JSON or SSE stream)
//	mock-429 -> 429 with a JSON error body containing a fake secret and Retry-After: 1
//	mock-500 -> 500
//	other    -> 401
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	promptTokens     = 42
	completionTokens = 7
	fakeSecret       = "sk-should-be-redacted-123456"
)

var mockModels = []string{"mock-gpt", "mock-gpt-upstream", "mock-retry", "mock-fail"}

type stats struct {
	mu     sync.Mutex
	byKey  map[string]int
	byPath map[string]int
	log    []string
}

func (s *stats) record(key, path string, stream bool, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey[key]++
	s.byPath[path]++
	s.log = append(s.log, fmt.Sprintf("%s key=%s path=%s stream=%v status=%d", time.Now().Format("15:04:05.000"), key, path, stream, status))
}

func bearer(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return auth
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func usageObj() map[string]any {
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	flag.Parse()

	st := &stats{byKey: map[string]int{}, byPath: map[string]int{}}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /_stats", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		writeJSON(w, 200, map[string]any{"by_key": st.byKey, "by_path": st.byPath, "log": st.log})
	})

	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		key := bearer(r)
		st.record(key, r.URL.Path, false, 200)
		log.Printf("key=%s path=%s stream=false", key, r.URL.Path)
		data := make([]map[string]any, 0, len(mockModels))
		for _, m := range mockModels {
			data = append(data, map[string]any{"id": m, "object": "model", "created": 1700000000, "owned_by": "mock"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	})

	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		key := bearer(r)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model         string `json:"model"`
			Stream        bool   `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.Unmarshal(body, &req)
		log.Printf("key=%s path=%s stream=%v model=%s", key, r.URL.Path, req.Stream, req.Model)

		switch key {
		case "mock-ok":
			// handled below
		case "mock-429":
			st.record(key, r.URL.Path, req.Stream, 429)
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 429, map[string]any{"error": map[string]any{
				"message": "Rate limit exceeded for key " + fakeSecret + ". Please retry later.",
				"type":    "rate_limit_error",
				"code":    "rate_limit_exceeded",
			}})
			return
		case "mock-500":
			st.record(key, r.URL.Path, req.Stream, 500)
			writeJSON(w, 500, map[string]any{"error": map[string]any{"message": "mock internal error", "type": "server_error"}})
			return
		default:
			st.record(key, r.URL.Path, req.Stream, 401)
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "invalid_request_error"}})
			return
		}

		st.record(key, r.URL.Path, req.Stream, 200)
		model := req.Model
		if model == "" {
			model = "mock-gpt"
		}
		id := fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano())
		created := time.Now().Unix()
		content := "Hello from mockupstream (" + model + ")"

		if !req.Stream {
			writeJSON(w, 200, map[string]any{
				"id": id, "object": "chat.completion", "created": created, "model": model,
				"choices": []any{map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": content},
					"finish_reason": "stop",
				}},
				"usage": usageObj(),
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		send := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			}
		}
		send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		for _, part := range strings.SplitAfter(content, " ") {
			send(chunk(map[string]any{"content": part}, nil))
		}
		send(chunk(map[string]any{}, "stop"))
		if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
			send(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{}, "usage": usageObj(),
			})
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := bearer(r)
		st.record(key, r.URL.Path, false, 404)
		log.Printf("key=%s path=%s method=%s -> 404", key, r.URL.Path, r.Method)
		writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "not found"}})
	})

	log.Printf("mockupstream listening on %s (models: %s)", *addr, strings.Join(sortedCopy(mockModels), ", "))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Printf("listen: %v", err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
