// LLM Tracing - built-in Traces page for CLIProxyAPI.
// Security: every value from the API is untrusted. The DOM is built only with
// createElement/textContent; no HTML strings are ever parsed.
(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const RESOURCE_MARK = '/v0/resource/plugins/';
  const SESSION_KEY = 'llm-tracing.management-key';
  const REFRESH_MS = 5000;
  const LIST_LIMIT = 100;
  const SLOW_MS = 10000;
  const SESSION_LIMIT = 20;
  const RANGE_KEY = 'llm-tracing.range';
  const RANGE_LABELS = { '15m': 'last 15 minutes', '1h': 'last hour', '6h': 'last 6 hours', '24h': 'last 24 hours', '7d': 'last 7 days', all: 'whole history' };

  // The page is served at <prefix>/v0/resource/plugins/<id>/dashboard; the
  // API lives on the same origin under <prefix>/v0/management/.
  const apiBase = (() => {
    const path = location.pathname;
    const at = path.indexOf(RESOURCE_MARK);
    const prefix = at > 0 ? path.slice(0, at) : '';
    return prefix + '/v0/management/plugins/llm-tracing';
  })();

  let managementKey = '';
  let timer = null;
  let busy = false;
  let selectedId = '';
  let modelDebounce = null;
  let sessionFilter = null; // {id, label} when the table shows one session

  // ---------- small DOM helpers ----------
  function el(tag, text, className) {
    const node = document.createElement(tag);
    if (text !== undefined && text !== null) node.textContent = String(text);
    if (className) node.className = className;
    return node;
  }
  function str(value) {
    return value === undefined || value === null ? '' : String(value);
  }
  function num(value) {
    const n = Number(value);
    return Number.isFinite(n) ? n : 0;
  }

  // ---------- formatting ----------
  function fmtDuration(ms) {
    ms = num(ms);
    if (ms < 1000) return Math.round(ms) + ' ms';
    if (ms < 60000) return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + ' s';
    const minutes = Math.floor(ms / 60000);
    return minutes + 'm ' + Math.round((ms % 60000) / 1000) + 's';
  }
  function fmtInt(value) {
    return Math.round(num(value)).toLocaleString();
  }
  // fmtCompact renders 950, 12.3k, 1.2M.
  function fmtCompact(value) {
    const n = Math.round(num(value));
    const abs = Math.abs(n);
    if (abs < 1000) return String(n);
    if (abs < 1e6) return (n / 1e3).toFixed(abs < 1e4 ? 1 : 0).replace(/\.0$/, '') + 'k';
    return (n / 1e6).toFixed(abs < 1e7 ? 1 : 0).replace(/\.0$/, '') + 'M';
  }
  // Token objects (trace rows, summary, per-attempt "tokens") share the
  // field names total_input_tokens, cache_read_tokens, cache_creation_tokens.
  function uncachedOf(t) {
    return Math.max(0, num(t.total_input_tokens) - num(t.cache_read_tokens) - num(t.cache_creation_tokens));
  }
  function inputBreakdown(t) {
    return fmtInt(uncachedOf(t)) + ' uncached + ' + fmtInt(t.cache_read_tokens) + ' cache read + ' +
      fmtInt(t.cache_creation_tokens) + ' cache write = ' + fmtInt(t.total_input_tokens) + ' input tokens';
  }
  function cacheText(t) {
    const read = num(t.cache_read_tokens);
    const write = num(t.cache_creation_tokens);
    const parts = [];
    if (read > 0) parts.push(fmtCompact(read) + ' read');
    if (write > 0) parts.push(fmtCompact(write) + ' write');
    return parts.length ? parts.join(' · ') : '-';
  }
  function fmtTime(iso) {
    const d = new Date(str(iso));
    if (Number.isNaN(d.getTime())) return '-';
    const now = new Date();
    const sameDay = d.toDateString() === now.toDateString();
    return sameDay ? d.toLocaleTimeString() : d.toLocaleString();
  }
  // fmtPercent keeps one decimal near the ends so 99.7% never shows as 100%.
  function fmtPercent(rate) {
    const r = num(rate);
    const decimals = (r > 0 && r < 0.1) || (r > 0.99 && r < 1) ? 1 : 0;
    let text = (r * 100).toFixed(decimals);
    if (r < 1 && text === '100.0') text = '99.9';
    return text + '%';
  }
  // fmtUSD renders $0.0042, $0.42, $12.34, $1.2k.
  function fmtUSD(value) {
    const n = num(value);
    if (n === 0) return '$0';
    if (n < 0.01) return '$' + n.toFixed(4);
    if (n < 1000) return '$' + n.toFixed(2);
    return '$' + fmtCompact(n);
  }
  function costText(t) {
    const priced = t.priced !== false;
    if (!priced && num(t.cost_usd) === 0) return '-';
    return fmtUSD(t.cost_usd) + (priced ? '' : '+');
  }
  function costTitle(t) {
    const parts = ['Estimated at API list prices' + (str(t.price_source) ? ' (' + str(t.price_source) + ')' : '')];
    const saved = num(t.cost_without_cache_usd) - num(t.cost_usd);
    if (saved > 0) parts.push('cache saved ' + fmtUSD(saved) + ' (would be ' + fmtUSD(t.cost_without_cache_usd) + ')');
    if (t.priced === false) parts.push('no price for some model; add it under pricing in the plugin config');
    return parts.join('; ');
  }
  // ctxGauge renders a context-window gauge; warn at 70%, bad at 90%.
  function ctxGauge(tokens, windowSize, used) {
    const box = el('div', undefined, 'ctx');
    tokens = num(tokens);
    windowSize = num(windowSize);
    if (tokens <= 0) {
      box.append(el('span', '-', 'ctx-text'));
      return box;
    }
    if (windowSize <= 0) {
      box.append(el('span', fmtCompact(tokens) + ' (window unknown)', 'ctx-text'));
      box.title = 'Set context_windows in the plugin config for this model.';
      return box;
    }
    const ratio = Math.min(1, Math.max(0, num(used)));
    if (ratio >= 0.9) box.classList.add('bad');
    else if (ratio >= 0.7) box.classList.add('warn');
    const track = el('div', undefined, 'ctx-track');
    const fill = el('div', undefined, 'ctx-fill');
    fill.style.width = Math.max(ratio * 100, 1) + '%';
    track.append(fill);
    box.append(track, el('span', fmtCompact(tokens) + ' / ' + fmtCompact(windowSize) + ' · ' + fmtPercent(ratio), 'ctx-text'));
    box.title = fmtInt(tokens) + ' of ' + fmtInt(windowSize) + ' context tokens used by the prompt' +
      (ratio >= 0.9 ? ' - nearly full: start a new conversation or compact' : ratio >= 0.7 ? ' - getting full' : '');
    return box;
  }
  function currentRange() {
    return $('f-range').value || 'all';
  }

  function isError(t) {
    return str(t.outcome) !== 'canceled' && (str(t.outcome) !== 'succeeded' || num(t.status_code) >= 400);
  }
  function statusBadge(t) {
    const outcome = str(t.outcome) || 'unknown';
    const code = num(t.status_code);
    const labels = { succeeded: 'OK', failed: 'Failed', rejected: 'Rejected', canceled: 'Canceled', unknown: 'Unknown' };
    let label = labels[outcome] || outcome;
    if (code > 0) label += ' ' + code;
    let kind = 'bad';
    if (!isError(t)) kind = 'ok';
    else if (outcome === 'rejected' || outcome === 'canceled') kind = 'warn';
    else if (outcome === 'unknown') kind = '';
    return el('span', label, 'badge' + (kind ? ' ' + kind : ''));
  }
  function attemptsText(t) {
    const count = num(t.attempt_count);
    const failed = num(t.failed_attempts);
    return failed > 0 ? count + ' (' + failed + ' failed)' : String(count);
  }

  // ---------- API ----------
  class ApiError extends Error {
    constructor(message, status) {
      super(message);
      this.status = status;
    }
  }

  async function api(path) {
    if (!managementKey) throw new ApiError('Enter the management key.', 401);
    const controller = new AbortController();
    const abort = setTimeout(() => controller.abort(), 15000);
    let response;
    try {
      response = await fetch(apiBase + path, {
        method: 'GET',
        headers: { Authorization: 'Bearer ' + managementKey, Accept: 'application/json' },
        credentials: 'omit',
        cache: 'no-store',
        referrerPolicy: 'no-referrer',
        signal: controller.signal,
      });
    } catch (err) {
      throw new ApiError('Cannot reach the proxy. Check that CLIProxyAPI is running.', 0);
    } finally {
      clearTimeout(abort);
    }
    let body = null;
    try {
      body = await response.json();
    } catch (err) {
      body = null;
    }
    if (!response.ok) {
      const serverMessage = body && typeof body.error === 'string' ? body.error : '';
      if (response.status === 401) {
        throw new ApiError('Wrong management key — 5 failed attempts lock this IP for 30 minutes.', 401);
      }
      if (response.status === 403) {
        throw new ApiError(serverMessage ? 'Access denied: ' + serverMessage : 'Access denied by the proxy.', 403);
      }
      if (response.status === 404 && !serverMessage) {
        throw new ApiError('The LLM Tracing API was not found. Make sure the plugin (v0.2.0 or newer) is enabled.', 404);
      }
      throw new ApiError(serverMessage || 'Request failed (HTTP ' + response.status + ').', response.status);
    }
    if (!body || typeof body !== 'object') throw new ApiError('The proxy returned an unexpected response.', response.status);
    return body;
  }

  // ---------- connect / disconnect ----------
  function storage() {
    try {
      return window.sessionStorage;
    } catch (err) {
      return null;
    }
  }

  function showConnectError(message) {
    const node = $('connect-error');
    node.textContent = message;
    node.hidden = !message;
  }

  async function connect() {
    const key = $('key').value.trim();
    if (!key) {
      showConnectError('Enter the management key.');
      return;
    }
    managementKey = key;
    $('connect-btn').disabled = true;
    showConnectError('');
    try {
      await refresh(true);
      const store = storage();
      if (store) {
        if ($('remember').checked) store.setItem(SESSION_KEY, key);
        else store.removeItem(SESSION_KEY);
      }
      $('key').value = '';
      $('connect').hidden = true;
      $('app').hidden = false;
      $('connbar').hidden = false;
      schedule();
    } catch (err) {
      managementKey = '';
      showConnectError(err.message);
    } finally {
      $('connect-btn').disabled = false;
    }
  }

  function disconnect(message) {
    managementKey = '';
    stopTimer();
    const store = storage();
    if (store) store.removeItem(SESSION_KEY);
    selectedId = '';
    sessionFilter = null;
    renderSessionChip();
    $('rows').replaceChildren();
    $('session-rows').replaceChildren();
    $('detail').hidden = true;
    $('app').hidden = true;
    $('connbar').hidden = true;
    $('connect').hidden = false;
    showConnectError(message || '');
    $('key').focus();
  }

  // ---------- refresh loop ----------
  function stopTimer() {
    if (timer) clearInterval(timer);
    timer = null;
  }

  function schedule() {
    stopTimer();
    if (!managementKey || !$('f-auto').checked || document.visibilityState !== 'visible') return;
    timer = setInterval(() => {
      if (document.visibilityState === 'visible') refresh(false).catch(handleError);
    }, REFRESH_MS);
  }

  function handleError(err) {
    if (err && (err.status === 401 || err.status === 403)) {
      // Stop retrying: CPA bans an IP after repeated failed key attempts.
      disconnect(err.message);
      return;
    }
    $('status-line').textContent = err && err.message ? err.message : 'Refresh failed.';
  }

  function listQuery() {
    const params = new URLSearchParams();
    params.set('limit', String(LIST_LIMIT));
    params.set('status', $('f-status').value);
    const model = $('f-model').value.trim();
    if (model) params.set('model', model.slice(0, 200));
    const provider = $('f-provider').value;
    if (provider) params.set('provider', provider.slice(0, 200));
    if ($('f-slow').checked) params.set('min_ms', String(SLOW_MS));
    params.set('range', currentRange());
    if (sessionFilter) params.set('session', sessionFilter.id);
    return '/traces?' + params.toString();
  }

  async function refresh(throwErrors) {
    if (busy) return;
    busy = true;
    try {
      const range = encodeURIComponent(currentRange());
      const [summary, list, sessions] = await Promise.all([
        api('/summary?range=' + range),
        api(listQuery()),
        api('/sessions?range=' + range + '&limit=' + SESSION_LIMIT),
      ]);
      renderSummary(summary);
      renderSessions(Array.isArray(sessions.sessions) ? sessions.sessions : []);
      renderTraces(Array.isArray(list.traces) ? list.traces : []);
      if (selectedId) await loadDetail(selectedId, false);
      $('status-line').textContent = 'Updated ' + new Date().toLocaleTimeString() + ' - ' + historyText(summary);
    } catch (err) {
      if (throwErrors) throw err;
      handleError(err);
    } finally {
      busy = false;
    }
  }

  function historyText(s) {
    const h = s.history && typeof s.history === 'object' ? s.history : {};
    const parts = [fmtInt(s.stored_traces) + ' request(s) in history'];
    if (h.enabled) {
      parts.push(h.loading ? 'loading saved history...' : 'saved to disk for ' + fmtInt(h.days) + ' day(s)');
      if (str(h.last_error)) parts.push('save error: ' + str(h.last_error));
    } else {
      parts.push('not saved to disk (save_history: false), lost on restart');
    }
    parts.push('attempt details for the newest ' + fmtInt(s.detail_traces));
    return parts.join(' · ');
  }

  // ---------- rendering ----------
  function renderSummary(s) {
    $('version').textContent = s.version ? 'v' + str(s.version) : '';
    $('m-requests').textContent = fmtInt(s.requests);
    const rangeLabel = RANGE_LABELS[str(s.range)] || RANGE_LABELS[currentRange()] || '';
    $('m-requests-hint').textContent = rangeLabel;
    const line = $('range-line');
    if (num(s.requests) > 0 && str(s.oldest_trace_at)) {
      line.textContent = 'Cards, sessions and requests below cover the ' + rangeLabel.replace(/^all/, 'requests') + ': ' +
        fmtInt(s.requests) + ' request(s) from ' + fmtTime(s.oldest_trace_at) + ' to ' + fmtTime(s.newest_trace_at) + '.';
    } else if (num(s.stored_traces) > 0) {
      line.textContent = 'No requests in the ' + rangeLabel + '. ' + fmtInt(s.stored_traces) + ' older request(s) are kept - choose a longer time range.';
    } else {
      line.textContent = '';
    }
    const cost = $('m-cost');
    cost.textContent = num(s.requests) > 0 ? fmtUSD(s.cost_usd) + (num(s.unpriced_requests) > 0 ? '+' : '') : '-';
    const savings = num(s.cache_savings_usd);
    const costHint = $('m-cost-hint');
    costHint.textContent = savings > 0 ? 'cache saved ' + fmtUSD(savings) : 'at API list prices';
    costHint.classList.toggle('ok', savings > 0);
    const unpriced = Array.isArray(s.unpriced_models) ? s.unpriced_models.map(str).filter((m) => m) : [];
    cost.title = 'Estimated at API list prices (' + str(s.prices_as_of) + '). Subscription (OAuth) accounts are not billed per token; ' +
      'this is what the same tokens would cost through the API. Without the prompt cache: ' + fmtUSD(s.cost_without_cache_usd) + '.' +
      (unpriced.length ? ' No price for: ' + unpriced.join(', ') + ' (' + fmtInt(s.unpriced_requests) + ' request(s) only partly counted). Add them under pricing in the plugin config.' : '');
    $('m-errors').textContent = fmtPercent(s.error_rate);
    $('m-errors-hint').textContent = fmtInt(s.errors) + ' failed' + (num(s.canceled) > 0 ? ' · ' + fmtInt(s.canceled) + ' canceled (not counted)' : '');
    $('m-latency').textContent = fmtDuration(s.avg_latency_ms) + ' / ' + fmtDuration(s.p95_latency_ms);
    $('m-tokens').textContent = fmtCompact(s.total_input_tokens) + ' / ' + fmtCompact(s.output_tokens);
    $('m-tokens').title = inputBreakdown(s) + '; ' + fmtInt(s.output_tokens) + ' output tokens';
    $('m-cache').textContent = num(s.total_input_tokens) > 0 ? fmtPercent(s.cache_hit_rate) : '-';
    $('m-cache-hint').textContent = num(s.total_input_tokens) > 0
      ? fmtCompact(s.cache_read_tokens) + ' read / ' + fmtCompact(s.total_input_tokens) + ' input'
      : 'cache reads / input';
    $('m-retried').textContent = fmtInt(s.retried_requests);
    renderProviders(Array.isArray(s.providers) ? s.providers : []);

    const pill = $('exporter');
    const exp = s.exporter && typeof s.exporter === 'object' ? s.exporter : {};
    pill.hidden = false;
    pill.title = '';
    if (!exp.enabled) {
      pill.textContent = 'Local only';
      pill.className = 'pill';
      pill.title = 'No OTLP endpoint configured; traces are only shown here.';
    } else if (str(exp.last_error)) {
      pill.textContent = 'Export error';
      pill.className = 'pill bad';
      pill.title = str(exp.last_error);
    } else {
      pill.textContent = 'Exporting to ' + (str(exp.endpoint_host) || 'OTLP endpoint');
      pill.className = 'pill ok';
      pill.title = fmtInt(exp.exported_spans) + ' spans exported, ' + fmtInt(exp.dropped_spans) + ' dropped';
    }
  }

  function renderProviders(providers) {
    const select = $('f-provider');
    const current = select.value;
    const fragment = document.createDocumentFragment();
    const all = el('option', 'All providers');
    all.value = '';
    fragment.append(all);
    const names = providers.map(str).filter((p) => p);
    if (current && !names.includes(current)) names.push(current);
    // Rebuilding on every refresh would close an open dropdown.
    const signature = names.join('\n');
    if (select.dataset.signature === signature) return;
    select.dataset.signature = signature;
    for (const name of names) {
      const option = el('option', name);
      option.value = name;
      fragment.append(option);
    }
    select.replaceChildren(fragment);
    select.value = current;
  }

  function renderSessions(sessions) {
    const rows = $('session-rows');
    const fragment = document.createDocumentFragment();
    for (const s of sessions) {
      const id = str(s.id);
      const tr = el('tr');
      tr.tabIndex = 0;
      tr.dataset.id = id;
      if (sessionFilter && sessionFilter.id === id) tr.className = 'selected';
      const when = el('td', fmtTime(s.last_at), 'time');
      when.append(el('span', (s.active ? 'active · ' : '') + 'started ' + fmtTime(s.started_at), 'session-tag'));
      tr.append(when);
      const model = el('td', str(s.model) || '(none)', 'model');
      model.title = str(s.serving_model) && str(s.serving_model) !== str(s.model) ? 'Served by ' + str(s.serving_model) : str(s.model);
      tr.append(model);
      const account = el('td', str(s.account) || '-', 'account');
      account.title = str(s.account);
      tr.append(account);
      const notes = [];
      if (num(s.errors) > 0) notes.push(fmtInt(s.errors) + ' failed');
      if (num(s.canceled) > 0) notes.push(fmtInt(s.canceled) + ' canceled');
      const requests = el('td', fmtInt(s.requests) + (notes.length ? ' (' + notes.join(', ') + ')' : ''), 'num');
      if (num(s.errors) > 0) requests.classList.add('failed-count');
      tr.append(requests);
      const ctx = el('td', undefined, 'ctx-cell');
      const gauge = ctxGauge(s.context_tokens, s.context_window, s.context_used);
      if (num(s.requests_until_full) >= 0 && num(s.context_window) > 0) {
        gauge.title += '. At the current growth, about ' + fmtInt(s.requests_until_full) + ' more request(s) fit.';
      }
      ctx.append(gauge);
      tr.append(ctx);
      const growth = el('td', num(s.growth_per_request) > 0 ? '+' + fmtCompact(s.growth_per_request) + '/req' : '-', 'num opt-2');
      growth.title = 'Prompt grew from ' + fmtInt(s.first_context_tokens) + ' to ' + fmtInt(s.context_tokens) + ' tokens (peak ' + fmtInt(s.peak_context_tokens) + ')';
      tr.append(growth);
      const cost = el('td', costText(s), 'num');
      cost.title = costTitle(s);
      tr.append(cost);
      tr.append(el('td', str(s.client_key) || '-', 'key opt-1'));
      const label = str(s.model) + ' · started ' + fmtTime(s.started_at);
      tr.addEventListener('click', () => toggleSession(id, label));
      tr.addEventListener('keydown', (event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          toggleSession(id, label);
        }
      });
      fragment.append(tr);
    }
    rows.replaceChildren(fragment);
    const empty = sessions.length === 0;
    $('sessions-empty').hidden = !empty;
    $('sessions-wrap').hidden = empty;
  }

  function toggleSession(id, label) {
    sessionFilter = sessionFilter && sessionFilter.id === id ? null : { id, label };
    renderSessionChip();
    refresh(false);
  }

  function renderSessionChip() {
    $('session-chip').hidden = !sessionFilter;
    $('session-chip-text').textContent = sessionFilter ? 'Session: ' + sessionFilter.label : '';
    for (const row of $('session-rows').children) row.classList.toggle('selected', !!sessionFilter && row.dataset.id === sessionFilter.id);
  }

  function renderTraces(traces) {
    const rows = $('rows');
    const fragment = document.createDocumentFragment();
    for (const t of traces) {
      const id = str(t.trace_id);
      const tr = el('tr');
      tr.tabIndex = 0;
      tr.dataset.id = id;
      if (id === selectedId) tr.className = 'selected';
      const time = el('td', fmtTime(t.started_at), 'time');
      if (num(t.session_requests) > 1) time.append(el('span', 'session request ' + fmtInt(t.session_seq) + ' of ' + fmtInt(t.session_requests), 'session-tag'));
      tr.append(time);
      const model = el('td', str(t.requested_model) || '(none)', 'model');
      model.title = str(t.requested_model);
      tr.append(model);
      const provider = el('td', str(t.provider) || '-', 'provider opt-2');
      provider.title = str(t.provider);
      tr.append(provider);
      const account = el('td', str(t.account) || '-', 'account');
      account.title = str(t.account);
      tr.append(account);
      const status = el('td');
      status.append(statusBadge(t));
      tr.append(status);
      tr.append(el('td', fmtDuration(t.duration_ms), 'num'));
      const attempts = el('td', attemptsText(t), 'num');
      if (num(t.failed_attempts) > 0) attempts.classList.add('failed-count');
      tr.append(attempts);
      const tokens = el('td', fmtCompact(t.total_input_tokens) + ' / ' + fmtCompact(t.output_tokens), 'num');
      tokens.title = inputBreakdown(t) + '; ' + fmtInt(t.output_tokens) + ' output tokens';
      tr.append(tokens);
      const cache = el('td', cacheText(t), 'cache opt-2');
      if (num(t.total_input_tokens) > 0) cache.title = 'Cache hit rate ' + fmtPercent(t.cache_hit_rate);
      tr.append(cache);
      const ctx = el('td', undefined, 'ctx-cell opt-2');
      ctx.append(ctxGauge(t.context_tokens, t.context_window, t.context_used));
      tr.append(ctx);
      const cost = el('td', costText(t), 'num');
      cost.title = costTitle(t);
      tr.append(cost);
      tr.append(el('td', str(t.client_key) || '-', 'key opt-1'));
      tr.addEventListener('click', () => select(id));
      tr.addEventListener('keydown', (event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          select(id);
        }
      });
      fragment.append(tr);
    }
    rows.replaceChildren(fragment);
    const empty = traces.length === 0;
    $('empty').hidden = !empty;
    $('table-wrap').hidden = empty;
    if (empty) {
      const filtered = $('f-status').value !== 'all' || $('f-model').value.trim() || $('f-provider').value || $('f-slow').checked || !!sessionFilter;
      $('empty').firstElementChild.textContent = filtered ? 'No traces match these filters' : 'No traces yet';
    }
  }

  function select(id) {
    selectedId = id;
    for (const row of $('rows').children) row.classList.toggle('selected', row.dataset.id === id);
    loadDetail(id, true).catch(handleError);
  }

  async function loadDetail(id, scroll) {
    let body;
    try {
      body = await api('/trace?id=' + encodeURIComponent(id));
    } catch (err) {
      if (err.status === 404) {
        // Evicted from the ring buffer since the list was loaded.
        if (selectedId === id) closeDetail();
        return;
      }
      throw err;
    }
    if (selectedId !== id || !body.trace) return;
    renderDetail(body.trace);
    if (scroll) $('detail').scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  function closeDetail() {
    selectedId = '';
    $('detail').hidden = true;
    for (const row of $('rows').children) row.classList.remove('selected');
  }

  function metaItem(list, label, value, mono) {
    const wrap = el('div');
    wrap.append(el('dt', label));
    const dd = el('dd', value === '' ? '-' : value);
    if (mono) dd.className = 'mono';
    wrap.append(dd);
    list.append(wrap);
  }

  function renderDetail(t) {
    $('detail').hidden = false;
    $('d-title').textContent = str(t.requested_model) || '(no model)';
    const badges = $('d-badges');
    badges.replaceChildren(statusBadge(t));
    if (t.stream) badges.append(el('span', 'stream', 'badge'));
    if (num(t.attempt_count) > 1) badges.append(el('span', 'retried', 'badge warn'));

    const meta = $('d-meta');
    meta.replaceChildren();
    metaItem(meta, 'Trace ID', str(t.trace_id), true);
    metaItem(meta, 'CPA request', str(t.cpa_trace_id), true);
    metaItem(meta, 'Started', fmtTime(t.started_at));
    metaItem(meta, 'Duration', fmtDuration(t.duration_ms));
    metaItem(meta, 'Client format', str(t.source_format));
    metaItem(meta, 'Attempts', attemptsText(t));
    metaItem(meta, 'Provider', str(t.provider));
    metaItem(meta, 'Account', str(t.account));
    metaItem(meta, 'Tokens in / out', fmtInt(t.total_input_tokens) + ' / ' + fmtInt(t.output_tokens));
    metaItem(meta, 'Input breakdown', fmtInt(uncachedOf(t)) + ' uncached · ' + fmtInt(t.cache_read_tokens) +
      ' cache read · ' + fmtInt(t.cache_creation_tokens) + ' cache write');
    metaItem(meta, 'Cache hit rate', num(t.total_input_tokens) > 0 ? fmtPercent(t.cache_hit_rate) : '-');
    metaItem(meta, 'Est. cost', costText(t) + (num(t.cost_without_cache_usd) > num(t.cost_usd) ? ' (without cache ' + fmtUSD(t.cost_without_cache_usd) + ')' : ''));
    metaItem(meta, 'Context used', num(t.context_window) > 0 && num(t.context_tokens) > 0
      ? fmtInt(t.context_tokens) + ' of ' + fmtInt(t.context_window) + ' (' + fmtPercent(t.context_used) + ')'
      : (num(t.context_tokens) > 0 ? fmtInt(t.context_tokens) + ' (window unknown)' : ''));
    metaItem(meta, 'Session', num(t.session_requests) > 0 ? 'request ' + fmtInt(t.session_seq) + ' of ' + fmtInt(t.session_requests) : '');
    metaItem(meta, 'Client key', str(t.client_key), true);

    const error = $('d-error');
    error.textContent = str(t.error);
    error.hidden = !str(t.error);

    const spans = Array.isArray(t.spans) ? t.spans : [];
    const more = $('d-more');
    if (t.has_detail === false) {
      $('d-waterfall').replaceChildren(el('p', 'Attempt details are kept only for the newest requests. This older request keeps its totals, cost and session.', 'muted'));
      $('d-attempts').replaceChildren();
      more.hidden = true;
      return;
    }
    renderWaterfall(t, spans);
    const attempts = spans.filter((s) => str(s.kind) === 'client');
    const hidden = num(t.attempt_count) - attempts.length;
    more.hidden = hidden <= 0;
    more.textContent = hidden > 0 ? hidden + ' more attempt(s) are counted but not kept in memory.' : '';
    renderAttempts(attempts);
  }

  function renderWaterfall(t, spans) {
    const box = $('d-waterfall');
    box.replaceChildren();
    let total = num(t.duration_ms);
    for (const s of spans) total = Math.max(total, num(s.start_offset_ms) + num(s.duration_ms));
    if (total <= 0) total = 1;
    const pct = (ms) => Math.min(100, Math.max(0, (num(ms) / total) * 100));
    let ordinal = 0;
    for (const s of spans) {
      const isRoot = !str(s.parent_span_id);
      const failed = str(s.status) === 'error';
      const row = el('div', undefined, 'wf-row');
      const attrs = s.attributes && typeof s.attributes === 'object' ? s.attributes : {};
      let label;
      if (isRoot && str(s.kind) === 'server') {
        label = 'Request';
      } else {
        ordinal += 1;
        label = '#' + str(attrs['cpa.attempt'] || ordinal) + ' ' + (str(s.provider) || str(s.name));
      }
      const labelNode = el('div', label, 'wf-label');
      labelNode.title = str(s.name) + (str(s.account) ? ' - ' + str(s.account) : '');
      const track = el('div', undefined, 'wf-track');
      const bar = el('div', undefined, 'wf-bar' + (isRoot && str(s.kind) === 'server' ? ' root' : '') + (failed ? ' failed' : ''));
      // CSSOM property writes are allowed by the CSP (no inline style attributes).
      bar.style.left = pct(s.start_offset_ms) + '%';
      bar.style.width = Math.max(pct(s.duration_ms), 0.4) + '%';
      bar.title = fmtDuration(s.duration_ms) + (failed ? ' - failed' : '');
      track.append(bar);
      for (const e of Array.isArray(s.events) ? s.events : []) {
        const mark = el('div', undefined, 'wf-mark');
        mark.style.left = pct(e.offset_ms) + '%';
        mark.title = str(e.name) + ' at ' + fmtDuration(num(e.offset_ms) - num(s.start_offset_ms)) + ' after attempt start';
        track.append(mark);
      }
      row.append(labelNode, track, el('div', fmtDuration(s.duration_ms), 'wf-time'));
      box.append(row);
    }
  }

  function kv(list, label, value) {
    if (value === '' || value === undefined || value === null) return;
    list.append(el('dt', label), el('dd', value));
  }

  function renderAttempts(attempts) {
    const box = $('d-attempts');
    box.replaceChildren();
    if (attempts.length === 0) {
      box.append(el('p', 'No upstream attempt was recorded (for example, the request was rejected before reaching a provider).', 'muted'));
      return;
    }
    attempts.forEach((s, index) => {
      const a = s.attributes && typeof s.attributes === 'object' ? s.attributes : {};
      const failed = str(s.status) === 'error';
      const card = el('div', undefined, 'attempt' + (failed ? ' failed' : ''));
      const head = el('h4', 'Attempt ' + str(a['cpa.attempt'] || index + 1));
      head.append(el('span', failed ? 'failed' : 'ok', 'badge ' + (failed ? 'bad' : 'ok')));
      card.append(head);
      const list = el('dl', undefined, 'kv');
      const tok = s.tokens && typeof s.tokens === 'object' ? s.tokens : {};
      kv(list, 'Provider', str(s.provider));
      kv(list, 'Account', str(s.account));
      kv(list, 'Model', str(a['gen_ai.request.model']));
      kv(list, 'Response model', str(a['gen_ai.response.model']));
      kv(list, 'Status code', a['http.response.status_code'] !== undefined ? str(a['http.response.status_code']) : '');
      kv(list, 'Started', '+' + fmtDuration(s.start_offset_ms));
      kv(list, 'Latency', fmtDuration(s.duration_ms));
      kv(list, 'TTFT', a['cpa.ttft_ms'] !== undefined ? fmtDuration(a['cpa.ttft_ms']) : '');
      kv(list, 'Input (total)', fmtInt(tok.total_input_tokens));
      kv(list, 'Uncached input', fmtInt(tok.uncached_input_tokens));
      kv(list, 'Cache read', fmtInt(tok.cache_read_tokens));
      kv(list, 'Cache write', fmtInt(tok.cache_creation_tokens));
      kv(list, 'Cache hit rate', num(tok.total_input_tokens) > 0 ? fmtPercent(tok.cache_hit_rate) : '');
      kv(list, 'Output', fmtInt(tok.output_tokens));
      kv(list, 'Reasoning', num(tok.reasoning_tokens) > 0 ? fmtInt(tok.reasoning_tokens) : '');
      kv(list, 'Est. cost', s.cost_usd !== undefined && s.cost_usd !== null ? fmtUSD(s.cost_usd) : '');
      kv(list, 'Auth type', str(a['cpa.auth_type']));
      kv(list, 'Auth index', str(a['cpa.auth_index']));
      kv(list, 'Error', str(a['cpa.error']));
      card.append(list);

      const details = el('details');
      details.append(el('summary', 'All attributes'));
      const table = el('table', undefined, 'attr-table');
      const tbody = el('tbody');
      for (const key of Object.keys(a).sort()) {
        const row = el('tr');
        row.append(el('td', key), el('td', str(a[key])));
        tbody.append(row);
      }
      table.append(tbody);
      details.append(table);
      card.append(details);
      box.append(card);
    });
  }

  async function copyTraceID() {
    const id = selectedId;
    if (!id) return;
    const button = $('copy-id');
    let ok = false;
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(id);
        ok = true;
      }
    } catch (err) {
      ok = false;
    }
    if (!ok) {
      // Fallback for iframes without clipboard permission.
      const input = el('input');
      input.value = id;
      input.readOnly = true;
      input.className = 'mono';
      document.body.append(input);
      input.select();
      try {
        ok = document.execCommand('copy');
      } catch (err) {
        ok = false;
      }
      input.remove();
    }
    button.textContent = ok ? 'Copied' : 'Copy failed';
    setTimeout(() => { button.textContent = 'Copy trace ID'; }, 1500);
  }

  // ---------- wiring ----------
  function init() {
    $('connect-btn').addEventListener('click', connect);
    $('key').addEventListener('keydown', (event) => {
      if (event.key === 'Enter') connect();
    });
    $('disconnect').addEventListener('click', () => disconnect(''));
    $('refresh').addEventListener('click', () => refresh(false));
    $('f-status').addEventListener('change', () => refresh(false));
    $('f-slow').addEventListener('change', () => refresh(false));
    $('f-provider').addEventListener('change', () => refresh(false));
    $('f-range').addEventListener('change', () => {
      const rangeStore = storage();
      try {
        if (rangeStore) rangeStore.setItem(RANGE_KEY, currentRange());
      } catch (err) {
        // Storage may be blocked; the choice then lasts for this page only.
      }
      refresh(false);
    });
    $('session-clear').addEventListener('click', () => {
      sessionFilter = null;
      renderSessionChip();
      refresh(false);
    });
    $('f-model').addEventListener('input', () => {
      clearTimeout(modelDebounce);
      modelDebounce = setTimeout(() => refresh(false), 300);
    });
    $('f-auto').addEventListener('change', schedule);
    $('close-detail').addEventListener('click', closeDetail);
    $('copy-id').addEventListener('click', copyTraceID);
    document.addEventListener('visibilitychange', () => {
      if (!managementKey) return;
      if (document.visibilityState === 'visible' && $('f-auto').checked) refresh(false);
      schedule();
    });

    try {
      const rangeStore = storage();
      const savedRange = rangeStore ? rangeStore.getItem(RANGE_KEY) : null;
      if (savedRange && Object.prototype.hasOwnProperty.call(RANGE_LABELS, savedRange)) $('f-range').value = savedRange;
    } catch (err) {
      // Storage blocked: keep the default range.
    }
    const store = storage();
    const remembered = store ? store.getItem(SESSION_KEY) : null;
    if (remembered) {
      $('remember').checked = true;
      $('key').value = remembered;
      connect();
    } else {
      $('key').focus();
    }
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
