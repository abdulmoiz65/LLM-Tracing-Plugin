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
  function fmtPercent(rate) {
    return (num(rate) * 100).toFixed(num(rate) > 0 && num(rate) < 0.1 ? 1 : 0) + '%';
  }
  function isError(t) {
    return str(t.outcome) !== 'succeeded' || num(t.status_code) >= 400;
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
    $('rows').replaceChildren();
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
    return '/traces?' + params.toString();
  }

  async function refresh(throwErrors) {
    if (busy) return;
    busy = true;
    try {
      const [summary, list] = await Promise.all([api('/summary'), api(listQuery())]);
      renderSummary(summary);
      renderTraces(Array.isArray(list.traces) ? list.traces : []);
      if (selectedId) await loadDetail(selectedId, false);
      $('status-line').textContent = 'Updated ' + new Date().toLocaleTimeString() +
        ' - ' + fmtInt(summary.stored_traces) + ' of ' + fmtInt(summary.capacity) + ' recent traces kept in memory';
    } catch (err) {
      if (throwErrors) throw err;
      handleError(err);
    } finally {
      busy = false;
    }
  }

  // ---------- rendering ----------
  function renderSummary(s) {
    $('version').textContent = s.version ? 'v' + str(s.version) : '';
    $('m-requests').textContent = fmtInt(s.requests);
    $('m-errors').textContent = fmtPercent(s.error_rate);
    $('m-errors-hint').textContent = fmtInt(s.errors) + ' failed';
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

  function renderTraces(traces) {
    const rows = $('rows');
    const fragment = document.createDocumentFragment();
    for (const t of traces) {
      const id = str(t.trace_id);
      const tr = el('tr');
      tr.tabIndex = 0;
      tr.dataset.id = id;
      if (id === selectedId) tr.className = 'selected';
      tr.append(el('td', fmtTime(t.started_at), 'time'));
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
      const filtered = $('f-status').value !== 'all' || $('f-model').value.trim() || $('f-provider').value || $('f-slow').checked;
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
    metaItem(meta, 'Client key', str(t.client_key), true);

    const error = $('d-error');
    error.textContent = str(t.error);
    error.hidden = !str(t.error);

    const spans = Array.isArray(t.spans) ? t.spans : [];
    renderWaterfall(t, spans);
    const attempts = spans.filter((s) => str(s.kind) === 'client');
    const more = $('d-more');
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
