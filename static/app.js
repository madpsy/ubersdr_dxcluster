'use strict';

const BASE          = window.BASE_PATH || '';
const MAX_ROWS      = 200;
const VOICE_EXPIRE  = 10 * 60 * 1000; // 10 minutes in ms

// ── State ──────────────────────────────────────────────────────────────────
let countDecoder = 0;
let countCW      = 0;
let countDX      = 0;
// Voice activity: keyed by "band|freq" for upsert
const voiceMap   = new Map(); // key → { spot, tr }

// ── DOM refs ───────────────────────────────────────────────────────────────
let connDecoder, connCW, connVoice, connDX;
let hdrDecoder, hdrCW, hdrVoice, hdrDX;
let badgeDecoder, badgeCW, badgeVoice, badgeDX;
let tbodyDecoder, tbodyCW, tbodyVoice, tbodyDX;

document.addEventListener('DOMContentLoaded', () => {
  connDecoder  = document.getElementById('conn-decoder');
  connCW       = document.getElementById('conn-cw');
  connVoice    = document.getElementById('conn-voice');
  connDX       = document.getElementById('conn-dx');
  hdrDecoder   = document.getElementById('hdr-decoder');
  hdrCW        = document.getElementById('hdr-cw');
  hdrVoice     = document.getElementById('hdr-voice');
  hdrDX        = document.getElementById('hdr-dx');
  badgeDecoder = document.getElementById('badge-decoder');
  badgeCW      = document.getElementById('badge-cw');
  badgeVoice   = document.getElementById('badge-voice');
  badgeDX      = document.getElementById('badge-dx');
  tbodyDecoder = document.getElementById('tbody-decoder');
  tbodyCW      = document.getElementById('tbody-cw');
  tbodyVoice   = document.getElementById('tbody-voice');
  tbodyDX      = document.getElementById('tbody-dx');

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
  } catch(_) {}
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

// ── Spot dispatcher ────────────────────────────────────────────────────────
function onSpot(spot, live) {
  switch (spot.stream) {
    case 'decoder':   onDecoder(spot, live);  break;
    case 'cwskimmer': onCW(spot, live);       break;
    case 'voice':     onVoice(spot, live);    break;
    case 'dxcluster': onDXSpot(spot, live);   break;
  }
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

// ── Digital Decoder ────────────────────────────────────────────────────────
function onDecoder(spot, live) {
  clearPlaceholder(tbodyDecoder);
  countDecoder++;
  hdrDecoder.textContent   = countDecoder;
  badgeDecoder.textContent = countDecoder + ' spots';

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
  hdrCW.textContent   = countCW;
  badgeCW.textContent = countCW + ' spots';

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
  hdrDX.textContent   = countDX;
  badgeDX.textContent = countDX + ' spots';

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
  hdrVoice.textContent   = n;
  badgeVoice.textContent = n + ' active';
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
