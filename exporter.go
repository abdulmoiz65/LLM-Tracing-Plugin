package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxExportRetries = 2
	baseBackoff      = 250 * time.Millisecond
	maxRetryAfter    = 5 * time.Second
	maxResponseBytes = 64 << 10
)

// exporter posts OTLP/HTTP JSON payloads. It owns its http.Client (with
// export_timeout_ms as the per-attempt timeout) and serializes sends through
// a one-slot semaphore so shutdown can wait for an in-flight batch with a deadline.
type exporter struct {
	mu       sync.Mutex
	endpoint string
	headers  map[string]string
	client   *http.Client
	sleep    func(ctx context.Context, d time.Duration) error
	sem      chan struct{}
}

func newExporter() *exporter {
	return &exporter{sem: make(chan struct{}, 1), sleep: sleepContext}
}

func (e *exporter) configure(cfg config) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 2
	client := &http.Client{Timeout: cfg.ExportTimeout, Transport: transport}
	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		headers[name] = value
	}
	e.mu.Lock()
	old := e.client
	e.endpoint, e.headers, e.client = cfg.Endpoint, headers, client
	e.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

func (e *exporter) acquire(ctx context.Context) bool {
	select {
	case e.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (e *exporter) release() { <-e.sem }

// send posts body, retrying network errors and 429/502/503/504 up to
// maxExportRetries times with exponential backoff, honoring Retry-After up to
// maxRetryAfter. It returns the number of spans the backend reported as rejected.
func (e *exporter) send(ctx context.Context, body []byte) (int64, error) {
	e.mu.Lock()
	endpoint, headers, client := e.endpoint, e.headers, e.client
	e.mu.Unlock()
	if endpoint == "" || client == nil {
		return 0, errors.New("no endpoint configured")
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		rejected, retryAfter, retryable, err := e.post(ctx, client, endpoint, headers, body)
		if err == nil {
			return rejected, nil
		}
		lastErr = err
		if !retryable || attempt >= maxExportRetries || ctx.Err() != nil {
			return 0, lastErr
		}
		delay := baseBackoff << attempt
		if retryAfter > 0 {
			delay = min(retryAfter, maxRetryAfter)
		}
		if errSleep := e.sleep(ctx, delay); errSleep != nil {
			return 0, lastErr
		}
	}
}

func (e *exporter) post(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, body []byte) (rejected int64, retryAfter time.Duration, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, 0, false, fmt.Errorf("build request for %s: invalid endpoint", safeEndpoint(endpoint))
	}
	req.Header.Set("User-Agent", pluginID+"/"+pluginVersion)
	for _, name := range sortedHeaderNames(headers) {
		req.Header.Set(name, headers[name])
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// url.Error embeds the full URL (including any query string); report
		// only the sanitized endpoint and the underlying cause.
		cause := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			cause = urlErr.Err
		}
		return 0, 0, true, fmt.Errorf("post %s: %v", safeEndpoint(endpoint), cause)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var parsed otlpExportResponse
		if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &parsed) == nil && parsed.PartialSuccess != nil {
			return int64(parsed.PartialSuccess.RejectedSpans), 0, false, nil
		}
		return 0, 0, false, nil
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		retryable = true
		retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	snippet := strings.TrimSpace(sanitizeErrorText(truncateUTF8(string(raw), 256)))
	if snippet != "" {
		snippet = ": " + snippet
	}
	return 0, retryAfter, retryable, fmt.Errorf("post %s: HTTP %d%s", safeEndpoint(endpoint), resp.StatusCode, snippet)
}

// parseRetryAfter supports delta-seconds and HTTP-date forms.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// safeEndpoint renders the endpoint for logs without credentials or query.
func safeEndpoint(endpoint string) string {
	if s := sanitizeBaseURL(endpoint); s != "" {
		return s
	}
	return "<endpoint>"
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
