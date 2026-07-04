'use strict';

const BASE          = window.BASE_PATH || '';
const MAX_ROWS      = 200;   // max rows per decoder / CW table
const VOICE_EXPIRE  = 10 * 60 * 1000; // 10 minutes in ms

// ── State ──────────────────────────────────────────────────────────────────
let countDecoder = 0;
let countCW      = 0;
// Voice activity: keyed by "band|freq" for upsert
const voiceMap   = new Map(); // key → { spot, tr }

// ── DOM refs ───────────────────────────────────────────────────────────────
let connDecoder, connCW, connVoice;
let hdrDecoder, hdrCW, hdrVoice;
let badgeDecoder, badgeCW, badgeVoice;
let tbodyDecoder, tbodyCW, tbodyVoice;

document.addEventListener('DOMContentLoaded', () => {
  connDecoder  = document.getElementById('conn-decoder');
  connCW       = document.getElementById('conn-cw');
  connVoice    = document.getElementById('conn-voice');
  hdrDecoder   = document.getElementById('hdr-decoder');
  hdrCW        = document.getElementById('hdr-cw');
  hdrVoice     = document.getElementById('hdr-voice');
  badgeDecoder = document.getElementById('badge-decoder');
  badgeCW      = document.getElementById('badge-cw');
  badgeVoice   = document.getElementById('badge-voice');
  tbodyDecoder = document.getElementById('tbody-decoder');
  tbodyCW      = document.getElementById('tbody-cw');
  tbodyVoice   = document.getElementById('tbody-voice');

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
    // Also update page title
    if (rxCall) document.title = '📡 ' + rxCall + ' DX Cluster';
  }

  connect();

  // Load history for decoder and CW panels
  loadHistory('decoder');
  loadHistory('cwskimmer');
  loadHistory('voice');

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

  // The SSE comment "connected" fires as an unnamed event in some browsers;
  // treat any successful open as connected.
  es.onopen = () => setConnState('connected');

  es.onerror = () => {
    setConnState('disconnected');
    // EventSource auto-reconnects; we just update the UI
  };
}

function setConnState(state) {
  // state: 'connected' | 'disconnected' | 'waiting'
  [connDecoder, connCW, connVoice].forEach(el => {
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
    // (prepend logic will put newest at top)
    spots.slice().reverse().forEach(s => onSpot(s, false));
  } catch(_) {}
}

// ── Spot dispatcher ────────────────────────────────────────────────────────
function onSpot(spot, live) {
  switch (spot.stream) {
    case 'decoder':   onDecoder(spot, live);  break;
    case 'cwskimmer': onCW(spot, live);       break;
    case 'voice':     onVoice(spot, live);    break;
  }
}

// ── Helpers ────────────────────────────────────────────────────────────────
function fmtUTC(isoStr) {
  if (!isoStr) return '—';
  const d = new Date(isoStr);
  return d.toISOString().slice(11, 19);
}

function fmtFreq(hz) {
  if (!hz) return '—';
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

// ── Digital Decoder ────────────────────────────────────────────────────────
function onDecoder(spot, live) {
  clearPlaceholder(tbodyDecoder);
  countDecoder++;
  hdrDecoder.textContent   = countDecoder;
  badgeDecoder.textContent = countDecoder + ' spots';

  const tr = document.createElement('tr');
  if (live) tr.className = 'new-row';

  tr.innerHTML = `
    <td class="ts-col">${fmtUTC(spot.timestamp)}</td>
    <td class="call-col">${esc(spot.callsign)}</td>
    <td class="freq-col">${fmtFreq(spot.freq_hz)}</td>
    <td class="mode-col">${esc(spot.mode || '—')}</td>
    <td class="band-col">${esc(spot.band || '—')}</td>
    <td class="${snrClass(spot.snr)}">${spot.snr > 0 ? '+' : ''}${spot.snr} dB</td>
    <td class="country-col">${countryCell(spot)}</td>
    <td class="dist-col">${fmtDist(spot.distance_km)}</td>`;

  tbodyDecoder.insertBefore(tr, tbodyDecoder.firstChild);
  trimTable(tbodyDecoder, MAX_ROWS);
}

// ── CW Skimmer ─────────────────────────────────────────────────────────────
function onCW(spot, live) {
  clearPlaceholder(tbodyCW);
  countCW++;
  hdrCW.textContent   = countCW;
  badgeCW.textContent = countCW + ' spots';

  const tr = document.createElement('tr');
  if (live) tr.className = 'new-row-cw';

  tr.innerHTML = `
    <td class="ts-col">${fmtUTC(spot.timestamp)}</td>
    <td class="call-col">${esc(spot.callsign)}</td>
    <td class="freq-col">${fmtFreq(spot.freq_hz)}</td>
    <td class="band-col">${esc(spot.band || '—')}</td>
    <td class="${snrClass(spot.snr)}">${spot.snr} dB</td>
    <td class="wpm-col">${spot.wpm || '—'}</td>
    <td class="country-col">${countryCell(spot)}</td>
    <td class="dist-col">${fmtDist(spot.distance_km)}</td>`;

  tbodyCW.insertBefore(tr, tbodyCW.firstChild);
  trimTable(tbodyCW, MAX_ROWS);
}

// ── Voice Activity ─────────────────────────────────────────────────────────
function onVoice(spot, live) {
  const key = (spot.band || '') + '|' + (spot.freq_hz || spot.est_dial_freq || 0);

  if (voiceMap.has(key)) {
    // Upsert: update existing row
    const entry = voiceMap.get(key);
    entry.spot = spot;
    updateVoiceRow(entry.tr, spot, live);
  } else {
    // New entry: prepend row
    clearPlaceholder(tbodyVoice);
    const tr = document.createElement('tr');
    if (live) tr.className = 'new-row-voice';
    tbodyVoice.insertBefore(tr, tbodyVoice.firstChild);
    voiceMap.set(key, { spot, tr });
    updateVoiceRow(tr, spot, live);
  }

  updateVoiceBadge();
}

function updateVoiceRow(tr, spot, live) {
  if (live) {
    tr.className = 'new-row-voice';
  }
  const callsign = spot.callsign === 'N0CALL'
    ? '<span style="color:var(--dim)">N0CALL</span>'
    : `<strong>${esc(spot.callsign)}</strong>`;

  tr.innerHTML = `
    <td class="ts-col">${fmtUTC(spot.timestamp)}</td>
    <td class="call-col">${callsign}</td>
    <td class="freq-col">${fmtFreq(spot.freq_hz)}</td>
    <td class="mode-col">${esc(spot.voice_mode || spot.mode || '—')}</td>
    <td class="band-col">${esc(spot.band || '—')}</td>
    <td class="${snrClass(spot.snr)}">${spot.snr.toFixed ? spot.snr.toFixed(1) : spot.snr} dB</td>
    <td class="conf-col">${spot.confidence ? (spot.confidence * 100).toFixed(0) + '%' : '—'}</td>
    <td class="country-col">${spot.country ? esc(spot.country) : '—'}</td>`;
}

function pruneVoice() {
  const now = Date.now();
  let changed = false;
  for (const [key, entry] of voiceMap) {
    const ts = new Date(entry.spot.timestamp).getTime();
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
  hdrVoice.textContent   = n;
  badgeVoice.textContent = n + ' active';
  if (n === 0) {
    tbodyVoice.innerHTML = '<tr><td colspan="8" class="no-data">No active voice signals</td></tr>';
  }
}

// ── Country cell ───────────────────────────────────────────────────────────
function countryCell(spot) {
  if (!spot.country) return '—';
  const flag = spot.country_code ? countryFlag(spot.country_code) : '';
  return flag + ' ' + esc(spot.country);
}

function countryFlag(code) {
  // Convert ISO 3166-1 alpha-2 to regional indicator emoji
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
