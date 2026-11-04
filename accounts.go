package main

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	// accountCacheTTL is how long host.auth.list results are reused. A failed
	// lookup is also retried only after this delay.
	accountCacheTTL = time.Minute
	// maxAccountLabelBytes bounds one account label.
	maxAccountLabelBytes = 128
	// maxAccountEntries bounds the cached auth entries.
	maxAccountEntries = 10000
	// shortAuthIndexLen is the auth index prefix shown in fallback labels.
	shortAuthIndexLen = 8
)

// hostAuthEntry is the subset of pluginapi.HostAuthFileEntry (json tags, as
// returned by host.auth.list inside {"files":[...]}) used to build labels.
// Account is deliberately absent: for API-key credentials the host fills it
// with the raw upstream API key.
type hostAuthEntry struct {
	AuthIndex   string `json:"auth_index"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Provider    string `json:"provider"`
	Label       string `json:"label"`
	Email       string `json:"email"`
	AccountType string `json:"account_type"`
}

type hostAuthListResponse struct {
	Files []hostAuthEntry `json:"files"`
}

// hostAuthList calls the host.auth.list callback. It is only invoked from
// the management handler or the background loop, never on the request path.
func hostAuthList() ([]hostAuthEntry, error) {
	raw, err := callHost("host.auth.list", struct{}{})
	if err != nil {
		return nil, err
	}
	return decodeHostAuthList(raw)
}

// decodeHostAuthList decodes the result of host.auth.list ({"files":[...]}).
func decodeHostAuthList(raw json.RawMessage) ([]hostAuthEntry, error) {
	var resp hostAuthListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, errors.New("decode host.auth.list response failed")
	}
	return resp.Files, nil
}

// accountResolver caches auth index -> label from host.auth.list.
type accountResolver struct {
	mu   sync.Mutex
	list func() ([]hostAuthEntry, error)
	// labels is replaced, never mutated, so snapshots can be read without mu.
	labels    map[string]string
	fetchedAt time.Time
	fetched   bool
	inflight  bool
	lastErr   string
}

func newAccountResolver(list func() ([]hostAuthEntry, error)) *accountResolver {
	return &accountResolver{list: list, labels: map[string]string{}}
}

// refresh reloads the labels when the cache is older than accountCacheTTL.
// The host call runs without holding mu; concurrent callers keep using the
// previous snapshot. Failures keep the previous labels.
func (r *accountResolver) refresh(now time.Time) {
	r.mu.Lock()
	if r.list == nil || r.inflight || (r.fetched && now.Sub(r.fetchedAt) < accountCacheTTL) {
		r.mu.Unlock()
		return
	}
	r.inflight = true
	list := r.list
	r.mu.Unlock()

	entries, err := list()
	var labels map[string]string
	if err == nil {
		labels = make(map[string]string, min(len(entries), maxAccountEntries))
		for _, e := range entries {
			if len(labels) >= maxAccountEntries {
				break
			}
			index := strings.TrimSpace(e.AuthIndex)
			if label := entryLabel(e); index != "" && label != "" {
				labels[index] = label
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight, r.fetched, r.fetchedAt = false, true, now
	if err != nil {
		r.lastErr = truncateItemError(sanitizeErrorText(err.Error()))
		return
	}
	r.labels, r.lastErr = labels, ""
}

func (r *accountResolver) snapshot() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.labels
}

type accountStatus struct {
	Enabled          bool   `json:"enabled"`
	KnownCredentials int    `json:"known_credentials"`
	LastRefreshAt    string `json:"last_refresh_at"`
	LastError        string `json:"last_error"`
}

func (r *accountResolver) status(enabled bool) accountStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := accountStatus{Enabled: enabled, KnownCredentials: len(r.labels), LastError: r.lastErr}
	if r.fetched {
		out.LastRefreshAt = r.fetchedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// entryLabel picks the most readable identifier of an auth entry: e-mail,
// then the host label, then the auth file name. Values that look like
// credentials are skipped, so a label can never carry a raw key.
func entryLabel(e hostAuthEntry) string {
	name := strings.TrimSpace(e.Name)
	if len(name) > 5 && strings.EqualFold(name[len(name)-5:], ".json") {
		name = name[:len(name)-5]
	}
	for _, candidate := range []string{e.Email, e.Label, name} {
		if label := safeLabel(candidate); label != "" {
			return label
		}
	}
	return ""
}

// safeLabel returns a trimmed, bounded label, or "" when the value is empty,
// contains control characters or looks like a secret.
func safeLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return ""
	}
	if redactSecrets(s) != s || looksLikeToken(s) {
		return ""
	}
	if len(s) > maxAccountLabelBytes {
		s = truncateUTF8(s, maxAccountLabelBytes-len("...")) + "..."
	}
	return s
}

// looksLikeToken flags opaque values (API keys, access tokens) that the
// redaction patterns may not recognize: a run of 24 or more letters and
// digits without a separator. E-mail addresses are exempt.
func looksLikeToken(s string) bool {
	if strings.Contains(s, "@") {
		return false
	}
	run := 0
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			run++
			if run >= 24 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// fallbackAccountLabel identifies a credential without personal data:
// "<auth type> · <first 8 characters of the auth index>".
func fallbackAccountLabel(authIndex, authType string) string {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return ""
	}
	short := authIndex
	if len(short) > shortAuthIndexLen {
		short = short[:shortAuthIndexLen]
	}
	kind := safeLabel(authType)
	if kind == "" {
		kind = "credential"
	}
	return kind + " · " + short
}

// accountLabel resolves a credential to its cached label, falling back to
// the auth type and short auth index. labels may be nil.
func accountLabel(labels map[string]string, authIndex, authType string) string {
	if label := labels[strings.TrimSpace(authIndex)]; label != "" {
		return label
	}
	return fallbackAccountLabel(authIndex, authType)
}
