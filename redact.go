package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// maxErrorBytes bounds exported error text.
	maxErrorBytes = 1024
	// maxRedactScanBytes bounds the input scanned by the redaction patterns.
	maxRedactScanBytes = 64 << 10
	truncatedMarker    = "...[truncated]"
)

type redaction struct {
	pattern     *regexp.Regexp
	replacement string
}

// redactions run in order; earlier, more specific patterns win.
var redactions = []redaction{
	// Authorization schemes.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9\-._~+/]+=*`), "$1 [REDACTED]"},
	// JWT-looking tokens (header.payload.signature).
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]*`), "[REDACTED_JWT]"},
	// OpenAI / Anthropic style keys (sk-..., sk-ant-..., sk-proj-...).
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`), "sk-[REDACTED]"},
	// Google API keys.
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{20,}`), "AIza[REDACTED]"},
	// key=value / "key": "value" style secrets.
	{regexp.MustCompile(`(?i)\b((?:x-)?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|auth[_-]?token|client[_-]?secret|password|secret|token|key)["']?\s*[:=]\s*["']?)[^"'&\s,;}]+`), "${1}[REDACTED]"},
}

// redactSecrets removes credentials from free text such as upstream error bodies.
func redactSecrets(s string) string {
	for _, r := range redactions {
		s = r.pattern.ReplaceAllString(s, r.replacement)
	}
	return s
}

// sanitizeErrorText redacts and truncates error text for export. Redaction
// runs before truncation so a secret straddling the cut is still removed.
func sanitizeErrorText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > maxRedactScanBytes {
		s = truncateUTF8(s, maxRedactScanBytes)
	}
	s = redactSecrets(s)
	if len(s) > maxErrorBytes {
		s = truncateUTF8(s, maxErrorBytes-len(truncatedMarker)) + truncatedMarker
	}
	return s
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// maskClientKey returns a stable, non-reversible fingerprint of the client
// API key. The host passes the raw client key (the authenticated principal),
// so it is never exported as-is.
func maskClientKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:6])
}

// sanitizeBaseURL strips credentials, query and fragment from an upstream URL.
// Unparseable values are dropped rather than exported verbatim.
func sanitizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}
