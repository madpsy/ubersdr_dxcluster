'use strict';

// ── Help modal ─────────────────────────────────────────────────────────────
let helpLoaded = false;

function openHelpModal() {
  const overlay = document.getElementById('help-overlay');
  if (overlay) overlay.classList.add('open');
  if (!helpLoaded) {
    fetch(BASE + '/api/help')
      .then(r => r.text())
      .then(text => {
        const body = document.getElementById('help-body');
        if (body) body.textContent = text;
        helpLoaded = true;
      })
      .catch(() => {
        const body = document.getElementById('help-body');
        if (body) body.textContent = 'Failed to load help.';
      });
  }
}

function closeHelpModal() {
  const overlay = document.getElementById('help-overlay');
  if (overlay) overlay.classList.remove('open');
}

// ── Client download info modal ──────────────────────────────────────────────
// Shown when the "Download Client" button is clicked; the download itself is
// triggered by the anchor's href (this does not preventDefault).
function showClientInfo() {
  const overlay = document.getElementById('client-info-overlay');
  if (overlay) overlay.classList.add('open');
}

function closeClientInfo() {
  const overlay = document.getElementById('client-info-overlay');
  if (overlay) overlay.classList.remove('open');
}

document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    closeHelpModal();
    closeClientInfo();
  }
});

const BASE          = window.BASE_PATH || '';
const MAX_ROWS      = 200;    // max rows per individual stream table
const MAX_ROWS_ALL  = 2000;   // combined panel buffer (larger to absorb hidden filtered rows)
const VOICE_EXPIRE  = 10 * 60 * 1000; // 10 minutes in ms

// ── State ──────────────────────────────────────────────────────────────────
let countDecoder = 0;
let countCW      = 0;
let countDX      = 0;
let countAll     = 0;
// Voice activity: keyed by "band|freq" for upsert
const voiceMap   = new Map(); // key → { spot, tr }

// ── DOM refs ───────────────────────────────────────────────────────────────
let connDecoder, connCW, connVoice, connDX;
let hdrDecoder, hdrCW, hdrVoice, hdrDX, hdrTotal;
let badgeDecoder, badgeCW, badgeVoice, badgeDX, badgeAll;
let tbodyDecoder, tbodyCW, tbodyVoice, tbodyDX, tbodyAll;

document.addEventListener('DOMContentLoaded', () => {
  connDecoder  = document.getElementById('conn-decoder');
  connCW       = document.getElementById('conn-cw');
  connVoice    = document.getElementById('conn-voice');
  connDX       = document.getElementById('conn-dx');
  hdrDecoder   = document.getElementById('hdr-decoder');
  hdrCW        = document.getElementById('hdr-cw');
  hdrVoice     = document.getElementById('hdr-voice');
  hdrDX        = document.getElementById('hdr-dx');
  hdrTotal     = document.getElementById('hdr-total');
  badgeDecoder = document.getElementById('badge-decoder');
  badgeCW      = document.getElementById('badge-cw');
  badgeVoice   = document.getElementById('badge-voice');
  badgeDX      = document.getElementById('badge-dx');
  badgeAll     = document.getElementById('badge-all');
  tbodyDecoder = document.getElementById('tbody-decoder');
  tbodyCW      = document.getElementById('tbody-cw');
  tbodyVoice   = document.getElementById('tbody-voice');
  tbodyDX      = document.getElementById('tbody-dx');
  tbodyAll     = document.getElementById('tbody-all');

  // Populate receiver info in header from server-injected globals
  const rxCall = window.RX_CALLSIGN || '';
  const rxLoc  = window.RX_LOCATION || '';
  const rxName = window.RX_NAME     || '';
  if (rxCall || rxLoc) {
    const infoEl = document.getElementById('rx-info');
    const callEl = document.getElementById('rx-call');
    const locEl  = document.getElementById('rx-loc');
    if (infoEl) infoEl.style.display = 'flex';
    if (callEl) callEl.textContent = rxCall;
    if (locEl)  locEl.textContent  = rxLoc || rxName;
    if (rxCall) document.title = '\uD83D\uDCE1 ' + rxCall + ' DX Cluster';
    }
  
    // Show telnet port badge from server-injected global
    const telnetAddr = window.TELNET_ADDR || '';
    if (telnetAddr) {
      const pill    = document.getElementById('telnet-pill');
      const portEl  = document.getElementById('hdr-telnet-port');
      // Extract port number from ":7300" or "0.0.0.0:7300"
      const port = telnetAddr.split(':').pop();
      if (pill)   pill.style.display = 'flex';
      if (portEl) portEl.textContent = port;
    }
  
    connect();
  
    // Load country list for filter select
    loadCountries();
  
    // Poll /api/status every 10 s for telnet client count
    pollStatus();
    setInterval(pollStatus, 10_000);

  // Load history for all four panels
  loadHistory('decoder');
  loadHistory('cwskimmer');
  loadHistory('voice');
  loadHistory('dxcluster');

  // Periodically age out stale voice entries
  setInterval(pruneVoice, 30_000);
});

// ── SSE connection ─────────────────────────────────────────────────────────
function connect() {
  setConnState('waiting');

  const es = new EventSource(BASE + '/api/events');

  es.addEventListener('message', e => {
    try {
      const spot = JSON.parse(e.data);
      onSpot(spot, true);
    } catch(_) {}
  });

  es.addEventListener('heartbeat', () => {
    setConnState('connected');
  });

  es.onopen = () => setConnState('connected');

  es.onerror = () => {
    setConnState('disconnected');
  };
}

// ── Country list loader ────────────────────────────────────────────────────
async function loadCountries() {
  try {
    const resp = await fetch(BASE + '/api/countries');
    if (!resp.ok) return;
    const countries = await resp.json();
    if (!Array.isArray(countries) || countries.length === 0) return;
    const sel = document.getElementById('f-country');
    if (!sel) return;
    // Keep the "All" option, append country options sorted alphabetically
    countries.forEach(c => {
      if (!c.country_code) return;
      const opt = document.createElement('option');
      opt.value = c.country_code;
      opt.textContent = c.name + ' (' + c.country_code + ')';
      sel.appendChild(opt);
    });
  } catch(_) {}
}

// ── Status polling ─────────────────────────────────────────────────────────
async function pollStatus() {
  try {
    const resp = await fetch(BASE + '/api/status');
    if (!resp.ok) return;
    const data = await resp.json();
    const clientsEl = document.getElementById('hdr-telnet-clients');
    if (clientsEl && typeof data.telnet_clients === 'number') {
      clientsEl.textContent = data.telnet_clients;
    }
    // Update the client tooltip on the Clients badge
    updateClientTooltip(data.telnet_client_list || []);
  } catch(_) {}
}

/**
 * Format a connected_at ISO timestamp as a human duration "Xh Ym Zs".
 * @param {string} isoTs  RFC3339 timestamp from the server
 * @returns {string}
 */
function fmtConnDuration(isoTs) {
  if (!isoTs) return '';
  const secs = Math.max(0, Math.floor((Date.now() - new Date(isoTs).getTime()) / 1000));
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = secs % 60;
  if (h > 0) return h + 'h ' + m + 'm ' + s + 's';
  if (m > 0) return m + 'm ' + s + 's';
  return s + 's';
}

/**
 * Build or update a tooltip on the "Clients" value span showing masked IPs,
 * callsigns, and connection duration of connected telnet/web-terminal clients.
 * The tooltip is a plain <div> positioned via CSS — no external libraries.
 * All content is set via textContent (XSS-safe).
 *
 * @param {Array<{ip: string, callsign: string, connected_at: string}>} clients
 */
function updateClientTooltip(clients) {
  const clientsEl = document.getElementById('hdr-telnet-clients');
  if (!clientsEl) return;

  // Find or create the tooltip element
  let tip = document.getElementById('client-ip-tip');
  if (!tip) {
    tip = document.createElement('div');
    tip.id = 'client-ip-tip';
    tip.className = 'client-ip-tip';
    // Insert after the clients element inside the pill
    clientsEl.parentNode.insertBefore(tip, clientsEl.nextSibling);

    // Show/hide on hover of the whole pill
    const pill = document.getElementById('telnet-pill');
    if (pill) {
      pill.addEventListener('mouseenter', () => {
        if (tip.childNodes.length > 0) tip.classList.add('visible');
      });
      pill.addEventListener('mouseleave', () => {
        tip.classList.remove('visible');
      });
    }
  }

  // Rebuild tooltip content
  tip.textContent = '';
  if (!clients || clients.length === 0) {
    const line = document.createElement('span');
    line.className = 'client-ip-tip-line client-ip-tip-empty';
    line.textContent = 'No clients connected';
    tip.appendChild(line);
  } else {
    clients.forEach(c => {
      const line = document.createElement('span');
      line.className = 'client-ip-tip-line';
      const dur = fmtConnDuration(c.connected_at);
      // Show "CALLSIGN  xxx.xxx.1.2  1h 4m 23s" or "(logging in…)  xxx.xxx.1.2  5s"
      if (c.callsign) {
        line.textContent = c.callsign + '\u2002' + c.ip + (dur ? '\u2002' + dur : '');
      } else {
        line.textContent = c.ip + '\u2002(logging in…)' + (dur ? '\u2002' + dur : '');
      }
      tip.appendChild(line);
    });
  }
}

function setConnState(state) {
  [connDecoder, connCW, connVoice, connDX].forEach(el => {
    if (!el) return;
    el.className = 'conn-pill ' + state;
  });
}

// ── History load ───────────────────────────────────────────────────────────
async function loadHistory(stream) {
  try {
    const resp = await fetch(BASE + '/api/spots?stream=' + stream);
    if (!resp.ok) return;
    const spots = await resp.json();
    if (!Array.isArray(spots)) return;
    // History is newest-first; reverse so oldest is processed first
    spots.slice().reverse().forEach(s => onSpot(s, false));
  } catch(_) {}
}

// ── Card collapse toggle ───────────────────────────────────────────────────
function toggleCard(id) {
  const body    = document.getElementById('body-' + id);
  const icon    = document.getElementById('collapse-' + id);
  if (!body) return;
  const collapsed = body.classList.toggle('collapsed');
  if (icon) icon.textContent = collapsed ? '▼' : '▲';
}

// ── Spot dispatcher ────────────────────────────────────────────────────────
function onSpot(spot, live) {
  onAllSpot(spot, live);   // always feed combined panel
  switch (spot.stream) {
    case 'decoder':   onDecoder(spot, live);  break;
    case 'cwskimmer': onCW(spot, live);       break;
    case 'voice':     onVoice(spot, live);    break;
    case 'dxcluster': onDXSpot(spot, live);   break;
  }
}

// ── Combined All Spots panel ───────────────────────────────────────────────
// Columns: UTC | Type | Callsign | Freq | Band | Country
const STREAM_LABELS = {
  decoder:   { label: 'Digital', cls: 'type-digital' },
  cwskimmer: { label: 'CW',      cls: 'type-cw'      },
  voice:     { label: 'Voice',   cls: 'type-voice'   },
  dxcluster: { label: 'DX',      cls: 'type-dx'      },
};

function onAllSpot(spot, live) {
  if (!tbodyAll) return;
  clearPlaceholder(tbodyAll);
  countAll++;
  if (badgeAll) badgeAll.textContent = countAll + ' spots';
  if (hdrTotal) hdrTotal.textContent = countAll;

  const meta = STREAM_LABELS[spot.stream] || { label: spot.stream, cls: '' };
  const flag    = spot.country_code ? countryFlag(spot.country_code) + ' ' : '';
  const country = spot.country ? flag + esc(spot.country) : '\u2014';

  // Determine mode for filter matching
  let mode = spot.mode || '';
  if (spot.stream === 'cwskimmer')  mode = 'CW';
  if (spot.stream === 'voice')      mode = spot.voice_mode || spot.mode || '';
  // Band is already normalised server-side (e.g. "20m_FT8" → "20m")

  const tr = document.createElement('tr');
  if (live) tr.className = 'new-row-all';

  // Store filter-relevant data as attributes
  tr.dataset.stream  = spot.stream        || '';
  tr.dataset.band    = spot.band || '';
  tr.dataset.mode    = mode;
  tr.dataset.cont    = spot.continent     || '';
  tr.dataset.country = spot.country_code  || '';
  tr.dataset.call    = spot.callsign      || '';
  tr.dataset.snr     = isNaN(spot.snr) ? '' : String(spot.snr);

  tr.innerHTML =
    '<td class="ts-col">'      + fmtUTC(spot.timestamp)                    + '</td>' +
    '<td><span class="type-badge ' + meta.cls + '">' + meta.label + '</span></td>' +
    '<td class="call-col">'    + esc(spot.callsign || '\u2014')             + '</td>' +
    '<td class="freq-col">'    + fmtFreq(spot.freq_hz)                      + '</td>' +
    '<td class="band-col">'    + esc(spot.band || '\u2014')                 + '</td>' +
    '<td class="country-col">' + country                                    + '</td>';

  // Apply current filter before inserting
  if (!rowMatchesFilter(tr)) tr.style.display = 'none';

  tbodyAll.insertBefore(tr, tbodyAll.firstChild);
  trimTable(tbodyAll, MAX_ROWS_ALL);
}

// ── Helpers ────────────────────────────────────────────────────────────────
function fmtUTC(isoStr) {
  if (!isoStr) return '\u2014';
  return new Date(isoStr).toISOString().slice(11, 19);
}

function fmtFreq(hz) {
  if (!hz) return '\u2014';
  return (hz / 1000).toFixed(1) + ' kHz';
}

function snrClass(snr) {
  if (snr >= 20)  return 'snr-hi';
  if (snr >= 10)  return 'snr-med';
  if (snr >= 0)   return 'snr-lo';
  return 'snr-neg';
}

function fmtDist(km) {
  if (!km || km === 0) return '';
  return km >= 1000
    ? (km / 1000).toFixed(1) + ' Mm'
    : Math.round(km) + ' km';
}

function clearPlaceholder(tbody) {
  const ph = tbody.querySelector('.no-data');
  if (ph) ph.closest('tr').remove();
}

function trimTable(tbody, max) {
  while (tbody.rows.length > max) {
    tbody.deleteRow(tbody.rows.length - 1);
  }
}

// Build a unified table row: UTC | Callsign | Freq | Band | Mode | SNR | Country | Info
function buildRow(fields) {
  // fields: { ts, callsign, freq_hz, band, mode, snr, country, country_code, info }
  const snr     = fields.snr;
  const snrStr  = typeof snr === 'number'
    ? (snr > 0 ? '+' : '') + snr.toFixed(snr % 1 === 0 ? 0 : 1) + ' dB'
    : (snr || '\u2014');
  const flag    = fields.country_code ? countryFlag(fields.country_code) + ' ' : '';
  const country = fields.country ? flag + esc(fields.country) : '\u2014';

  const tr = document.createElement('tr');
  tr.innerHTML =
    '<td class="ts-col">'      + fmtUTC(fields.ts)              + '</td>' +
    '<td class="call-col">'    + (fields.callsign || '\u2014')   + '</td>' +
    '<td class="freq-col">'    + fmtFreq(fields.freq_hz)         + '</td>' +
    '<td class="band-col">'    + esc(fields.band || '\u2014')    + '</td>' +
    '<td class="mode-col">'    + esc(fields.mode || '\u2014')    + '</td>' +
    '<td class="' + snrClass(snr) + '">' + snrStr                + '</td>' +
    '<td class="country-col">' + country                         + '</td>' +
    '<td class="info-col">'    + (fields.info || '\u2014')       + '</td>';
  return tr;
}

// ── Filter logic (combined panel only) ────────────────────────────────────

// Stream type → mode values produced by that stream
const STREAM_MODES = {
  decoder:   ['FT8','FT4','WSPR','JS8','FT2'],
  cwskimmer: ['CW'],
  voice:     ['USB','LSB'],
  dxcluster: [],   // no mode field
};

function getCheckedModes() {
  const map = {
    'f-mode-ft8':  'FT8',  'f-mode-ft4':  'FT4',
    'f-mode-wspr': 'WSPR', 'f-mode-js8':  'JS8',
    'f-mode-cw':   'CW',   'f-mode-usb':  'USB',
    'f-mode-lsb':  'LSB',
  };
  const checked = new Set();
  for (const [id, mode] of Object.entries(map)) {
    const el = document.getElementById(id);
    if (el && el.checked) checked.add(mode);
  }
  return checked;
}

function getSelectedOptions(selectId) {
  const el = document.getElementById(selectId);
  if (!el) return [];
  return Array.from(el.selectedOptions)
    .map(o => o.value)
    .filter(v => v !== '');
}

function getTextList(inputId) {
  const el = document.getElementById(inputId);
  if (!el || !el.value.trim()) return [];
  return el.value.split(',').map(s => s.trim().toUpperCase()).filter(Boolean);
}

function rowMatchesFilter(tr) {
  // Read current filter state
  const showDigital = document.getElementById('f-type-digital')?.checked ?? true;
  const showCW      = document.getElementById('f-type-cw')?.checked      ?? true;
  const showVoice   = document.getElementById('f-type-voice')?.checked    ?? true;
  const showDX      = document.getElementById('f-type-dx')?.checked       ?? true;

  const checkedModes = getCheckedModes();
  const bands        = getSelectedOptions('f-band');
  const conts        = getSelectedOptions('f-cont');
  const countries    = getSelectedOptions('f-country');
  const callPfx      = getTextList('f-call');

  const snrMinEl = document.getElementById('f-snr-min');
  const snrMaxEl = document.getElementById('f-snr-max');
  const snrMin   = snrMinEl ? parseInt(snrMinEl.value, 10) : -30;
  const snrMax   = snrMaxEl ? parseInt(snrMaxEl.value, 10) : 40;
  const snrMinActive = snrMin > -30;
  const snrMaxActive = snrMax < 40;

  // Read data attributes stored on the row
  const stream  = tr.dataset.stream  || '';
  const band    = (tr.dataset.band    || '').toUpperCase();
  const mode    = (tr.dataset.mode    || '').toUpperCase();
  const cont    = (tr.dataset.cont    || '').toUpperCase();
  const country = (tr.dataset.country || '').toUpperCase();
  const call    = (tr.dataset.call    || '').toUpperCase();
  const snr     = parseFloat(tr.dataset.snr || 'NaN');

  // Stream type toggle
  if (stream === 'decoder'   && !showDigital) return false;
  if (stream === 'cwskimmer' && !showCW)      return false;
  if (stream === 'voice'     && !showVoice)   return false;
  if (stream === 'dxcluster' && !showDX)      return false;

  // Mode filter — only applies if the stream has modes
  if (STREAM_MODES[stream] && STREAM_MODES[stream].length > 0) {
    if (mode && !checkedModes.has(mode)) return false;
  }

  // Band filter
  if (bands.length > 0 && band && !bands.map(b => b.toUpperCase()).includes(band)) return false;

  // Continent filter
  if (conts.length > 0 && cont && !conts.map(c => c.toUpperCase()).includes(cont)) return false;

  // Country filter
  if (countries.length > 0 && country && !countries.includes(country)) return false;

  // Callsign prefix filter
  if (callPfx.length > 0 && call) {
    if (!callPfx.some(pfx => call.startsWith(pfx))) return false;
  }

  // SNR filter
  if (snrMinActive && !isNaN(snr) && snr < snrMin) return false;
  if (snrMaxActive && !isNaN(snr) && snr > snrMax) return false;

  return true;
}

function applyFilters() {
  if (!tbodyAll) return;
  const rows = tbodyAll.querySelectorAll('tr[data-stream]');
  rows.forEach(tr => {
    tr.style.display = rowMatchesFilter(tr) ? '' : 'none';
  });
}

function onSnrMinChange() {
  const el  = document.getElementById('f-snr-min');
  const val = document.getElementById('f-snr-min-val');
  if (el && val) val.textContent = parseInt(el.value, 10) <= -30 ? 'Any' : el.value + ' dB';
  applyFilters();
}

function onSnrMaxChange() {
  const el  = document.getElementById('f-snr-max');
  const val = document.getElementById('f-snr-max-val');
  if (el && val) val.textContent = parseInt(el.value, 10) >= 40 ? 'Any' : el.value + ' dB';
  applyFilters();
}

function clearAllFilters() {
  // Reset stream toggles — digital and DX are off by default
  const streamDefaults = { 'f-type-digital': false, 'f-type-cw': true, 'f-type-voice': true, 'f-type-dx': false };
  for (const [id, def] of Object.entries(streamDefaults)) {
    const el = document.getElementById(id);
    if (el) el.checked = def;
  }
  // Reset mode checkboxes
  ['f-mode-ft8','f-mode-ft4','f-mode-wspr','f-mode-js8','f-mode-cw','f-mode-usb','f-mode-lsb'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.checked = true;
  });
  // Reset multi-selects (deselect all)
  ['f-band','f-cont'].forEach(id => {
    const el = document.getElementById(id);
    if (el) Array.from(el.options).forEach(o => o.selected = false);
  });
  // Reset country select and call text input
  const countryEl = document.getElementById('f-country');
  if (countryEl) Array.from(countryEl.options).forEach(o => o.selected = false);
  const callEl = document.getElementById('f-call');
  if (callEl) callEl.value = '';
  // Reset SNR sliders
  const snrMin = document.getElementById('f-snr-min');
  const snrMax = document.getElementById('f-snr-max');
  if (snrMin) { snrMin.value = -30; document.getElementById('f-snr-min-val').textContent = 'Any'; }
  if (snrMax) { snrMax.value = 40;  document.getElementById('f-snr-max-val').textContent = 'Any'; }
  applyFilters();
}

// ── Digital Decoder ────────────────────────────────────────────────────────
function onDecoder(spot, live) {
  clearPlaceholder(tbodyDecoder);
  countDecoder++;
  if (hdrDecoder)   hdrDecoder.textContent   = countDecoder;
  if (badgeDecoder) badgeDecoder.textContent = countDecoder + ' spots';

  // Info: locator if present, else distance
  const info = spot.locator
    ? '<span class="locator-col">' + esc(spot.locator) + '</span>'
    : (fmtDist(spot.distance_km) || '\u2014');

  const tr = buildRow({
    ts:           spot.timestamp,
    callsign:     esc(spot.callsign),
    freq_hz:      spot.freq_hz,
    band:         spot.band,
    mode:         spot.mode,
    snr:          spot.snr,
    country:      spot.country,
    country_code: spot.country_code,
    info:         info,
  });
  if (live) tr.className = 'new-row';

  tbodyDecoder.insertBefore(tr, tbodyDecoder.firstChild);
  trimTable(tbodyDecoder, MAX_ROWS);
}

// ── CW Skimmer ─────────────────────────────────────────────────────────────
function onCW(spot, live) {
  clearPlaceholder(tbodyCW);
  countCW++;
  if (hdrCW)   hdrCW.textContent   = countCW;
  if (badgeCW) badgeCW.textContent = countCW + ' spots';

  // Info: WPM + spotter callsign
  const wpm     = spot.wpm     ? spot.wpm + ' wpm'          : '';
  const spotter = spot.spotter ? 'de ' + esc(spot.spotter)  : '';
  const info    = [wpm, spotter].filter(Boolean).join(' ') || '\u2014';

  const tr = buildRow({
    ts:           spot.timestamp,
    callsign:     esc(spot.callsign),
    freq_hz:      spot.freq_hz,
    band:         spot.band,
    mode:         'CW',
    snr:          spot.snr,
    country:      spot.country,
    country_code: spot.country_code,
    info:         info,
  });
  if (live) tr.className = 'new-row-cw';

  tbodyCW.insertBefore(tr, tbodyCW.firstChild);
  trimTable(tbodyCW, MAX_ROWS);
}

// ── DX Cluster Spots ───────────────────────────────────────────────────────
function onDXSpot(spot, live) {
  clearPlaceholder(tbodyDX);
  countDX++;
  if (hdrDX)   hdrDX.textContent   = countDX;
  if (badgeDX) badgeDX.textContent = countDX + ' spots';

  // Info: comment from spotter
  const info = spot.comment ? esc(spot.comment) : '\u2014';

  const tr = buildRow({
    ts:           spot.timestamp,
    callsign:     esc(spot.callsign),   // dx_call mapped to callsign in spot.go
    freq_hz:      spot.freq_hz,
    band:         spot.band,
    mode:         '\u2014',             // DX spots don't carry mode
    snr:          null,                 // no SNR for DX spots
    country:      spot.country,
    country_code: spot.country_code,
    info:         info,
  });

  // Override the SNR cell with the spotter callsign instead
  const cells = tr.querySelectorAll('td');
  if (cells[5]) {
    cells[5].className = 'muted-col';
    cells[5].textContent = spot.spotter ? esc(spot.spotter) : '\u2014';
  }

  if (live) tr.className = 'new-row-dx';

  tbodyDX.insertBefore(tr, tbodyDX.firstChild);
  trimTable(tbodyDX, MAX_ROWS);
}

// ── Voice Activity ─────────────────────────────────────────────────────────
function onVoice(spot, live) {
  const key = (spot.band || '') + '|' + (spot.freq_hz || spot.est_dial_freq || 0);

  if (voiceMap.has(key)) {
    const entry = voiceMap.get(key);
    entry.spot = spot;
    updateVoiceRow(entry.tr, spot, live);
  } else {
    clearPlaceholder(tbodyVoice);
    const tr = document.createElement('tr');
    tbodyVoice.insertBefore(tr, tbodyVoice.firstChild);
    voiceMap.set(key, { spot, tr });
    updateVoiceRow(tr, spot, live);
  }

  updateVoiceBadge();
}

function updateVoiceRow(tr, spot, live) {
  if (live) tr.className = 'new-row-voice';

  // Callsign: dim N0CALL
  const callsign = spot.callsign === 'N0CALL'
    ? '<span style="color:var(--dim)">N0CALL</span>'
    : '<strong>' + esc(spot.callsign) + '</strong>';

  // Info: confidence% + bandwidth
  const conf = spot.confidence ? (spot.confidence * 100).toFixed(0) + '%' : '';
  const bw   = spot.bandwidth  ? spot.bandwidth + ' Hz'               : '';
  const info = [conf, bw].filter(Boolean).join(' \u00B7 ') || '\u2014';

  const snr    = spot.snr;
  const snrStr = typeof snr === 'number'
    ? snr.toFixed(1) + ' dB'
    : (snr || '\u2014');
  const flag    = spot.country_code ? countryFlag(spot.country_code) + ' ' : '';
  const country = spot.country ? flag + esc(spot.country) : '\u2014';

  tr.innerHTML =
    '<td class="ts-col">'      + fmtUTC(spot.timestamp)                    + '</td>' +
    '<td class="call-col">'    + callsign                                   + '</td>' +
    '<td class="freq-col">'    + fmtFreq(spot.freq_hz)                      + '</td>' +
    '<td class="band-col">'    + esc(spot.band || '\u2014')                 + '</td>' +
    '<td class="mode-col">'    + esc(spot.voice_mode || spot.mode || '\u2014') + '</td>' +
    '<td class="' + snrClass(snr) + '">' + snrStr                           + '</td>' +
    '<td class="country-col">' + country                                    + '</td>' +
    '<td class="info-col">'    + info                                       + '</td>';
}

function pruneVoice() {
  const now = Date.now();
  let changed = false;
  for (const [key, entry] of voiceMap) {
    const ts  = new Date(entry.spot.timestamp).getTime();
    const age = now - ts;
    if (age > VOICE_EXPIRE) {
      entry.tr.remove();
      voiceMap.delete(key);
      changed = true;
    } else if (age > VOICE_EXPIRE / 2) {
      entry.tr.classList.add('stale');
    }
  }
  if (changed) updateVoiceBadge();
}

function updateVoiceBadge() {
  const n = voiceMap.size;
  if (hdrVoice)   hdrVoice.textContent   = n;
  if (badgeVoice) badgeVoice.textContent = n + ' active';
  if (n === 0) {
    tbodyVoice.innerHTML = '<tr><td colspan="8" class="no-data">No active voice signals</td></tr>';
  }
}

// ── Country flag emoji ─────────────────────────────────────────────────────
function countryFlag(code) {
  if (!code || code.length !== 2) return '';
  const offset = 0x1F1E6 - 65;
  return String.fromCodePoint(code.toUpperCase().charCodeAt(0) + offset) +
         String.fromCodePoint(code.toUpperCase().charCodeAt(1) + offset);
}

// ── XSS escape ─────────────────────────────────────────────────────────────
function esc(str) {
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}
