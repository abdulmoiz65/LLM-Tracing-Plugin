package main

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Management API routes live under the host's only plugin management prefix
// (/v0/management). The host authenticates them with the management key
// before management.handle is called. Resources live under
// /v0/resource/plugins/<plugin id>/ and are NOT authenticated, so they serve
// only static page assets, never trace data.
const (
	apiBase            = "/v0/management/plugins/" + pluginID
	resourcePrefix     = "/v0/resource/plugins/"
	defaultListLimit   = 50
	maxListLimit       = 500
	maxModelFilterLen  = 200
	maxProviderLen     = 200
	maxMinDurationMS   = 24 * 60 * 60 * 1000
	contentTypeJSON    = "application/json; charset=utf-8"
	dashboardMenu      = "LLM Tracing"
	dashboardMenuHint  = "Requests, sessions, cost, insights and tokens"
	resourceDashboard  = "dashboard"
	resourceStylesheet = "dashboard.css"
	resourceScript     = "dashboard.js"
	resourceLogo       = "logo.png"
)

// pageCSP forbids inline script and style, plugins, foreign connections and
// framing by other origins (the Management Center frames the page from the
// same origin).
const pageCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"

//go:embed ui/dashboard.html
var dashboardHTML []byte

//go:embed ui/dashboard.css
var dashboardCSS []byte

//go:embed ui/dashboard.js
var dashboardJS []byte

// dashboardLogo is the 64 px author avatar in the page header (3 KB).
//
//go:embed assets/logo-64.png
var dashboardLogo []byte

// managementRequest mirrors the host's rpcManagementRequest: an embedded
// pluginapi.ManagementRequest (Go field names; Body is []byte, so Base64)
// plus host_callback_id, which this plugin does not use.
type managementRequest struct {
	Method  string
	Path    string
	Headers map[string][]string
	Query   map[string][]string
	Body    []byte
}

// managementResponse mirrors pluginapi.ManagementResponse.
type managementResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

type managementRoute struct {
	Method string
	Path   string
}

type resourceRoute struct {
	Path        string
	Menu        string `json:",omitempty"`
	Description string `json:",omitempty"`
}

type managementRegistration struct {
	Routes    []managementRoute `json:"routes"`
	Resources []resourceRoute   `json:"resources"`
}

func managementRegistrationResponse() managementRegistration {
	return managementRegistration{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: apiBase + "/summary"},
			{Method: http.MethodGet, Path: apiBase + "/traces"},
			{Method: http.MethodGet, Path: apiBase + "/trace"},
			{Method: http.MethodGet, Path: apiBase + "/sessions"},
			{Method: http.MethodGet, Path: apiBase + "/insights"},
			{Method: http.MethodGet, Path: apiBase + "/whatif"},
		},
		Resources: []resourceRoute{
			{Path: "/" + resourceDashboard, Menu: dashboardMenu, Description: dashboardMenuHint},
			{Path: "/" + resourceStylesheet},
			{Path: "/" + resourceScript},
			{Path: "/" + resourceLogo},
		},
	}
}

func securityHeaders(contentType string) map[string][]string {
	return map[string][]string{
		"Content-Type":            {contentType},
		"Cache-Control":           {"no-store"},
		"X-Content-Type-Options":  {"nosniff"},
		"Referrer-Policy":         {"no-referrer"},
		"Content-Security-Policy": {pageCSP},
	}
}

func jsonResponse(status int, value any) managementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"encoding failed"}`)
	}
	return managementResponse{StatusCode: status, Headers: securityHeaders(contentTypeJSON), Body: body}
}

func errorResponse(status int, message string) managementResponse {
	return jsonResponse(status, map[string]string{"error": message})
}

// resourceName returns the asset name of /v0/resource/plugins/<id>/<name>.
func resourceName(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, resourcePrefix)
	if !ok {
		return "", false
	}
	id, name, ok := strings.Cut(rest, "/")
	if !ok || id == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

func serveResource(name string) managementResponse {
	switch name {
	case resourceDashboard:
		return managementResponse{StatusCode: http.StatusOK, Headers: securityHeaders("text/html; charset=utf-8"), Body: dashboardHTML}
	case resourceStylesheet:
		return managementResponse{StatusCode: http.StatusOK, Headers: securityHeaders("text/css; charset=utf-8"), Body: dashboardCSS}
	case resourceLogo:
		return managementResponse{StatusCode: http.StatusOK, Headers: securityHeaders("image/png"), Body: dashboardLogo}
	case resourceScript:
		return managementResponse{StatusCode: http.StatusOK, Headers: securityHeaders("text/javascript; charset=utf-8"), Body: dashboardJS}
	default:
		return errorResponse(http.StatusNotFound, "not found")
	}
}

// handleManagement serves management.handle for both API routes and
// resources. Every branch answers with a JSON or asset body and a status
// code; it never returns an RPC error for a bad HTTP request.
func (p *pluginState) handleManagement(req managementRequest) managementResponse {
	if !strings.EqualFold(req.Method, http.MethodGet) {
		return errorResponse(http.StatusMethodNotAllowed, "method not allowed")
	}
	if name, ok := resourceName(req.Path); ok {
		return serveResource(name)
	}
	endpoint, ok := strings.CutPrefix(req.Path, apiBase+"/")
	if !ok {
		return errorResponse(http.StatusNotFound, "not found")
	}
	t := p.tracer
	cfg := t.config()
	if !cfg.uiActive() {
		return errorResponse(http.StatusNotFound, "traces page is disabled (ui_enabled: false)")
	}
	query := url.Values(req.Query)
	switch endpoint {
	case "summary":
		window, label, errMsg := parseRange(query, "1h")
		if errMsg != "" {
			return errorResponse(http.StatusBadRequest, errMsg)
		}
		t.refreshAccounts(cfg)
		summary := t.summaryFor(window, label)
		summary.History = p.currentHistory().status()
		summary.History.Days = cfg.HistoryDays
		return jsonResponse(http.StatusOK, summary)
	case "traces":
		filter, errMsg := parseTraceFilter(query)
		if errMsg != "" {
			return errorResponse(http.StatusBadRequest, errMsg)
		}
		if window, _, errRange := parseRange(query, "all"); errRange != "" {
			return errorResponse(http.StatusBadRequest, errRange)
		} else if window > 0 {
			filter.since = t.now().Add(-window).UnixNano()
		}
		return jsonResponse(http.StatusOK, map[string]any{"traces": t.store.list(filter, t.refreshAccounts(cfg))})
	case "sessions":
		window, _, errMsg := parseRange(query, "all")
		if errMsg != "" {
			return errorResponse(http.StatusBadRequest, errMsg)
		}
		limit := defaultListLimit
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return errorResponse(http.StatusBadRequest, "invalid limit: expected an integer")
			}
			limit = n
		}
		now := t.now()
		var since int64
		if window > 0 {
			since = now.Add(-window).UnixNano()
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"sessions":            t.store.sessions(since, limit, now, t.refreshAccounts(cfg)),
			"session_gap_seconds": int(cfg.SessionGap / time.Second),
		})
	case "insights":
		window, label, errMsg := parseRange(query, "all")
		if errMsg != "" {
			return errorResponse(http.StatusBadRequest, errMsg)
		}
		var since int64
		if window > 0 {
			since = t.now().Add(-window).UnixNano()
		}
		return jsonResponse(http.StatusOK, t.store.insights(since, label, t.refreshAccounts(cfg)))
	case "whatif":
		window, _, errMsg := parseRange(query, "all")
		if errMsg != "" {
			return errorResponse(http.StatusBadRequest, errMsg)
		}
		model := strings.TrimSpace(query.Get("model"))
		if model == "" || len(model) > maxModelFilterLen {
			return errorResponse(http.StatusBadRequest, "invalid model: expected a model name")
		}
		var since int64
		if window > 0 {
			since = t.now().Add(-window).UnixNano()
		}
		res, ok := t.store.whatIf(since, model)
		if !ok {
			return errorResponse(http.StatusBadRequest, "no price for this model; add it under pricing in the plugin config")
		}
		return jsonResponse(http.StatusOK, res)
	case "trace":
		tid, okID := parseTraceIDParam(query.Get("id"))
		if !okID {
			return errorResponse(http.StatusBadRequest, "invalid trace id: expected 32 hex characters")
		}
		detail, found := t.store.get(tid, t.refreshAccounts(cfg))
		if !found {
			return errorResponse(http.StatusNotFound, "trace not found")
		}
		return jsonResponse(http.StatusOK, map[string]any{"trace": detail})
	default:
		return errorResponse(http.StatusNotFound, "not found")
	}
}

// parseTraceFilter validates /traces query parameters. Out-of-range numbers
// are clamped; values that are not numbers or unknown statuses are rejected.
func parseTraceFilter(q url.Values) (traceFilter, string) {
	f := traceFilter{limit: defaultListLimit, status: "all"}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return f, "invalid limit: expected an integer"
		}
		f.limit = min(max(n, 1), maxListLimit)
	}
	switch status := strings.ToLower(strings.TrimSpace(q.Get("status"))); status {
	case "", "all":
	case "ok", "error", "canceled":
		f.status = status
	default:
		return f, "invalid status: expected all, ok, error or canceled"
	}
	model := strings.ToLower(strings.TrimSpace(q.Get("model")))
	f.model = truncateUTF8(model, maxModelFilterLen)
	provider := strings.ToLower(strings.TrimSpace(q.Get("provider")))
	f.provider = truncateUTF8(provider, maxProviderLen)
	if raw := strings.TrimSpace(q.Get("min_ms")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) {
			return f, "invalid min_ms: expected a number of milliseconds"
		}
		f.minMS = math.Min(math.Max(v, 0), maxMinDurationMS)
	}
	if raw := strings.ToLower(strings.TrimSpace(q.Get("session"))); raw != "" {
		if !validSessionID(raw) {
			return f, "invalid session: expected 32 hex characters"
		}
		f.session = raw
	}
	return f, ""
}

// timeRanges are the accepted `range` values; 0 means every stored trace.
var timeRanges = map[string]time.Duration{
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"all": 0,
}

func parseRange(q url.Values, fallback string) (time.Duration, string, string) {
	label := strings.ToLower(strings.TrimSpace(q.Get("range")))
	if label == "" {
		label = fallback
	}
	window, ok := timeRanges[label]
	if !ok {
		return 0, "", "invalid range: expected 15m, 1h, 6h, 24h, 7d or all"
	}
	return window, label, ""
}

func parseTraceIDParam(raw string) (traceID, bool) {
	var id traceID
	raw = strings.TrimSpace(raw)
	if len(raw) != 2*len(id) {
		return id, false
	}
	if _, err := hex.Decode(id[:], []byte(strings.ToLower(raw))); err != nil || id == (traceID{}) {
		return traceID{}, false
	}
	return id, true
}

type exporterStatus struct {
	Enabled       bool   `json:"enabled"`
	EndpointHost  string `json:"endpoint_host"`
	ExportedSpans uint64 `json:"exported_spans"`
	DroppedSpans  uint64 `json:"dropped_spans"`
	LastError     string `json:"last_error"`
	LastExportAt  string `json:"last_export_at"`
}

type summaryJSON struct {
	Version string `json:"version"`
	// Range is the requested window ("all": every stored trace, and then
	// WindowSeconds is 0).
	Range         string `json:"range"`
	WindowSeconds int    `json:"window_seconds"`
	summaryStats
	Exporter      exporterStatus `json:"exporter"`
	AccountLabels accountStatus  `json:"account_labels"`
	History       historyStatus  `json:"history"`
}

// refreshAccounts reloads credential labels from host.auth.list when the
// cache is stale and returns the label snapshot for the page, or nil when
// ui_show_account_labels is off (fallback labels only). It runs on the
// management request, never while a tracer or store lock is held.
func (t *tracer) refreshAccounts(cfg config) map[string]string {
	if !cfg.UIShowAccountLabels {
		return nil
	}
	t.accounts.refresh(t.now())
	return t.accounts.snapshot()
}

// summary is the default one-hour summary.
func (t *tracer) summary() summaryJSON { return t.summaryFor(summaryWindow, "1h") }

// summaryFor combines window statistics from the store with exporter state.
// A window <= 0 covers every stored trace.
func (t *tracer) summaryFor(window time.Duration, label string) summaryJSON {
	t.mu.Lock()
	now := t.now()
	labelsOn := t.cfg.UIShowAccountLabels || t.cfg.ExportAccountLabels
	exp := exporterStatus{
		Enabled:       t.cfg.active(),
		ExportedSpans: t.stats.exported,
		DroppedSpans:  t.stats.queueDropped + t.stats.exportDropped + t.stats.rejected,
		LastError:     t.lastExportErr,
	}
	if exp.Enabled {
		exp.EndpointHost = endpointHost(t.cfg.Endpoint)
	}
	if !t.lastExportAt.IsZero() {
		exp.LastExportAt = t.lastExportAt.UTC().Format(time.RFC3339)
	}
	t.mu.Unlock()
	return summaryJSON{
		Version:       pluginVersion,
		Range:         label,
		WindowSeconds: int(max(window, 0) / time.Second),
		summaryStats:  t.store.summaryFor(now, window),
		Exporter:      exp,
		AccountLabels: t.accounts.status(labelsOn),
	}
}

// endpointHost renders the exporter endpoint as host:port only: no scheme,
// credentials, path or query.
func endpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(parsed.Hostname(), port)
}
