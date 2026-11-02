package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		in     string
		secret string
	}{
		{"Authorization: Bearer abcdef1234567890.xyz", "abcdef1234567890"},
		{"auth basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA"},
		{`{"error":"invalid key sk-should-be-redacted-123456"}`, "sk-should-be-redacted-123456"},
		{"key sk-ant-api03-AbCdEfGhIjKlMnOp", "AbCdEfGhIjKlMnOp"},
		{"google AIzaSyA1234567890abcdefghijklmnopq rejected", "AIzaSyA1234567890abcdefghijklmnopq"},
		{"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U ok", "eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
		{"https://x/v1?api_key=supersecretvalue&x=1", "supersecretvalue"},
		{`{"api-key": "hunter2hunter2"}`, "hunter2hunter2"},
		{"client_secret=abc-def-ghi", "abc-def-ghi"},
		{"password: p@ssw0rd!", "p@ssw0rd!"},
	}
	for _, tc := range cases {
		got := redactSecrets(tc.in)
		if strings.Contains(got, tc.secret) {
			t.Errorf("secret survived redaction: %q -> %q", tc.in, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("no redaction marker: %q -> %q", tc.in, got)
		}
	}
	// Ordinary error text stays readable.
	plain := `{"error":{"type":"overloaded_error","message":"Overloaded","max_tokens":1024}}`
	if got := redactSecrets(plain); got != plain {
		t.Errorf("benign text altered: %q", got)
	}
}

func TestSanitizeErrorTextTruncates(t *testing.T) {
	long := strings.Repeat("é", 2000) + " sk-should-be-redacted-123456"
	got := sanitizeErrorText(long)
	if len(got) > maxErrorBytes {
		t.Fatalf("len=%d exceeds %d", len(got), maxErrorBytes)
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, truncatedMarker) {
		t.Fatalf("bad truncation: valid=%v", utf8.ValidString(got))
	}
	// A secret straddling the cut is redacted before truncation.
	straddle := strings.Repeat("a", maxErrorBytes-20) + " sk-should-be-redacted-123456 tail"
	if got := sanitizeErrorText(straddle); strings.Contains(got, "sk-should") || strings.Contains(got, "redacted-12") {
		t.Fatalf("straddling secret leaked: %q", got[len(got)-60:])
	}
	if sanitizeErrorText("  ") != "" {
		t.Fatal("blank should be empty")
	}
}

func TestMaskClientKey(t *testing.T) {
	raw := "e2e-client-key-0001"
	got := maskClientKey(raw)
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+12 {
		t.Fatalf("unexpected mask format %q", got)
	}
	if strings.Contains(got, raw) || strings.Contains(got, "0001") {
		t.Fatalf("mask leaks key: %q", got)
	}
	if maskClientKey(raw) != got || maskClientKey("other") == got {
		t.Fatal("mask must be stable and distinguish keys")
	}
	if maskClientKey("") != "" {
		t.Fatal("empty key must stay empty")
	}
}

func TestSanitizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://user:pass@api.example.com/v1?key=AIzaSecret#frag": "https://api.example.com/v1",
		"http://127.0.0.1:8080": "http://127.0.0.1:8080",
		"not a url":             "",
		"":                      "",
	}
	for in, want := range cases {
		if got := sanitizeBaseURL(in); got != want {
			t.Errorf("sanitizeBaseURL(%q)=%q want %q", in, got, want)
		}
	}
}
