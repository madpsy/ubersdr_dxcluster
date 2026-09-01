'use strict';

// search.js — the spot archive search modal.
//
// The modal is a front end for GET /api/search and nothing else: every control
// maps to one query parameter, and the "API URL" button hands over the exact
// URL the modal just used. So anything you can find here you can automate, and
// anything the API grows shows up here by adding one control.
//
// Every fixed picker (band, mode, source, continent, sort key) is built from
// window.SEARCH_META, which the server injects into the page — so the modal
// opens fully populated with no network round-trip. Only the country list is
// fetched, and app.js already has it loaded by then.

// ── Configuration from the server ──────────────────────────────────────────

const SMETA = window.SEARCH_META || {};
const SLIMITS = SMETA.limits || {};

// Period presets. Anything longer than the server's retention is pointless, so
// the list stops at the window the API will actually honour.
const SEARCH_PERIODS = [
  { key: '1h',  label: '1 h',   params: { hours: '1' } },
  { key: '6h',  label: '6 h',   params: { hours: '6' } },
  { key: '24h', label: '24 h',  params: { hours: '24' } },
  { key: '7d',  label: '7 d',   params: { days: '7' } },
  { key: '30d', label: '30 d',  params: { days: '30' } },
  { key: 'all', label: 'All',   params: { days: String(SLIMITS.max_window_days || 400) } },
  { key: 'custom', label: 'Custom', params: null },
];

// ── State ──────────────────────────────────────────────────────────────────

const searchState = {
  period: '24h',
  bands: new Set(),
  modes: new Set(),
  streams: new Set(),
  continents: new Set(),
  countries: new Map(),  // code → display name
  open: false,
  built: false,
  // Paging: the query that produced the current table, plus where to resume.
  lastQuery: null,
  nextCursor: null,
  nextOffset: null,
  inFlight: null,        // AbortController for the running request
  rows: 0,
};

let searchDebounce = null;

// ── Open / close ───────────────────────────────────────────────────────────

function openSearchModal() {
  const firstOpen = !searchState.built;
  buildSearchForm();
  const ov = document.getElementById('search-overlay');
  if (ov) ov.classList.add('open');
  searchState.open = true;
  const first = document.getElementById('sq-callsign');
  if (first) first.focus();
  // Open onto the last 24 hours rather than an empty table: the default search
  // is the cheapest one there is, and it shows what the controls do.
  if (firstOpen) runSearch();
}

function closeSearchModal() {
  const ov = document.getElementById('search-overlay');
  if (ov) ov.classList.remove('open');
  searchState.open = false;
  abortSearch();
}

function abortSearch() {
  if (searchState.inFlight) {
    searchState.inFlight.abort();
    searchState.inFlight = null;
  }
}

// ── Form construction ──────────────────────────────────────────────────────
//
// Built once, on first open, from the injected metadata. Everything here is
// synchronous — there is nothing to wait for.

function buildSearchForm() {
  if (searchState.built) return;
  searchState.built = true;

  chipGroup('sq-period', SEARCH_PERIODS.map(p => ({ value: p.key, label: p.label })),
    { single: true, selected: searchState.period, onChange: onPeriodChange });

  chipGroup('sq-band', (SMETA.bands || []).map(b => ({ value: b, label: b })),
    { set: searchState.bands, onChange: scheduleSearch });

  // Modes come grouped by the stream that produces them, so the picker reads
  // Digital → CW → Voice rather than as one undifferentiated wall of chips.
  const modeChips = [];
  (SMETA.mode_groups || []).forEach((g, i) => {
    if (i > 0) modeChips.push({ divider: true });
    (g.modes || []).forEach(m => modeChips.push({ value: m, label: m, title: g.label }));
  });
  chipGroup('sq-mode', modeChips, { set: searchState.modes, onChange: scheduleSearch });

  const labels = SMETA.stream_labels || {};
  chipGroup('sq-stream', (SMETA.streams || []).map(s => ({ value: s, label: labels[s] || s })),
    { set: searchState.streams, onChange: scheduleSearch });

  chipGroup('sq-continent', (SMETA.continents || []).map(c => ({ value: c.code, label: c.code, title: c.name })),
    { set: searchState.continents, onChange: scheduleSearch });

  // Sort keys, with each one's natural direction remembered so picking
  // "Distance" gives furthest-first without a second click.
  const sortSel = document.getElementById('sq-sort');
  if (sortSel) {
    (SMETA.sorts || []).forEach(s => {
      const opt = document.createElement('option');
      opt.value = s.key;
      opt.textContent = s.label;
      opt.dataset.defaultOrder = s.default_order || 'desc';
      if (s.key === 'ts') opt.selected = true;
      sortSel.appendChild(opt);
    });
    sortSel.addEventListener('change', () => {
      const opt = sortSel.selectedOptions[0];
      const ord = document.getElementById('sq-order');
      if (opt && ord) ord.value = opt.dataset.defaultOrder || 'desc';
      scheduleSearch();
    });
  }

  buildSearchHead();
  populateCountryList();

  // Text inputs run the search on a pause in typing; pickers run it at once.
  ['sq-callsign', 'sq-spotter', 'sq-locator', 'sq-text'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener('input', scheduleSearch);
  });
  ['sq-snr-min', 'sq-snr-max', 'sq-freq-min', 'sq-freq-max',
   'sq-dist-min', 'sq-dist-max', 'sq-wpm-min', 'sq-wpm-max',
   'sq-hour-min', 'sq-hour-max', 'sq-from', 'sq-to'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener('input', scheduleSearch);
  });
  ['sq-callsign-exact', 'sq-spotter-exact', 'sq-order', 'sq-limit'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener('change', scheduleSearch);
  });

  const country = document.getElementById('sq-country');
  if (country) country.addEventListener('change', onCountryPicked);

  // Enter runs the search from anywhere in the form.
  const form = document.getElementById('search-form');
  if (form) {
    form.addEventListener('keydown', e => {
      if (e.key === 'Enter') { e.preventDefault(); runSearch(); }
    });
  }
}

// chipGroup renders a row of toggle chips into a container.
//
// opts.set   — a Set the chips add to and remove from (multi-select)
// opts.single — radio behaviour, tracked by opts.selected instead
function chipGroup(containerId, items, opts) {
  const box = document.getElementById(containerId);
  if (!box) return;
  box.innerHTML = '';
  items.forEach(item => {
    if (item.divider) {
      const d = document.createElement('span');
      d.className = 'sf-chip-divider';
      box.appendChild(d);
      return;
    }
    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'sf-chip';
    chip.dataset.value = item.value;
    chip.textContent = item.label;
    if (item.title) chip.title = item.title;
    if (opts.single && item.value === opts.selected) chip.classList.add('on');
    if (opts.set && opts.set.has(item.value)) chip.classList.add('on');

    chip.addEventListener('click', () => {
      if (opts.single) {
        box.querySelectorAll('.sf-chip').forEach(c => c.classList.remove('on'));
        chip.classList.add('on');
      } else {
        chip.classList.toggle('on');
        if (chip.classList.contains('on')) opts.set.add(item.value);
        else opts.set.delete(item.value);
      }
      opts.onChange(item.value);
    });
    box.appendChild(chip);
  });
}

function onPeriodChange(key) {
  searchState.period = key;
  const custom = document.getElementById('sq-custom-range');
  if (custom) custom.hidden = key !== 'custom';
  if (key === 'custom') {
    // Seed the custom range with the last 24 hours so it is never two blanks.
    const from = document.getElementById('sq-from');
    const to   = document.getElementById('sq-to');
    if (from && !from.value) from.value = isoLocalInput(new Date(Date.now() - 864e5));
    if (to && !to.value)     to.value   = isoLocalInput(new Date());
  }
  scheduleSearch();
}

// isoLocalInput formats a Date for a datetime-local input, in UTC — the whole
// UI speaks UTC, and a picker that silently meant local time would shift every
// search by the viewer's offset.
function isoLocalInput(d) {
  return d.toISOString().slice(0, 16);
}

// ── Country picker ─────────────────────────────────────────────────────────
//
// Countries are the one list too long to inject, and app.js has already
// fetched it for the live filter bar. Reuse that rather than asking twice.

function populateCountryList() {
  const list = document.getElementById('sq-country-list');
  if (!list) return;
  const fill = countries => {
    list.innerHTML = '';
    countries.forEach(c => {
      if (!c.country_code) return;
      const opt = document.createElement('option');
      opt.value = c.name + ' (' + c.country_code + ')';
      list.appendChild(opt);
    });
  };
  if (Array.isArray(window.COUNTRIES) && window.COUNTRIES.length) {
    fill(window.COUNTRIES);
  } else {
    document.addEventListener('countries-loaded',
      () => fill(window.COUNTRIES || []), { once: true });
  }
}

function onCountryPicked() {
  const input = document.getElementById('sq-country');
  if (!input) return;
  const m = /\(([A-Za-z0-9]{2})\)\s*$/.exec(input.value.trim());
  if (!m) return;
  const code = m[1].toUpperCase();
  searchState.countries.set(code, input.value.trim().replace(/\s*\([^)]*\)\s*$/, ''));
  input.value = '';
  renderCountryChips();
  scheduleSearch();
}

function renderCountryChips() {
  const box = document.getElementById('sq-country-chips');
  if (!box) return;
  box.innerHTML = '';
  searchState.countries.forEach((name, code) => {
    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'sf-chip on sf-chip-removable';
    chip.textContent = countryFlag(code) + ' ' + name + ' ×';
    chip.title = 'Remove ' + name;
    chip.addEventListener('click', () => {
      searchState.countries.delete(code);
      renderCountryChips();
      scheduleSearch();
    });
    box.appendChild(chip);
  });
}

// ── Query building ─────────────────────────────────────────────────────────

// buildSearchQuery turns the form into the /api/search query string. Only
// non-empty controls contribute, so the URL stays as short as the search is
// specific — and readable when copied out.
function buildSearchQuery() {
  const q = new URLSearchParams();

  const period = SEARCH_PERIODS.find(p => p.key === searchState.period);
  if (period && period.params) {
    Object.entries(period.params).forEach(([k, v]) => q.set(k, v));
  } else {
    const from = val('sq-from'), to = val('sq-to');
    if (from) q.set('from', from + ':00Z');
    if (to)   q.set('to', to + ':00Z');
  }

  const call = val('sq-callsign');
  if (call) q.set(checked('sq-callsign-exact') ? 'callsign_exact' : 'callsign', call);
  const spotter = val('sq-spotter');
  if (spotter) q.set(checked('sq-spotter-exact') ? 'spotter_exact' : 'spotter', spotter);

  setIf(q, 'locator', val('sq-locator'));
  setIf(q, 'q', val('sq-text'));

  csvSet(q, 'band', searchState.bands);
  csvSet(q, 'mode', searchState.modes);
  csvSet(q, 'stream', searchState.streams);
  csvSet(q, 'continent', searchState.continents);
  if (searchState.countries.size) {
    q.set('country_code', Array.from(searchState.countries.keys()).join(','));
  }

  [['snr_min', 'sq-snr-min'], ['snr_max', 'sq-snr-max'],
   ['freq_min', 'sq-freq-min'], ['freq_max', 'sq-freq-max'],
   ['dist_min', 'sq-dist-min'], ['dist_max', 'sq-dist-max'],
   ['wpm_min', 'sq-wpm-min'], ['wpm_max', 'sq-wpm-max'],
   ['hour_min', 'sq-hour-min'], ['hour_max', 'sq-hour-max'],
  ].forEach(([param, id]) => setIf(q, param, val(id)));

  const sort = val('sq-sort'), order = val('sq-order');
  if (sort && sort !== 'ts') q.set('sort', sort);
  if (order) q.set('order', order);
  const limit = val('sq-limit');
  if (limit && limit !== '100') q.set('limit', limit);

  return q;
}

function val(id) {
  const el = document.getElementById(id);
  return el ? el.value.trim() : '';
}
function checked(id) {
  const el = document.getElementById(id);
  return !!(el && el.checked);
}
function setIf(q, key, v) { if (v !== '' && v != null) q.set(key, v); }
function csvSet(q, key, set) { if (set.size) q.set(key, Array.from(set).join(',')); }

// ── Running the search ─────────────────────────────────────────────────────

// scheduleSearch debounces the auto-run so typing a callsign is one request,
// not one per keystroke.
function scheduleSearch() {
  clearTimeout(searchDebounce);
  searchDebounce = setTimeout(runSearch, 350);
}

async function runSearch() {
  clearTimeout(searchDebounce);
  const q = buildSearchQuery();
  searchState.lastQuery = q;
  searchState.rows = 0;
  await fetchSearchPage(q, false);
}

async function loadMoreSearch() {
  if (!searchState.lastQuery) return;
  const q = new URLSearchParams(searchState.lastQuery);
  if (searchState.nextCursor) {
    q.set('cursor', searchState.nextCursor);
  } else if (searchState.nextOffset != null) {
    q.set('offset', String(searchState.nextOffset));
  } else {
    return;
  }
  // The total was already established by the first page; re-counting it on
  // every "load more" would double the cost of paging for no new information.
  q.set('count', 'none');
  await fetchSearchPage(q, true);
}

async function fetchSearchPage(q, append) {
  // A search in flight is a search nobody wants any more the moment the filters
  // change, so it is cancelled rather than raced.
  abortSearch();
  const ctrl = new AbortController();
  searchState.inFlight = ctrl;

  setSearchBusy(true);
  try {
    const resp = await fetch(BASE + '/api/search?' + q.toString(), { signal: ctrl.signal });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      showSearchError(data.error || ('HTTP ' + resp.status));
      return;
    }
    renderSearchResults(data, append);
  } catch (err) {
    if (err.name !== 'AbortError') showSearchError('Search failed: ' + err.message);
  } finally {
    if (searchState.inFlight === ctrl) searchState.inFlight = null;
    setSearchBusy(false);
  }
}

function setSearchBusy(busy) {
  const btn = document.getElementById('sq-run');
  if (btn) {
    btn.disabled = busy;
    btn.textContent = busy ? 'Searching…' : 'Search';
  }
}

function showSearchError(msg) {
  const summary = document.getElementById('search-summary');
  if (summary) {
    summary.textContent = msg;
    summary.className = 'search-error';
  }
  const more = document.getElementById('search-more');
  if (more) more.hidden = true;
}

// ── Results ────────────────────────────────────────────────────────────────

// SEARCH_COLUMNS maps each result column to the sort key that orders by it,
// so a header click is the same operation as picking from the Sort dropdown.
const SEARCH_COLUMNS = [
  { key: 'ts',       label: 'UTC',      sort: 'ts' },
  { key: 'stream',   label: 'Source' },
  { key: 'callsign', label: 'Callsign', sort: 'callsign' },
  { key: 'freq',     label: 'Freq',     sort: 'freq' },
  { key: 'band',     label: 'Band' },
  { key: 'mode',     label: 'Mode',     sort: 'mode' },
  { key: 'snr',      label: 'SNR',      sort: 'snr' },
  { key: 'spotter',  label: 'Spotter',  sort: 'spotter' },
  { key: 'country',  label: 'Country',  sort: 'country' },
  { key: 'dist',     label: 'Dist',     sort: 'distance' },
  { key: 'info',     label: 'Info' },
];

function buildSearchHead() {
  const head = document.getElementById('search-head');
  if (!head) return;
  head.innerHTML = '';
  SEARCH_COLUMNS.forEach(col => {
    const th = document.createElement('th');
    th.textContent = col.label;
    if (col.sort) {
      th.className = 'sortable';
      th.dataset.sort = col.sort;
      th.title = 'Sort by ' + col.label;
      th.addEventListener('click', () => sortSearchBy(col.sort));
    }
    head.appendChild(th);
  });
}

// sortSearchBy applies a header click: a new column takes its natural
// direction, the current column flips.
function sortSearchBy(key) {
  const sortSel  = document.getElementById('sq-sort');
  const orderSel = document.getElementById('sq-order');
  if (!sortSel || !orderSel) return;
  if (sortSel.value === key) {
    orderSel.value = orderSel.value === 'desc' ? 'asc' : 'desc';
  } else {
    sortSel.value = key;
    const opt = sortSel.selectedOptions[0];
    orderSel.value = (opt && opt.dataset.defaultOrder) || 'desc';
  }
  markSortedHeader();
  runSearch();
}

function markSortedHeader() {
  const sort  = val('sq-sort');
  const order = val('sq-order');
  document.querySelectorAll('#search-head th.sortable').forEach(th => {
    th.classList.toggle('sorted', th.dataset.sort === sort);
    th.dataset.dir = th.dataset.sort === sort ? order : '';
  });
}

function renderSearchResults(data, append) {
  const tbody = document.getElementById('search-tbody');
  if (!tbody) return;
  if (!append) tbody.innerHTML = '';

  const spots = data.spots || [];
  searchState.rows = (append ? searchState.rows : 0) + spots.length;

  if (!append && spots.length === 0) {
    tbody.innerHTML = '<tr><td colspan="' + SEARCH_COLUMNS.length +
      '" class="no-data">No spots match these filters.</td></tr>';
  }

  const frag = document.createDocumentFragment();
  spots.forEach(sp => frag.appendChild(buildSearchRow(sp)));
  tbody.appendChild(frag);

  // Paging state for the next "Load more".
  searchState.nextCursor = data.next_cursor || null;
  searchState.nextOffset = (data.next_offset != null) ? data.next_offset : null;
  const more = document.getElementById('search-more');
  if (more) more.hidden = !data.has_more;

  updateSearchSummary(data);
  markSortedHeader();
}

function updateSearchSummary(data) {
  const summary = document.getElementById('search-summary');
  const timing  = document.getElementById('search-timing');
  const note    = document.getElementById('search-footer-note');

  if (summary) {
    summary.className = '';
    let text;
    if (data.total == null) {
      text = searchState.rows.toLocaleString() + ' shown';
    } else {
      const total = data.total_capped
        ? 'over ' + data.total.toLocaleString()
        : data.total.toLocaleString();
      text = total + ' match' + (data.total === 1 ? '' : 'es') +
             ' · showing ' + searchState.rows.toLocaleString();
    }
    if (data.warnings && data.warnings.length) text += ' · ' + data.warnings.join('; ');
    summary.textContent = text;
  }

  if (timing && data.took_ms != null) {
    timing.textContent = data.took_ms + ' ms';
  }

  // Spell out the window actually searched — a preset says "7 d" but the API
  // resolves it to real bounds, and that is what the results reflect.
  const win = document.getElementById('search-window-note');
  if (win && data.filter) {
    win.textContent = shortUTC(data.filter.from) + ' → ' + shortUTC(data.filter.to) + ' UTC';
  }
  if (note) {
    note.textContent = data.has_more
      ? 'More results available'
      : (searchState.rows ? 'End of results' : '');
  }
}

function shortUTC(iso) {
  if (!iso) return '';
  return String(iso).replace('T', ' ').replace(/(:\d\d)?(Z|[+-]\d\d:\d\d)$/, '');
}

// buildSearchRow renders one result. The dataset attributes are the same ones
// the live tables set, so the existing right-click spot menu works here too.
function buildSearchRow(sp) {
  const tr = document.createElement('tr');
  tr.dataset.call    = sp.callsign || '';
  tr.dataset.freq    = sp.freq_hz || '';
  tr.dataset.band    = sp.band || '';
  tr.dataset.mode    = sp.mode || sp.voice_mode || '';
  tr.dataset.country = sp.country_code || '';
  tr.dataset.snr     = (sp.snr == null) ? '' : String(sp.snr);

  const labels = SMETA.stream_labels || {};
  const mode   = sp.mode || sp.voice_mode ||
                 (sp.stream === 'cwskimmer' ? 'CW' : '');
  const snr    = (typeof sp.snr === 'number' && sp.snr !== 0)
    ? (sp.snr > 0 ? '+' : '') + sp.snr.toFixed(sp.snr % 1 === 0 ? 0 : 1)
    : '\u2014';
  const flag   = sp.country_code ? countryFlag(sp.country_code) + ' ' : '';
  const info   = sp.comment || sp.message || '';

  tr.innerHTML =
    '<td class="ts-col">'    + esc(fullUTC(sp.timestamp))             + '</td>' +
    '<td class="src-col">'   + esc(labels[sp.stream] || sp.stream || '') + '</td>' +
    '<td class="call-col">'  + esc(sp.callsign || '\u2014')           + '</td>' +
    '<td class="freq-col">'  + fmtFreq(sp.freq_hz)                    + '</td>' +
    '<td class="band-col">'  + esc(sp.band || '\u2014')               + '</td>' +
    '<td class="mode-col">'  + esc(mode || '\u2014')                  + '</td>' +
    '<td class="' + snrClass(sp.snr) + '">' + snr                     + '</td>' +
    '<td class="call-col">'  + esc(sp.spotter || '\u2014')            + '</td>' +
    '<td class="country-col">' + (sp.country ? flag + esc(sp.country) : '\u2014') + '</td>' +
    '<td class="dist-col">'  + esc(fmtDist(sp.distance_km) || '\u2014') + '</td>' +
    '<td class="info-col">'  + esc(info || '\u2014')                  + '</td>';
  return tr;
}

// fullUTC includes the date, because a search spans days where the live tables
// only ever show the last few minutes.
function fullUTC(iso) {
  if (!iso) return '\u2014';
  return new Date(iso).toISOString().replace('T', ' ').slice(0, 19);
}

// ── Export and sharing ─────────────────────────────────────────────────────

function searchApiUrl(extra) {
  const q = new URLSearchParams(searchState.lastQuery || buildSearchQuery());
  if (extra) Object.entries(extra).forEach(([k, v]) => q.set(k, v));
  const base = window.location.origin + BASE + '/api/search?';
  return base + q.toString();
}

async function copySearchApiUrl() {
  const url = searchApiUrl();
  try {
    await navigator.clipboard.writeText(url);
    flashSearchNote('API URL copied to clipboard');
  } catch (_) {
    // Clipboard access is denied in some browsers over plain HTTP; showing the
    // URL is a worse experience than copying it, but better than silence.
    window.prompt('Copy this URL:', url);
  }
}

// downloadSearchCsv asks the API for the same query as CSV. The row cap is
// raised to the API maximum, since a spreadsheet export wanting only the 100
// rows on screen would be a surprise.
function downloadSearchCsv() {
  const max = SLIMITS.max_limit || 1000;
  window.location.href = searchApiUrl({ format: 'csv', limit: String(max), count: 'none' });
  flashSearchNote('Downloading up to ' + max.toLocaleString() + ' rows as CSV');
}

function flashSearchNote(msg) {
  const note = document.getElementById('search-footer-note');
  if (!note) return;
  const prev = note.textContent;
  note.textContent = msg;
  setTimeout(() => { if (note.textContent === msg) note.textContent = prev; }, 2500);
}

function resetSearch() {
  ['sq-callsign', 'sq-spotter', 'sq-locator', 'sq-text', 'sq-country',
   'sq-snr-min', 'sq-snr-max', 'sq-freq-min', 'sq-freq-max',
   'sq-dist-min', 'sq-dist-max', 'sq-wpm-min', 'sq-wpm-max',
   'sq-hour-min', 'sq-hour-max', 'sq-from', 'sq-to'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.value = '';
  });
  ['sq-callsign-exact', 'sq-spotter-exact'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.checked = false;
  });
  [searchState.bands, searchState.modes, searchState.streams, searchState.continents]
    .forEach(s => s.clear());
  searchState.countries.clear();
  renderCountryChips();
  document.querySelectorAll('#sq-band .sf-chip, #sq-mode .sf-chip, #sq-stream .sf-chip, #sq-continent .sf-chip')
    .forEach(c => c.classList.remove('on'));

  searchState.period = '24h';
  document.querySelectorAll('#sq-period .sf-chip').forEach(c =>
    c.classList.toggle('on', c.dataset.value === '24h'));
  const custom = document.getElementById('sq-custom-range');
  if (custom) custom.hidden = true;

  const sortSel = document.getElementById('sq-sort');
  if (sortSel) sortSel.value = 'ts';
  const orderSel = document.getElementById('sq-order');
  if (orderSel) orderSel.value = 'desc';
  const limitSel = document.getElementById('sq-limit');
  if (limitSel) limitSel.value = '100';

  runSearch();
}

// ── Wiring ─────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => {
  const tbody = document.getElementById('search-tbody');
  if (tbody) tbody.addEventListener('contextmenu', onSpotContextMenu);

  // Prefill the callsign box and open straight into a search when a spot's
  // context menu asks for one.
  document.addEventListener('search-callsign', e => {
    openSearchModal();
    const el = document.getElementById('sq-callsign');
    if (el) el.value = e.detail || '';
    const period = document.querySelector('#sq-period .sf-chip[data-value="30d"]');
    if (period) period.click(); else runSearch();
  });
});

document.addEventListener('keydown', e => {
  if (searchState.open && e.key === 'Escape') {
    // The spot context menu also closes on Escape; let it win if it is open.
    if (!document.getElementById('spot-ctx-menu')) closeSearchModal();
    return;
  }
  if (e.key === 'k' && (e.ctrlKey || e.metaKey)) {
    e.preventDefault();
    openSearchModal();
    return;
  }
  // A bare "/" opens search, the way it does almost everywhere else — but not
  // while the user is typing into something.
  const tag = (e.target.tagName || '').toLowerCase();
  if (e.key === '/' && tag !== 'input' && tag !== 'textarea' && tag !== 'select') {
    e.preventDefault();
    openSearchModal();
  }
});
