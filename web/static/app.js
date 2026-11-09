// Content Search dashboard: a single page over the JSON API, written in plain
// ES2020 with no dependencies and no build step.
//
// Titles, tags and provider names come from third-party feeds, so every value
// reaches the document through textContent or DOM properties; the script
// never parses a markup string.
(() => {
  'use strict';

  const API = '/api/v1';
  const DEBOUNCE_MS = 300;
  const TOAST_MS = 5000;
  const SYNC_POLL_MS = 2000;
  const SYNC_POLL_WINDOW_MS = 30000;
  const SYNC_REFRESH_MS = 60000;
  const MAX_PAGE = 10000;

  const TYPES = ['', 'video', 'text'];
  const SORTS = ['score', 'relevance', 'published_at'];
  const ORDERS = ['desc', 'asc'];
  const PER_PAGES = [10, 20, 50];
  const DEFAULTS = { q: '', type: '', sort: 'score', order: 'desc', page: 1, per_page: 20 };

  const byId = (id) => document.getElementById(id);
  const el = {
    form: byId('controls'),
    q: byId('q'),
    type: byId('type'),
    sort: byId('sort'),
    order: byId('order'),
    perPage: byId('per-page'),
    error: byId('error'),
    loading: byId('loading'),
    results: byId('results'),
    empty: byId('empty'),
    summary: byId('summary'),
    prev: byId('prev'),
    next: byId('next'),
    syncStatus: byId('sync-status'),
    syncButton: byId('sync-button'),
    toast: byId('toast'),
  };
  const relevanceOption = el.sort.querySelector('option[value="relevance"]');
  const relativeTimeFormat = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });
  const TIME_UNITS = [['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]];

  // ApiError carries the RFC 7807 problem document of a failed request.
  class ApiError extends Error {
    constructor(status, problem, retryAfter) {
      super(`HTTP ${status}`);
      this.name = 'ApiError';
      this.status = status;
      this.problem = problem && typeof problem === 'object' ? problem : {};
      this.retryAfter = retryAfter;
    }

    get detail() {
      const text = this.problem.detail || this.problem.title || this.message;
      return this.retryAfter ? `${text} (retry after ${this.retryAfter}s)` : text;
    }

    get fieldErrors() {
      const errors = this.problem.errors;
      return errors && typeof errors === 'object' ? Object.entries(errors) : [];
    }
  }

  // request performs a JSON call and turns non-2xx responses into ApiError.
  async function request(path, options = {}) {
    const response = await fetch(path, { headers: { Accept: 'application/json' }, ...options });
    const text = await response.text();
    let body = null;
    try {
      body = text ? JSON.parse(text) : null;
    } catch {
      body = null;
    }
    if (!response.ok) {
      throw new ApiError(response.status, body, response.headers.get('Retry-After'));
    }
    return body;
  }

  function messageOf(err) {
    if (err instanceof ApiError) return err.detail;
    if (err instanceof TypeError) return 'The API could not be reached.';
    return err.message || String(err);
  }

  // ---- Search state, mirrored in the query string so a reload keeps it ----

  function readState() {
    const params = new URLSearchParams(window.location.search);
    const pick = (key, allowed) => (allowed.includes(params.get(key)) ? params.get(key) : DEFAULTS[key]);
    const page = parseInt(params.get('page'), 10);
    const perPage = parseInt(params.get('per_page'), 10);
    return normalize({
      q: (params.get('q') || '').trim(),
      type: pick('type', TYPES),
      sort: pick('sort', SORTS),
      order: pick('order', ORDERS),
      page: page >= 1 ? Math.min(page, MAX_PAGE) : DEFAULTS.page,
      per_page: PER_PAGES.includes(perPage) ? perPage : DEFAULTS.per_page,
    });
  }

  // normalize applies the one cross-field rule: relevance needs a query.
  function normalize(next) {
    if (next.sort === 'relevance' && !next.q) next.sort = 'score';
    return next;
  }

  const state = readState();

  function writeState() {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(state)) {
      if (value !== DEFAULTS[key]) params.set(key, String(value));
    }
    const query = params.toString();
    window.history.replaceState(null, '', query ? `?${query}` : window.location.pathname);
  }

  function syncControls() {
    // Leave the input alone while it still agrees with the state, so the
    // caret and a trailing space the user is typing survive a refresh.
    if (el.q.value.trim() !== state.q) el.q.value = state.q;
    el.type.value = state.type;
    el.sort.value = state.sort;
    relevanceOption.disabled = !state.q;
    el.order.textContent = state.order === 'asc' ? 'Ascending' : 'Descending';
    el.perPage.value = String(state.per_page);
  }

  function update(patch) {
    Object.assign(state, patch);
    normalize(state);
    syncControls();
    writeState();
    search();
  }

  // ---- Searching and rendering ----

  let inflight = null;

  async function search() {
    if (inflight) inflight.abort();
    const controller = new AbortController();
    inflight = controller;
    setLoading(true);
    el.error.hidden = true;
    try {
      const body = await request(`${API}/contents?${searchParams()}`, { signal: controller.signal });
      render(body);
    } catch (err) {
      if (err.name === 'AbortError') return;
      renderRows([]);
      el.empty.hidden = true;
      el.summary.textContent = '';
      el.prev.disabled = true;
      el.next.disabled = true;
      showError(err);
    } finally {
      if (inflight === controller) {
        inflight = null;
        setLoading(false);
      }
    }
  }

  function searchParams() {
    const params = new URLSearchParams({
      sort: state.sort,
      order: state.order,
      page: String(state.page),
      per_page: String(state.per_page),
    });
    if (state.q) params.set('q', state.q);
    if (state.type) params.set('type', state.type);
    return params.toString();
  }

  function render(body) {
    const items = Array.isArray(body && body.data) ? body.data : [];
    const meta = (body && body.meta) || {};
    const total = Number.isInteger(meta.total) ? meta.total : items.length;
    const totalPages = Number.isInteger(meta.total_pages) ? meta.total_pages : Math.ceil(total / state.per_page);
    if (items.length === 0 && total > 0 && state.page > 1) {
      // The page ran past the end, e.g. items vanished since the URL was
      // shared; show the last page instead of an empty one.
      update({ page: Math.max(1, totalPages) });
      return;
    }
    renderRows(items);
    el.empty.textContent = items.length === 0 ? emptyMessage(total) : '';
    el.empty.hidden = items.length > 0;
    const pages = Math.max(totalPages, 1);
    el.summary.textContent = `Page ${state.page} of ${pages} - ${total} ${total === 1 ? 'result' : 'results'}`;
    el.prev.disabled = state.page <= 1;
    el.next.disabled = state.page >= pages;
  }

  function emptyMessage(total) {
    if (total === 0 && !state.q && !state.type) return 'No content yet - click Sync now to pull the providers.';
    if (state.q) return `No results for "${state.q}".`;
    return 'No results match the current filters.';
  }

  function renderRows(items) {
    el.results.replaceChildren(...items.map(renderRow));
  }

  function renderRow(item) {
    const tr = document.createElement('tr');
    tr.setAttribute('role', 'row');
    tr.append(
      cell('Title', titleLink(item), 'title'),
      cell('Type', badge(item.type)),
      cell('Score', scoreLabel(item), 'num'),
      cell('Published', formatDate(item.published_at)),
      cell('Provider', item.provider || '-'),
      cell('Tags', tagList(item.tags)),
    );
    return tr;
  }

  // cell wraps content (a node or plain text) in a td; the label feeds the
  // stacked layout on narrow screens.
  function cell(label, content, className) {
    const td = document.createElement('td');
    td.setAttribute('role', 'cell');
    td.dataset.label = label;
    if (className) td.className = className;
    td.append(content);
    return td;
  }

  function titleLink(item) {
    const a = document.createElement('a');
    a.textContent = typeof item.title === 'string' && item.title ? item.title : '(untitled)';
    if (typeof item.id === 'string' && item.id) a.href = `${API}/contents/${encodeURIComponent(item.id)}`;
    return a;
  }

  function badge(type) {
    const span = document.createElement('span');
    const kind = type === 'video' || type === 'text' ? type : 'other';
    span.className = `badge badge-${kind}`;
    span.textContent = typeof type === 'string' && type ? type : 'unknown';
    return span;
  }

  function scoreLabel(item) {
    const span = document.createElement('span');
    span.className = 'score';
    span.textContent = Number.isFinite(item.score) ? item.score.toFixed(2) : '-';
    const b = item.score_breakdown;
    if (b && typeof b === 'object') {
      const terms = [b.base, b.type_coefficient, b.freshness, b.engagement].map(formatNumber);
      span.title = `base x coefficient + freshness + engagement\n${terms[0]} x ${terms[1]} + ${terms[2]} + ${terms[3]}`;
    }
    return span;
  }

  // formatNumber prints at most two decimals without trailing zeros.
  function formatNumber(value) {
    return Number.isFinite(value) ? String(Number(value.toFixed(2))) : '?';
  }

  function formatDate(iso) {
    const t = Date.parse(iso);
    return Number.isNaN(t) ? '-' : new Date(t).toISOString().slice(0, 10);
  }

  function tagList(tags) {
    const names = Array.isArray(tags) ? tags.filter((tag) => typeof tag === 'string' && tag) : [];
    if (names.length === 0) return '-';
    const ul = document.createElement('ul');
    ul.className = 'tags';
    for (const name of names) {
      const li = document.createElement('li');
      li.className = 'chip';
      li.textContent = name;
      ul.append(li);
    }
    return ul;
  }

  function setLoading(on) {
    el.loading.hidden = !on;
    el.results.setAttribute('aria-busy', String(on));
  }

  function showError(err) {
    const detail = document.createElement('p');
    detail.textContent = messageOf(err);
    el.error.replaceChildren(detail);
    const fields = err instanceof ApiError ? err.fieldErrors : [];
    if (fields.length > 0) {
      const list = document.createElement('ul');
      for (const [field, message] of fields) {
        const li = document.createElement('li');
        li.textContent = `${field}: ${message}`;
        list.append(li);
      }
      el.error.append(list);
    }
    el.error.hidden = false;
  }

  // ---- Synchronisation status and trigger ----

  let knownRuns = [];
  const isRunning = (run) => run.status === 'running';

  async function refreshSyncStatus() {
    try {
      const body = await request(`${API}/sync/runs?limit=10`);
      knownRuns = Array.isArray(body && body.data) ? body.data : [];
      el.syncStatus.textContent = syncStatusText(knownRuns);
      return knownRuns;
    } catch (err) {
      el.syncStatus.textContent = `Last sync: unknown (${messageOf(err)})`;
      return null;
    }
  }

  // syncStatusText summarises the newest batch. Runs arrive newest first and
  // the runs of one trigger (one per provider) share a batch_id.
  function syncStatusText(runs) {
    if (runs.some(isRunning)) return 'Last sync: running...';
    const latest = runs.find((run) => run.finished_at);
    if (!latest) return 'Last sync: never';
    const batch = latest.batch_id ? runs.filter((run) => run.batch_id === latest.batch_id) : [latest];
    const fetched = batch.reduce((sum, run) => sum + (Number.isInteger(run.fetched) ? run.fetched : 0), 0);
    const problem = batch.find((run) => run.status !== 'succeeded');
    const status = problem ? problem.status || 'unknown' : 'succeeded';
    return `Last sync: ${relativeTime(latest.finished_at)} - ${status} (${fetched} items fetched)`;
  }

  function relativeTime(iso) {
    const t = Date.parse(iso);
    if (Number.isNaN(t)) return 'at an unknown time';
    const seconds = Math.round((t - Date.now()) / 1000);
    const [unit, size] = TIME_UNITS.find(([, s]) => Math.abs(seconds) >= s) || TIME_UNITS[TIME_UNITS.length - 1];
    return relativeTimeFormat.format(Math.trunc(seconds / size), unit);
  }

  async function triggerSync() {
    el.syncButton.disabled = true;
    try {
      const body = await request(`${API}/sync`, { method: 'POST' });
      const providers = Array.isArray(body && body.providers) ? body.providers.join(', ') : '';
      toast(providers ? `Sync started for ${providers}.` : 'Sync started.');
      pollSyncStatus();
    } catch (err) {
      const alreadyRunning = err instanceof ApiError && err.status === 409;
      toast(messageOf(err), !alreadyRunning);
      if (alreadyRunning) pollSyncStatus();
    } finally {
      el.syncButton.disabled = false;
    }
  }

  let pollTimer = 0;

  // pollSyncStatus follows a sync that was just triggered: it refreshes the
  // status line every two seconds and reloads the results once the new runs
  // have finished, giving up after thirty seconds.
  function pollSyncStatus() {
    const knownMaxID = Math.max(0, ...knownRuns.map((run) => Number(run.id) || 0));
    const deadline = Date.now() + SYNC_POLL_WINDOW_MS;
    let sawRunning = false;
    clearTimeout(pollTimer);
    const tick = async () => {
      const runs = await refreshSyncStatus();
      if (runs) {
        const running = runs.some(isRunning);
        sawRunning = sawRunning || running;
        const finishedNew = runs.some((run) => !isRunning(run) && Number(run.id) > knownMaxID);
        if (!running && (sawRunning || finishedNew)) {
          search();
          return;
        }
      }
      if (Date.now() < deadline) pollTimer = setTimeout(tick, SYNC_POLL_MS);
    };
    tick();
  }

  let toastTimer = 0;

  function toast(message, isError = false) {
    el.toast.textContent = message;
    el.toast.classList.toggle('toast-error', isError);
    el.toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
      el.toast.hidden = true;
    }, TOAST_MS);
  }

  // ---- Wiring ----

  let debounceTimer = 0;

  function applyQuery() {
    clearTimeout(debounceTimer);
    const q = el.q.value.trim();
    if (q !== state.q) update({ q, page: 1 });
  }

  el.q.addEventListener('input', () => {
    clearTimeout(debounceTimer);
    debounceTimer = setTimeout(applyQuery, DEBOUNCE_MS);
  });
  el.form.addEventListener('submit', (event) => {
    event.preventDefault();
    applyQuery();
  });
  el.type.addEventListener('change', () => update({ type: el.type.value, page: 1 }));
  el.sort.addEventListener('change', () => update({ sort: el.sort.value, page: 1 }));
  el.order.addEventListener('click', () => update({ order: state.order === 'desc' ? 'asc' : 'desc', page: 1 }));
  el.perPage.addEventListener('change', () => update({ per_page: Number(el.perPage.value), page: 1 }));
  el.prev.addEventListener('click', () => update({ page: Math.max(1, state.page - 1) }));
  el.next.addEventListener('click', () => update({ page: Math.min(state.page + 1, MAX_PAGE) }));
  el.syncButton.addEventListener('click', triggerSync);

  syncControls();
  writeState();
  search();
  refreshSyncStatus();
  setInterval(refreshSyncStatus, SYNC_REFRESH_MS);
})();
