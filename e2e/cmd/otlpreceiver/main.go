// Command otlpreceiver is a minimal OTLP/HTTP JSON trace receiver for the e2e harness.
//
// POST /v1/traces (application/json) -> 200 {} and the body is appended as one line to -out.
// Protobuf bodies are rejected with 415. GET /dump returns all received payloads as a JSON array.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4318", "listen address")
	out := flag.String("out", "traces.jsonl", "file to append received payloads to (one JSON per line)")
	failFirst := flag.Int("fail-first", 0, "respond 503 to the first N trace requests (exercise exporter retry)")
	flag.Parse()

	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("open %s: %v", *out, err)
		os.Exit(1)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Printf("close: %v", errClose)
		}
	}()

	var (
		mu       sync.Mutex
		payloads []json.RawMessage
		seen     int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if strings.Contains(ct, "protobuf") {
			log.Printf("REJECT 415: received protobuf payload (Content-Type=%q); this receiver only accepts application/json", ct)
			http.Error(w, "only application/json is supported", http.StatusUnsupportedMediaType)
			return
		}
		var reader io.Reader = r.Body
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
			gz, errGzip := gzip.NewReader(r.Body)
			if errGzip != nil {
				http.Error(w, "bad gzip", http.StatusBadRequest)
				return
			}
			reader = gz
		}
		body, errRead := io.ReadAll(reader)
		if errRead != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen++
		n := seen
		mu.Unlock()
		if n <= *failFirst {
			log.Printf("request #%d: injected 503 (fail-first=%d)", n, *failFirst)
			w.Header().Set("Retry-After", "0")
			http.Error(w, "injected failure", http.StatusServiceUnavailable)
			return
		}
		if !json.Valid(body) {
			log.Printf("request #%d: invalid JSON (Content-Type=%q, %d bytes)", n, ct, len(body))
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		var compact bytes.Buffer
		if errCompact := json.Compact(&compact, body); errCompact != nil {
			compact.Reset()
			compact.Write(body)
		}
		mu.Lock()
		payloads = append(payloads, json.RawMessage(append([]byte(nil), compact.Bytes()...)))
		_, errWrite := f.Write(append(compact.Bytes(), '\n'))
		count := len(payloads)
		mu.Unlock()
		if errWrite != nil {
			log.Printf("write: %v", errWrite)
		}
		log.Printf("request #%d: accepted %d bytes (stored payloads=%d, Content-Type=%q, Content-Encoding=%q)", n, len(body), count, ct, r.Header.Get("Content-Encoding"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("GET /dump", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		list := payloads
		if list == nil {
			list = []json.RawMessage{}
		}
		if errEnc := json.NewEncoder(w).Encode(list); errEnc != nil {
			log.Printf("dump: %v", errEnc)
		}
	})

	log.Printf("otlpreceiver listening on %s, writing to %s, fail-first=%d", *addr, *out, *failFirst)
	if errListen := http.ListenAndServe(*addr, mux); errListen != nil {
		log.Printf("listen: %v", errListen)
	}
}
