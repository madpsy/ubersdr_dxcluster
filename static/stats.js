/* stats.js — the DX cluster statistics dashboard.
 *
 * One filter row scopes every chart; each tab is a set of queries against
 * /api/stats/*. Charts are hand-built SVG so the page stays dependency-free,
 * like the rest of the UI.
 */
(function () {
'use strict';

const BASE = window.BASE_PATH || '';
const api = (path, qs) => `${BASE}/api/stats/${path}?${qs}`;

/* ── Palette ───────────────────────────────────────────────────────────────
 * Categorical slots in fixed order (see stats.css for the validation note).
 * Colour follows the entity, never its rank: a name keeps its slot across
 * filter changes, so filtering a series out never repaints the survivors.
 */
const SERIES = ['--s1', '--s2', '--s3', '--s4', '--s5',
                '--s6', '--s7', '--s8', '--s9', '--s10'];
const RAMP = ['--q0', '--q1', '--q2', '--q3', '--q4', '--q5', '--q6', '--q7'];
const cssVar = (n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim();

const slotRegistry = new Map(); // entity name → slot index, sticky for the session

// assignColors returns name → hex for the names in this render. Already-known
// entities keep their slot unless an earlier name in this render holds it, in
// which case the newcomer takes the lowest free slot.
const OTHER_SERIES = 'Other';

function assignColors(names) {
  const taken = new Set(), out = {};
  const pending = [];
  for (const n of names) {
    if (n === OTHER_SERIES) continue;
    const s = slotRegistry.get(n);
    if (s !== undefined && !taken.has(s)) { taken.add(s); out[n] = s; }
    else pending.push(n);
  }
  for (const n of pending) {
    let s = 0;
    while (taken.has(s) && s < SERIES.length - 1) s++;
    taken.add(s); slotRegistry.set(n, s); out[n] = s;
  }
  const hex = {};
  for (const n of names) {
    hex[n] = n === OTHER_SERIES ? cssVar('--muted') : cssVar(SERIES[out[n] % SERIES.length]);
  }
  return hex;
}

// rampColor maps a 0–1 magnitude onto the sequential ramp.
function rampColor(t) {
  if (!(t >= 0)) return cssVar(RAMP[0]);
  const i = Math.min(RAMP.length - 1, Math.round(t * (RAMP.length - 1)));
  return cssVar(RAMP[i]);
}

/* ── Time zone ─────────────────────────────────────────────────────────────
 * Every timestamp the API returns is UTC. Which zone they are *displayed* in is
 * purely a browser-side choice between three: UTC, the receiver's own zone, and
 * the viewer's. The receiver's zone comes from /api/description as an IANA name
 * ("Europe/London"), which is what makes this correct across a DST change — the
 * `timezone_offset` field alongside it is only today's offset, so a spot from
 * the other side of a transition would be an hour out.
 */
const TZ_KEY = 'dxstats.tz.v1';
const RX_TZ = (window.RX_TZ || '').trim();
const RX_TZ_OFFSET = Number(window.RX_TZ_OFFSET) || 0;

// A named zone is preferred; a whole-hour offset is the fallback when the
// instance reports no name (Etc/GMT signs are inverted, hence the flip).
// An absent name AND a zero offset means "not reported" — not UTC+0 — so the
// Instance option is withheld rather than quietly duplicating UTC.
const INSTANCE_ZONE = RX_TZ ||
  (RX_TZ_OFFSET !== 0 && RX_TZ_OFFSET % 60 === 0
    ? `Etc/GMT${RX_TZ_OFFSET > 0 ? '-' : '+'}${Math.abs(RX_TZ_OFFSET / 60)}`
    : '');

let tzMode = 'utc';

// zoneFor maps the mode to an Intl timeZone: undefined means the viewer's own.
function zoneFor(mode) {
  if (mode === 'utc') return 'UTC';
  if (mode === 'instance') return INSTANCE_ZONE || 'UTC';
  return undefined;
}

// tzAbbrev is the short zone name for the current mode ("UTC", "BST", "CEST"),
// so a displayed time always says which clock it is on.
function tzAbbrev(mode = tzMode, at = new Date()) {
  try {
    const parts = new Intl.DateTimeFormat('en-GB', {
      timeZone: zoneFor(mode), timeZoneName: 'short',
    }).formatToParts(at);
    return (parts.find(p => p.type === 'timeZoneName') || {}).value || '';
  } catch (e) { return ''; }
}

// tzOffsetMinutes is the offset in effect at a given instant, used to rotate
// the hour-of-day axis (which the server buckets in UTC).
function tzOffsetMinutes(at = new Date(), mode = tzMode) {
  if (mode === 'utc') return 0;
  if (mode === 'local') return -at.getTimezoneOffset();
  try {
    const f = new Intl.DateTimeFormat('en-US', {
      timeZone: zoneFor('instance'), hour12: false,
      year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    });
    const p = Object.fromEntries(f.formatToParts(at).map(x => [x.type, x.value]));
    const asUTC = Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour % 24, +p.minute, +p.second);
    return Math.round((asUTC - at.getTime()) / 60000);
  } catch (e) { return 0; }
}

// fmtInZone renders an instant in the selected zone.
function fmtInZone(d, opts) {
  try {
    return new Intl.DateTimeFormat('en-GB',
      Object.assign({ timeZone: zoneFor(tzMode), hour12: false }, opts)).format(d);
  } catch (e) {
    return new Intl.DateTimeFormat('en-GB', Object.assign({ hour12: false }, opts)).format(d);
  }
}

const DATE_TIME = { year: 'numeric', month: '2-digit', day: '2-digit',
                    hour: '2-digit', minute: '2-digit' };

/* ── Formatting ────────────────────────────────────────────────────────── */
const nf = new Intl.NumberFormat();
const fmtInt = (n) => (n === null || n === undefined) ? '—' : nf.format(Math.round(n));
const fmtNum = (n) => (n === null || n === undefined) ? '—' : nf.format(n);
const fmtHour = (h) => String(h).padStart(2, '0') + ':00';

// shiftHour moves a UTC hour-of-day bucket into the displayed zone. The server
// groups by UTC hour, so this rotates the axis rather than re-querying. The
// offset is the one in effect now: across a DST change inside a long window the
// two halves differ by an hour, which no single rotation can express.
function shiftHour(utcHour) {
  const off = Math.round(tzOffsetMinutes() / 60);
  return ((+utcHour + off) % 24 + 24) % 24;
}
const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

// countryFlag turns an ISO 3166-1 alpha-2 code into its flag emoji, by mapping
// each letter to its regional-indicator codepoint. Same approach as the live
// spot page; the Twemoji Flags webfont in fonts.css covers browsers whose
// system emoji font has no flag glyphs.
function countryFlag(code) {
  if (!code || code.length !== 2 || !/^[a-z]{2}$/i.test(code)) return '';
  const base = 0x1F1E6 - 65;
  return String.fromCodePoint(code.toUpperCase().charCodeAt(0) + base) +
         String.fromCodePoint(code.toUpperCase().charCodeAt(1) + base);
}

// withFlag prefixes a label with its country flag when there is a code for it.
const withFlag = (code, label) => {
  const f = countryFlag(code);
  return f ? `${f} ${label}` : label;
};

// keyLabel renders a raw group key for the dimension it came from. `meta` is the
// dimension's companion value from the API — the ISO code when grouping by
// country name, which is what makes a flag possible.
function keyLabel(dim, key, meta) {
  if (key === '' || key === null || key === undefined) return '—';
  if (dim === 'country') return withFlag(meta, key);
  if (dim === 'cc') return withFlag(key, meta || key);
  if (dim === 'stream') return streamLabel(key);
  if (dim === 'hour') return fmtHour(shiftHour(key));
  if (dim === 'weekday') return WEEKDAYS[+key] || key;
  if (dim === 'snr_bucket') return `${key} to ${+key + 5} dB`;
  if (dim === 'dist_bucket') return `${fmtInt(+key)}–${fmtInt(+key + 500)} km`;
  return String(key);
}

// bucketLabel shortens a time-series bucket key for an axis tick. Hour buckets
// arrive as full ISO instants, which are unreadable as ticks — show the time,
// and let the date appear only when it changes.
function bucketLabel(bucket, raw, prev) {
  if (bucket !== 'hour') return raw;
  const hhmm = raw.slice(11, 16);
  const day = raw.slice(0, 10);
  return (prev && prev.slice(0, 10) === day) ? hhmm : `${hhmm} · ${day.slice(5)}`;
}

// spotMode resolves a spot's mode the same way the SQL modeExpr does: digital
// spots carry it directly, voice spots keep USB/LSB in voice_mode, and CW
// skimmer spots carry none at all — the stream is the mode.
function spotMode(s) {
  return s.mode || s.voice_mode || (s.stream === 'cwskimmer' ? 'CW' : '');
}

// streamLabel turns a stored stream key into its display name, falling back to
// the key itself if the server ever reports a stream the UI doesn't know.
function streamLabel(key) {
  return (META.stream_labels || {})[key] || key;
}

function fmtTS(sec) {
  if (!sec) return '—';
  const d = new Date(sec * 1000);
  return fmtInZone(d, DATE_TIME).replace(',', '') + ' ' + tzAbbrev(tzMode, d);
}

// fmtSpotTime renders a spot's RFC3339 timestamp to the second.
// bucketZone names the clock a time-series axis is on. Hourly buckets are real
// instants and follow the chosen zone; daily and weekly buckets are UTC
// calendar days grouped in SQL, and relabelling them would be a lie — a "day"
// is not the same day once you move zones.
function bucketZone(bucket) {
  return bucket === 'hour' ? (tzAbbrev() || 'UTC') : 'UTC';
}

// Column headings carry the zone, so an exported CSV is unambiguous too.
function timeColLabel() { return `Time (${tzAbbrev() || 'UTC'})`; }
function hourColLabel() { return `Hour (${tzAbbrev() || 'UTC'})`; }

function fmtSpotTime(iso) {
  const d = new Date(iso);
  return fmtInZone(d, Object.assign({ second: '2-digit' }, DATE_TIME)).replace(',', '');
}

/* ── Tooltip ───────────────────────────────────────────────────────────── */
const tip = document.getElementById('tooltip');
function showTip(evt, html) {
  tip.innerHTML = html;
  tip.classList.add('show');
  const pad = 14, r = tip.getBoundingClientRect();
  let x = evt.clientX + pad, y = evt.clientY + pad;
  if (x + r.width > window.innerWidth - 8) x = evt.clientX - r.width - pad;
  if (y + r.height > window.innerHeight - 8) y = evt.clientY - r.height - pad;
  tip.style.left = x + 'px'; tip.style.top = y + 'px';
}
const hideTip = () => tip.classList.remove('show');

/* ── SVG helpers ───────────────────────────────────────────────────────── */
const NS = 'http://www.w3.org/2000/svg';
function el(tag, attrs, parent) {
  const n = document.createElementNS(NS, tag);
  for (const k in attrs) if (attrs[k] !== null && attrs[k] !== undefined) n.setAttribute(k, attrs[k]);
  if (parent) parent.appendChild(n);
  return n;
}

// niceTicks returns ~count round tick values covering [lo, hi].
function niceTicks(lo, hi, count) {
  if (hi === lo) { hi = lo + 1; }
  const raw = (hi - lo) / count;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(s => s >= raw) || mag * 10;
  const out = [];
  for (let v = Math.ceil(lo / step) * step; v <= hi + 1e-9; v += step) out.push(+v.toFixed(10));
  return out;
}

// A bar path with the data end rounded (r=4) and the baseline end square, so
// the mark stays anchored to the axis it grows from.
function barPath(x, y, w, h, r, dir) {
  r = Math.max(0, Math.min(r, w / 2, h));
  if (dir === 'up')    return `M${x},${y + h}V${y + r}q0,-${r} ${r},-${r}h${w - 2 * r}q${r},0 ${r},${r}V${y + h}Z`;
  if (dir === 'down')  return `M${x},${y}V${y + h - r}q0,${r} ${r},${r}h${w - 2 * r}q${r},0 ${r},-${r}V${y}Z`;
  /* right */          return `M${x},${y}h${w - r}q${r},0 ${r},${r}v${h - 2 * r}q0,${r} -${r},${r}h-${w - r}Z`;
}

/* ── Chart: multi-series line ──────────────────────────────────────────── */
function renderLines(host, opts) {
  const { series, names, xLabels, metricLabel } = opts;
  host.innerHTML = '';
  if (!names.length || !xLabels.length) return empty(host);

  const W = Math.max(320, host.clientWidth - 28);
  // The right margin holds the endpoint labels, so they can never be clipped.
  const H = 260, m = { t: 12, r: names.length > 1 && names.length <= 4 ? 70 : 16, b: 34, l: 52 };
  const iw = W - m.l - m.r, ih = H - m.t - m.b;

  let lo = Infinity, hi = -Infinity;
  for (const n of names) for (const p of series[n]) {
    if (p.v === null) continue;
    lo = Math.min(lo, p.v); hi = Math.max(hi, p.v);
  }
  if (!isFinite(lo)) return empty(host);
  // Counts get a zero baseline; a dB scale does not — anchoring SNR at zero
  // would flatten the diurnal swing the chart exists to show.
  if (opts.zeroBaseline !== false) lo = Math.min(lo, 0);
  else { const pad = (hi - lo) * 0.12 || 1; lo -= pad; hi += pad; }
  if (hi === lo) hi = lo + 1;

  const xi = new Map(xLabels.map((l, i) => [l, i]));
  const X = (i) => m.l + (xLabels.length === 1 ? iw / 2 : (i / (xLabels.length - 1)) * iw);
  const Y = (v) => m.t + ih - ((v - lo) / (hi - lo)) * ih;

  const svg = el('svg', { viewBox: `0 0 ${W} ${H}`, height: H, role: 'img' }, host);
  for (const t of niceTicks(lo, hi, 5)) {
    el('line', { class: 'grid-line', x1: m.l, x2: m.l + iw, y1: Y(t), y2: Y(t) }, svg);
    el('text', { class: 'axis-text tabular', x: m.l - 8, y: Y(t) + 3, 'text-anchor': 'end' }, svg).textContent = fmtNum(t);
  }
  el('line', { class: 'axis-line', x1: m.l, x2: m.l + iw, y1: m.t + ih, y2: m.t + ih }, svg);
  el('text', { class: 'axis-title', x: m.l, y: 10 }, svg).textContent = metricLabel;

  const tickText = opts.tickLabel || ((l) => l);
  const every = Math.max(1, Math.ceil(xLabels.length / (iw / 74)));
  xLabels.forEach((l, i) => {
    if (i % every && i !== xLabels.length - 1) return;
    // The final tick is right-aligned so it can't spill past the plot edge.
    const last = i === xLabels.length - 1;
    el('text', {
      class: 'axis-text tabular', x: X(i), y: m.t + ih + 15,
      'text-anchor': last && every > 1 ? 'end' : 'middle',
    }, svg).textContent = tickText(l, i, xLabels[i - every]);
  });

  const colors = assignColors(names);
  const nameOf = opts.seriesLabel || ((n) => n);
  const endpoints = [];
  for (const n of names) {
    // Lay the series out along the x axis first: a slot per tick, null where
    // there is no data. That makes gaps explicit and the rest a single pass.
    const at = new Array(xLabels.length).fill(null);
    for (const p of series[n]) {
      if (p.v === null || p.v === undefined) continue;
      const i = xi.get(p.label);
      if (i !== undefined) at[i] = p.v;
    }

    // A null is "no data for this bucket", so the line breaks there rather than
    // interpolating across it. Gaps start a fresh sub-path (M instead of L).
    let d = '', open = false, last = -1;
    for (let i = 0; i < at.length; i++) {
      if (at[i] === null) { open = false; continue; }
      d += `${open ? 'L' : 'M'}${X(i)},${Y(at[i])}`;
      // A point with no neighbour either side draws no visible stroke, so it
      // gets a marker instead of vanishing.
      const alone = !open && (i + 1 >= at.length || at[i + 1] === null);
      if (alone) el('circle', { cx: X(i), cy: Y(at[i]), r: 4, fill: colors[n] }, svg);
      open = true; last = i;
    }
    if (last < 0) continue;
    el('path', { d, fill: 'none', stroke: colors[n], 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' }, svg);

    // Direct-label the endpoint when there are few enough series to stay
    // legible. A lone series needs no label — the chart title names it.
    if (names.length > 1 && names.length <= 4) {
      endpoints.push({ n, x: X(last), y: Y(at[last]) });
    }
  }

  // Series that converge — all falling to zero at the end of the window, say —
  // would otherwise stack their labels into an unreadable blur. Spread them
  // vertically, keeping the original order, and clamp to the plot.
  if (endpoints.length) {
    const GAP = 12;
    endpoints.sort((a, b) => a.y - b.y);
    for (let i = 1; i < endpoints.length; i++) {
      endpoints[i].y = Math.max(endpoints[i].y, endpoints[i - 1].y + GAP);
    }
    const overflow = endpoints[endpoints.length - 1].y - (m.t + ih);
    if (overflow > 0) for (const e of endpoints) e.y -= overflow;
    for (const e of endpoints) {
      e.y = Math.max(m.t + 4, e.y);
      el('text', { class: 'mark-label', x: e.x + 6, y: e.y + 3, fill: colors[e.n] }, svg)
        .textContent = nameOf(e.n);
    }
  }

  // Crosshair band: one hit rect per x position, full plot height.
  const cross = el('line', { class: 'crosshair', y1: m.t, y2: m.t + ih, opacity: 0 }, svg);
  xLabels.forEach((l, i) => {
    const bw = Math.max(iw / xLabels.length, 1);
    const r = el('rect', { class: 'hit', x: X(i) - bw / 2, y: m.t, width: bw, height: ih }, svg);
    r.addEventListener('mousemove', (e) => {
      cross.setAttribute('x1', X(i)); cross.setAttribute('x2', X(i)); cross.setAttribute('opacity', 1);
      const lines = names.map(n => {
        const p = series[n].find(q => q.label === l);
        if (!p || p.v === null) return '';
        return `<div><span style="color:${colors[n]}">■</span> ${escapeHTML(nameOf(n))}: <b>${fmtNum(p.v)}</b></div>`;
      }).join('');
      showTip(e, `<b>${escapeHTML(l)}</b>${lines}`);
    });
    r.addEventListener('mouseleave', () => { cross.setAttribute('opacity', 0); hideTip(); });
  });

  if (names.length >= 2) host.appendChild(legendEl(names, colors, nameOf));
}

/* ── Chart: vertical bars ──────────────────────────────────────────────── */
function renderVBars(host, opts) {
  const { rows, dim, metricLabel } = opts;
  host.innerHTML = '';
  if (!rows.length) return empty(host);

  const W = Math.max(320, host.clientWidth - 28);
  const H = 250, m = { t: 22, r: 12, b: 34, l: 52 };
  const iw = W - m.l - m.r, ih = H - m.t - m.b;

  const vals = rows.map(r => r.v).filter(v => v !== null);
  if (!vals.length) return empty(host);
  let hi = Math.max(0, ...vals), lo = Math.min(0, ...vals);
  if (hi === lo) hi = lo + 1;
  const Y = (v) => m.t + ih - ((v - lo) / (hi - lo)) * ih;
  const zero = Y(0);

  const slot = iw / rows.length;
  const bw = Math.max(2, slot - 2); // a 2px surface gap separates adjacent bars
  const color = cssVar(SERIES[0]);  // one series → one colour, never a value ramp
  const peak = rows.reduce((a, b) => (b.v ?? -Infinity) > (a.v ?? -Infinity) ? b : a, rows[0]);

  const svg = el('svg', { viewBox: `0 0 ${W} ${H}`, height: H, role: 'img' }, host);
  for (const t of niceTicks(lo, hi, 5)) {
    el('line', { class: 'grid-line', x1: m.l, x2: m.l + iw, y1: Y(t), y2: Y(t) }, svg);
    el('text', { class: 'axis-text tabular', x: m.l - 8, y: Y(t) + 3, 'text-anchor': 'end' }, svg).textContent = fmtNum(t);
  }
  el('line', { class: 'axis-line', x1: m.l, x2: m.l + iw, y1: zero, y2: zero }, svg);
  el('text', { class: 'axis-title', x: m.l, y: 10 }, svg).textContent = metricLabel;

  const every = Math.max(1, Math.ceil(rows.length / (iw / 46)));
  rows.forEach((r, i) => {
    const x = m.l + i * slot + (slot - bw) / 2;
    const v = r.v ?? 0;
    const y = Math.min(Y(v), zero), h = Math.abs(Y(v) - zero);
    el('path', { d: barPath(x, y, bw, Math.max(h, 1), 4, v < 0 ? 'down' : 'up'), fill: color }, svg);

    if (i % every === 0) {
      el('text', { class: 'axis-text tabular', x: x + bw / 2, y: m.t + ih + 15, 'text-anchor': 'middle' }, svg)
        .textContent = keyLabel(dim, r.key, r.meta);
    }
    // Direct-label only the extreme — a number on every bar is unreadable.
    if (r === peak && r.v !== null) {
      el('text', { class: 'mark-label', x: x + bw / 2, y: Y(v) - 6, 'text-anchor': 'middle' }, svg).textContent = fmtNum(r.v);
    }
    // Hit area spans the full slot height so the target is never pinpoint.
    const hit = el('rect', { class: 'hit', x: m.l + i * slot, y: m.t, width: slot, height: ih }, svg);
    hit.addEventListener('mousemove', (e) => showTip(e,
      `<b>${escapeHTML(keyLabel(dim, r.key, r.meta))}</b><div>${escapeHTML(metricLabel)}: <b>${fmtNum(r.v)}</b></div>` +
      (r.n !== undefined ? `<div>from ${fmtInt(r.n)} spots</div>` : '')));
    hit.addEventListener('mouseleave', hideTip);
  });
}

/* ── Chart: horizontal bars (rankings) ─────────────────────────────────── */
function renderHBars(host, opts) {
  const { rows, dim, metricLabel } = opts;
  host.innerHTML = '';
  if (!rows.length) return empty(host);

  const W = Math.max(320, host.clientWidth - 28);
  const rowH = 22, m = { t: 20, r: 60, b: 8, l: Math.min(170, Math.max(70, W * 0.28)) };
  const H = m.t + rows.length * rowH + m.b;
  const iw = W - m.l - m.r;

  const hi = Math.max(1, ...rows.map(r => r.v ?? 0));
  const color = cssVar(SERIES[0]);

  const svg = el('svg', { viewBox: `0 0 ${W} ${H}`, height: H, role: 'img' }, host);
  el('text', { class: 'axis-title', x: m.l, y: 10 }, svg).textContent = metricLabel;

  rows.forEach((r, i) => {
    const y = m.t + i * rowH;
    const bh = rowH - 2; // 2px surface gap between adjacent bars
    const w = Math.max(1, ((r.v ?? 0) / hi) * iw);
    el('text', { class: 'axis-text', x: m.l - 8, y: y + bh / 2 + 3, 'text-anchor': 'end' }, svg)
      .textContent = truncate(keyLabel(dim, r.key, r.meta), 24);
    el('path', { d: barPath(m.l, y, w, bh, 4, 'right'), fill: color }, svg);
    // Value sits outside the bar end, so it can never be clipped by a short bar.
    el('text', { class: 'axis-text tabular', x: m.l + w + 6, y: y + bh / 2 + 3 }, svg).textContent = fmtNum(r.v);

    const hit = el('rect', { class: 'hit', x: 0, y: y - 1, width: W, height: rowH }, svg);
    hit.addEventListener('mousemove', (e) => showTip(e,
      `<b>${escapeHTML(keyLabel(dim, r.key, r.meta))}</b><div>${escapeHTML(metricLabel)}: <b>${fmtNum(r.v)}</b></div>` +
      (r.n !== undefined ? `<div>from ${fmtInt(r.n)} spots</div>` : '') +
      (opts.onPick ? '<div style="color:var(--danger)">click to exclude</div>' : '')));
    hit.addEventListener('mouseleave', hideTip);
    if (opts.onPick) {
      hit.style.cursor = 'pointer';
      hit.addEventListener('click', () => { hideTip(); opts.onPick(r.key); });
    }
  });
}

/* ── Chart: heatmap ────────────────────────────────────────────────────── */
function renderHeatmap(host, opts) {
  const { cells, xKeys, yKeys, xDim, yDim, metricLabel } = opts;
  const xMeta = opts.xMeta || {}, yMeta = opts.yMeta || {};
  host.innerHTML = '';
  if (!cells.length) return empty(host);

  const byXY = new Map(cells.map(c => [c.x + ' ' + c.y, c]));
  const vals = cells.map(c => c.v).filter(v => v !== null);
  const lo = Math.min(...vals), hi = Math.max(...vals);

  const W = Math.max(360, host.clientWidth - 28);
  const m = { t: 20, r: 10, b: 26, l: Math.min(150, Math.max(60, W * 0.16)) };
  const cw = Math.max(10, (W - m.l - m.r) / xKeys.length);
  const ch = 22;
  const H = m.t + yKeys.length * ch + m.b;

  const svg = el('svg', { viewBox: `0 0 ${W} ${H}`, height: H, role: 'img' }, host);
  el('text', { class: 'axis-title', x: m.l, y: 10 }, svg).textContent = metricLabel;

  const everyX = Math.max(1, Math.ceil(xKeys.length / ((W - m.l - m.r) / 42)));
  xKeys.forEach((xk, i) => {
    if (i % everyX) return;
    el('text', { class: 'axis-text tabular', x: m.l + i * cw + cw / 2, y: H - 10, 'text-anchor': 'middle' }, svg)
      .textContent = truncate(keyLabel(xDim, xk, xMeta[xk]), 8);
  });

  yKeys.forEach((yk, j) => {
    const y = m.t + j * ch;
    el('text', { class: 'axis-text', x: m.l - 8, y: y + ch / 2 + 3, 'text-anchor': 'end' }, svg)
      .textContent = truncate(keyLabel(yDim, yk, yMeta[yk]), 20);
    xKeys.forEach((xk, i) => {
      const c = byXY.get(xk + ' ' + yk);
      const x = m.l + i * cw;
      // 2px surface gap separates cells instead of a border around each mark.
      el('rect', {
        x: x + 1, y: y + 1, width: Math.max(1, cw - 2), height: ch - 2, rx: 3,
        fill: c && c.v !== null ? rampColor(hi === lo ? 1 : (c.v - lo) / (hi - lo)) : 'var(--bg2)'
      }, svg);
      const hit = el('rect', { class: 'hit', x, y, width: cw, height: ch }, svg);
      hit.addEventListener('mousemove', (e) => showTip(e,
        `<b>${escapeHTML(keyLabel(yDim, yk, yMeta[yk]))} · ${escapeHTML(keyLabel(xDim, xk, xMeta[xk]))}</b>` +
        `<div>${escapeHTML(metricLabel)}: <b>${c ? fmtNum(c.v) : '—'}</b></div>` +
        (c ? `<div>from ${fmtInt(c.n)} spots</div>` : '')));
      hit.addEventListener('mouseleave', hideTip);
    });
  });

  const scale = document.createElement('div');
  scale.className = 'scale-legend';
  scale.innerHTML = `<span>${fmtNum(lo)}</span><span class="scale-ramp">` +
    RAMP.map(v => `<i style="background:${cssVar(v)}"></i>`).join('') +
    `</span><span>${fmtNum(hi)}</span><span style="margin-left:6px">${escapeHTML(metricLabel)}</span>`;
  host.appendChild(scale);
}

/* ── Shared chart bits ─────────────────────────────────────────────────── */
function empty(host) {
  host.innerHTML = '<div class="no-data">No spots match these filters</div>';
}

function legendEl(names, colors, nameOf) {
  const label = nameOf || ((n) => n);
  const d = document.createElement('div');
  d.className = 'legend';
  d.innerHTML = names.map(n =>
    `<span class="legend-item"><span class="legend-swatch" style="background:${colors[n]}"></span>${escapeHTML(label(n))}</span>`
  ).join('');
  return d;
}

const escapeHTML = (s) => String(s).replace(/[&<>"']/g, c =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const truncate = (s, n) => s.length > n ? s.slice(0, n - 1) + '…' : s;

// buildTable renders a plain HTML table — the accessible twin of every chart.
function buildTable(cols, rows) {
  const t = document.createElement('table');
  t.className = 'data-table';
  t.innerHTML =
    '<thead><tr>' + cols.map(c => `<th class="${c.num ? 'num' : ''}">${escapeHTML(c.label)}</th>`).join('') + '</tr></thead>' +
    '<tbody>' + rows.map(r => '<tr>' + cols.map(c => {
      const v = c.get(r);
      const cls = [c.num ? 'num' : '', c.key ? 'key-cell' : '', c.exclude ? 'excludable' : ''].join(' ');
      const attrs = c.exclude
        ? ` data-exclude="${c.exclude}" data-value="${escapeHTML(v)}" title="Click to exclude ${escapeHTML(v)}"`
        : '';
      return `<td class="${cls}"${attrs}>${escapeHTML(v === null || v === undefined ? '—' : v)}</td>`;
    }).join('') + '</tr>').join('') + '</tbody>';
  return t;
}

// tableTwins is the registry behind both the chart table-views and the
// standalone tables. Everything registered here can be exported as CSV.
const tableTwins = new Map();

function setTableTwin(hostId, cols, rows, name) {
  tableTwins.set(hostId, { cols, rows, name: name || hostId });
  const host = document.getElementById(hostId);
  const existing = host.parentElement.querySelector(`.chart-table[data-for="${hostId}"]`);
  if (existing) renderTwin(hostId, existing);
}

// renderDataTable draws a standalone table and registers it for export.
function renderDataTable(hostId, cols, rows, name) {
  tableTwins.set(hostId, { cols, rows, name: name || hostId });
  const wrap = document.getElementById(hostId);
  wrap.innerHTML = '';
  wrap.appendChild(buildTable(cols, rows));
}

function renderTwin(hostId, wrap) {
  const t = tableTwins.get(hostId);
  wrap.innerHTML = '';
  if (!t) return;
  // The twin carries its own export button: the chart card's header holds the
  // Table toggle, and the two belong together.
  const bar = document.createElement('div');
  bar.className = 'twin-bar';
  bar.innerHTML = `<button class="mini-btn" data-csv="${hostId}">⬇ CSV</button>`;
  wrap.appendChild(bar);
  wrap.appendChild(buildTable(t.cols, t.rows));
}
document.addEventListener('click', (e) => {
  const btn = e.target.closest('[data-toggle-table]');
  if (!btn) return;
  const hostId = btn.dataset.toggleTable;
  const host = document.getElementById(hostId);
  let wrap = host.parentElement.querySelector(`.chart-table[data-for="${hostId}"]`);
  if (wrap) { wrap.remove(); btn.classList.remove('on'); return; }
  wrap = document.createElement('div');
  wrap.className = 'chart-table';
  wrap.dataset.for = hostId;
  host.parentElement.appendChild(wrap);
  renderTwin(hostId, wrap);
  btn.classList.add('on');
});

/* ── Exclusions ────────────────────────────────────────────────────────────
 * A receiver's own skimmer submits most of the spots on its own cluster, so it
 * sits at the top of every ranking and flattens everyone else. Exclusions are
 * part of the shared filter, and persist per browser: "not me" is a standing
 * preference, not something to re-apply on every visit.
 */
const EXCLUDE_KEY = 'dxstats.exclude.v1';
const excluded = { spotter: new Set(), callsign: new Set() };

function loadExclusions() {
  try {
    const saved = JSON.parse(localStorage.getItem(EXCLUDE_KEY) || '{}');
    for (const kind of ['spotter', 'callsign']) {
      for (const v of (saved[kind] || [])) excluded[kind].add(v);
    }
  } catch (e) { /* corrupt or unavailable storage — start with none */ }
}

function saveExclusions() {
  try {
    localStorage.setItem(EXCLUDE_KEY, JSON.stringify({
      spotter: [...excluded.spotter], callsign: [...excluded.callsign],
    }));
  } catch (e) { /* private mode — exclusions just won't persist */ }
}

function exclude(kind, value) {
  const v = String(value || '').trim().toUpperCase();
  if (!v || excluded[kind].has(v)) return;
  excluded[kind].add(v);
  saveExclusions();
  renderExclusions();
  refresh();
}

function unexclude(kind, value) {
  excluded[kind].delete(value);
  saveExclusions();
  renderExclusions();
  refresh();
}

// renderExclusions draws the badge bar. It stays hidden while nothing is
// excluded, so the filter row doesn't carry an empty widget around.
function renderExclusions() {
  const bar = $('exclude-bar'), chips = $('exclude-chips');
  const all = [...[...excluded.spotter].map(v => ['spotter', v]),
               ...[...excluded.callsign].map(v => ['callsign', v])];
  bar.hidden = all.length === 0;
  chips.innerHTML = all.map(([kind, v]) =>
    `<span class="exclude-chip"><span class="kind">${kind === 'spotter' ? 'spotter' : 'call'}</span>` +
    `${escapeHTML(v)}<button title="Include ${escapeHTML(v)} again" ` +
    `data-unexclude="${kind}" data-value="${escapeHTML(v)}">×</button></span>`).join('');
}

document.addEventListener('click', (e) => {
  const btn = e.target.closest('[data-unexclude]');
  if (btn) { unexclude(btn.dataset.unexclude, btn.dataset.value); return; }
  const cell = e.target.closest('[data-exclude]');
  if (cell) exclude(cell.dataset.exclude, cell.dataset.value);
});

/* ── Filter state ──────────────────────────────────────────────────────── */
const $ = (id) => document.getElementById(id);
const multiVals = (id) => Array.from($(id).selectedOptions).map(o => o.value).filter(Boolean);

// filterQS turns the filter row into the query string every endpoint accepts.
function filterQS(extra) {
  const p = new URLSearchParams();
  const period = $('f-period').value;
  if (period === 'custom') {
    if ($('f-from').value) p.set('from', $('f-from').value);
    if ($('f-to').value) p.set('to', $('f-to').value);
  } else {
    p.set('hours', period);
  }
  for (const [key, id] of [['band', 'f-band'], ['mode', 'f-mode'], ['continent', 'f-continent']]) {
    const v = multiVals(id);
    if (v.length) p.set(key, v.join(','));
  }
  const cc = $('f-country').value;
  if (cc) p.set('country_code', cc);

  const streams = Array.from(document.querySelectorAll('.f-stream:checked')).map(c => c.value);
  if (streams.length) p.set('stream', streams.join(','));

  for (const [key, id] of [
    ['callsign', 'f-callsign'], ['spotter', 'f-spotter'], ['locator', 'f-locator'],
    ['q', 'f-text'], ['snr_min', 'f-snr-min'], ['snr_max', 'f-snr-max'],
    ['dist_min', 'f-dist-min'], ['dist_max', 'f-dist-max'],
  ]) {
    const v = $(id).value.trim();
    if (v !== '') p.set(key, v);
  }

  // The hour-of-day box is read in whatever zone is on display, but the server
  // only groups by UTC hour — so shift back before sending, or the filter and
  // the axis would disagree.
  for (const [key, id] of [['hour_min', 'f-hour-min'], ['hour_max', 'f-hour-max']]) {
    const v = $(id).value.trim();
    if (v === '') continue;
    const off = Math.round(tzOffsetMinutes() / 60);
    p.set(key, String(((+v - off) % 24 + 24) % 24));
  }
  if (excluded.spotter.size) p.set('spotter_exclude', [...excluded.spotter].join(','));
  if (excluded.callsign.size) p.set('callsign_exclude', [...excluded.callsign].join(','));

  for (const k in (extra || {})) if (extra[k] !== '' && extra[k] !== undefined) p.set(k, extra[k]);
  return p.toString();
}

// windowHours is the length of the selected period, used to pick a sensible
// time-series bucket (hourly for short windows, daily for long ones).
function windowHours() {
  const period = $('f-period').value;
  if (period !== 'custom') return +period;
  const a = Date.parse($('f-from').value), b = Date.parse($('f-to').value || Date.now());
  return isFinite(a) && isFinite(b) ? Math.max(1, (b - a) / 36e5) : 168;
}

/* ── Loading indicator ─────────────────────────────────────────────────────
 * The previous render is held at reduced opacity rather than replaced by a
 * skeleton, so nothing shifts when the data lands. This adds the missing half:
 * a spinner saying what is being fetched.
 */
const TAB_LABELS = {
  overview: 'overview', besttime: 'best-time analysis', rankings: 'rankings',
  spotters: 'spotter activity', compare: 'cross-tab', spots: 'spots',
};

let busyTimer = null;

// setBusy shows the indicator after a short delay, so a fast query doesn't
// produce a flash of spinner that is gone before it can be read.
function setBusy(on, text) {
  const box = $('busy');
  clearTimeout(busyTimer);
  if (!on) {
    box.hidden = true;
    document.body.removeAttribute('aria-busy');
    return;
  }
  $('busy-text').textContent = text;
  document.body.setAttribute('aria-busy', 'true');
  if (!box.hidden) return; // already showing — just swap the text
  busyTimer = setTimeout(() => { box.hidden = false; }, 120);
}

/* ── Fetching ──────────────────────────────────────────────────────────── */
let inflight = 0;
async function get(path, extra) {
  const url = api(path, filterQS(extra));
  const res = await fetch(url);
  const body = await res.json().catch(() => ({ error: 'bad response' }));
  if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);
  updateHeader(body);
  return body;
}

// updateHeader keeps the window and spot-count pills current from whatever the
// active tab happened to fetch. Scoped queries (a single callsign lookup, say)
// carry their own narrower filter, so only unscoped responses count.
function updateHeader(body) {
  if (body.filter && body.filter.from && body.filter.to) {
    const d = (iso) => fmtInZone(new Date(iso), { year: 'numeric', month: '2-digit', day: '2-digit' });
    $('hdr-window').textContent = d(body.filter.from) + ' → ' + d(body.filter.to);
  }
  if (!body.filter) return;
  // A lookup narrows to one station, and the Spotters tab implicitly narrows to
  // the spotter-bearing streams. Neither is what the user set in the filter row,
  // so neither should redefine the headline count.
  if (body.filter.callsign_exact || body.filter.spotter_exact) return;
  const userStreams = Array.from(document.querySelectorAll('.f-stream:checked')).map(c => c.value);
  if ((body.filter.stream || []).length !== userStreams.length) return;
  const total = body.summary ? body.summary.spots : body.total;
  if (total !== undefined) $('hdr-spots').textContent = fmtInt(total);
}

// run wraps a render pass: it dims the current content rather than replacing it
// with a skeleton, so a refetch never causes a layout jump.
async function run(fn, what) {
  inflight++;
  document.body.classList.add('loading');
  setBusy(true, `Loading ${what || TAB_LABELS[activeTab] || 'data'}…`);
  // The spinner reports progress; this slot is left for errors alone, so the
  // two don't say the same thing in two places.
  $('tab-status').textContent = '';
  try {
    await fn();
  } catch (e) {
    $('tab-status').textContent = 'Error: ' + e.message;
  } finally {
    if (--inflight === 0) {
      document.body.classList.remove('loading');
      setBusy(false);
    }
  }
}

/* ── Tab: Overview ─────────────────────────────────────────────────────── */
async function renderOverview() {
  // "Auto" keeps the tick count sane: hourly for short windows, daily beyond.
  const chosen = $('ov-bucket').value;
  const bucket = chosen === 'auto' ? (windowHours() <= 96 ? 'hour' : 'day') : chosen;
  const split = $('ov-split').value;

  const [sum, ser, streams, countries, bands, modes, calls] = await Promise.all([
    get('summary'),
    get('series', { bucket, metric: 'count', split_by: split, max_series: 10 }),  // 11th+ folds to "Other"
    get('breakdown', { group_by: 'stream', sort: 'count', limit: 10 }),
    get('breakdown', { group_by: 'country', sort: 'count', limit: 12 }),
    get('breakdown', { group_by: 'band', sort: 'count', limit: 14 }),
    get('breakdown', { group_by: 'mode', sort: 'count', limit: 10 }),
    get('breakdown', { group_by: 'callsign', sort: 'count', limit: 12 }),
  ]);

  const s = sum.summary;
  $('ov-tiles').innerHTML = [
    tile('Spots', fmtInt(s.spots), s.per_hour ? `${fmtNum(s.per_hour)} per hour` : ''),
    tile('Unique callsigns', fmtInt(s.callsigns), ''),
    tile('Countries', fmtInt(s.countries), ''),
    tile('Spotters', fmtInt(s.spotters), ''),
    tile('Average SNR', s.avg_snr === null ? '—' : fmtNum(s.avg_snr) + ' dB', s.max_snr === null ? '' : `peak ${fmtNum(s.max_snr)} dB`),
    tile('Furthest spot', s.max_dist === null ? '—' : fmtInt(s.max_dist) + ' km', s.avg_dist === null ? '' : `avg ${fmtInt(s.avg_dist)} km`),
  ].join('');

  const xLabels = [];
  for (const n of ser.names) for (const p of ser.series[n]) if (!xLabels.includes(p.label)) xLabels.push(p.label);
  xLabels.sort();
  renderLines($('ov-series'), {
    series: ser.series, names: ser.names, xLabels,
    metricLabel: `Spots per ${bucket} (${bucketZone(bucket)})`,
    tickLabel: (l, i, prev) => bucketLabel(bucket, l, prev),
    seriesLabel: (n) => n === OTHER_SERIES ? n : keyLabel(split, n),
  });
  setTableTwin('ov-series', [
    { label: bucket === 'hour' ? `Hour (${bucketZone(bucket)})` : 'Date (UTC)', get: r => r.label },
    ...ser.names.map(n => ({
      label: n === OTHER_SERIES ? n : keyLabel(split, n), num: true, get: r => fmtNum(r[n]),
    })),
  ], xLabels.map(l => {
    const row = { label: l };
    for (const n of ser.names) {
      const p = ser.series[n].find(q => q.label === l);
      row[n] = p ? p.v : null;
    }
    return row;
  }), `activity-by-${split || 'total'}`);

  hbarCard('ov-streams', streams.rows, 'stream', 'Spots');
  hbarCard('ov-countries', countries.rows, 'country', 'Spots');
  hbarCard('ov-bands', bands.rows, 'band', 'Spots');
  hbarCard('ov-modes', modes.rows, 'mode', 'Spots');
  hbarCard('ov-calls', calls.rows, 'callsign', 'Spots');
}

function tile(label, value, sub) {
  return `<div class="tile"><div class="tile-label">${escapeHTML(label)}</div>` +
    `<div class="tile-value">${escapeHTML(value)}</div>` +
    `<div class="tile-sub">${escapeHTML(sub || '')}</div></div>`;
}

function hbarCard(hostId, rows, dim, metricLabel) {
  const host = $(hostId);
  setTableTwin(hostId, breakdownCols(dim), rows, `by-${dim}`);

  // A one-bar bar chart is just a number wearing a rectangle — which is what
  // happens whenever the filter pins this dimension to a single value. Show
  // the number instead.
  if (rows.length === 1) {
    const r = rows[0];
    host.innerHTML =
      `<div class="solo"><div class="solo-value">${escapeHTML(fmtInt(r.count))}</div>` +
      `<div class="solo-label">${escapeHTML(metricLabel.toLowerCase())} — ` +
      `${escapeHTML(keyLabel(dim, r.key, r.meta))}</div>` +
      `<div class="solo-sub">${escapeHTML(fmtInt(r.calls))} unique callsigns` +
      (r.avg_snr === null ? '' : ` · ${escapeHTML(fmtNum(r.avg_snr))} dB average`) + `</div></div>`;
    return;
  }
  renderHBars(host, {
    rows: rows.map(r => ({ key: r.key, meta: r.meta, v: r.count, n: r.count })), dim, metricLabel,
  });
}

/* ── Tab: Best Time ────────────────────────────────────────────────────── */
async function renderBestTime() {
  get('summary').catch(() => {}); // header only; failure is not worth surfacing
  const metric = $('bt-metric').value;
  const [byHour, heat] = await Promise.all([
    get('breakdown', { group_by: 'hour', sort: 'count', limit: 24 }),
    get('matrix', { x: 'hour', y: 'band', metric, limit_y: 20 }),
  ]);

  const hours = byHour.rows.slice().sort((a, b) => +a.key - +b.key);
  renderVBars($('bt-hours'), {
    rows: hours.map(r => ({ key: r.key, v: r.count, n: r.count })),
    dim: 'hour', metricLabel: 'Spots',
  });
  setTableTwin('bt-hours', breakdownCols('hour'), hours, 'spots-by-utc-hour');

  // Average SNR gets its own plot on its own axis — never a second y-scale.
  const snrPts = hours.filter(r => r.avg_snr !== null).map(r => ({ label: fmtHour(r.key), v: r.avg_snr }));
  renderLines($('bt-snr'), {
    series: { 'Average SNR': snrPts }, names: ['Average SNR'],
    xLabels: snrPts.map(p => p.label),
    metricLabel: `Average SNR, dB (hour, ${tzAbbrev() || 'UTC'})`, zeroBaseline: false,
  });
  setTableTwin('bt-snr', [
    { label: hourColLabel(), get: r => fmtHour(shiftHour(r.key)) },
    { label: 'Average SNR (dB)', num: true, get: r => fmtNum(r.avg_snr) },
    { label: 'Peak SNR (dB)', num: true, get: r => fmtNum(r.max_snr) },
    { label: 'Spots', num: true, get: r => fmtInt(r.count) },
  ], hours, 'avg-snr-by-utc-hour');

  const metricLabel = $('bt-metric').selectedOptions[0].textContent;
  renderHeatmap($('bt-heat'), {
    cells: heat.cells, xKeys: heat.x_keys, yKeys: heat.y_keys,
    xDim: 'hour', yDim: 'band', metricLabel,
  });
  setTableTwin('bt-heat', [
    { label: 'Band', get: c => c.y },
    { label: hourColLabel(), get: c => fmtHour(shiftHour(c.x)) },
    { label: metricLabel, num: true, get: c => fmtNum(c.v) },
    { label: 'Spots', num: true, get: c => fmtInt(c.n) },
  ], heat.cells.slice().sort((a, b) => (b.v ?? -Infinity) - (a.v ?? -Infinity)), 'hour-by-band');

  renderAnswer(byHour, heat);
}

// renderAnswer states the finding in words — the chart supports it, but the
// headline shouldn't need decoding.
function renderAnswer(byHour, heat) {
  const host = $('bt-answer');
  if (!byHour.rows.length) {
    host.innerHTML = '<div class="answer-text">No spots match these filters, so there is nothing to rank yet.</div>';
    return;
  }
  const best = byHour.rows[0]; // already sorted by count desc
  const total = byHour.rows.reduce((a, r) => a + r.count, 0);
  const share = total ? Math.round((best.count / total) * 100) : 0;

  let bestCell = null;
  for (const c of heat.cells) if (c.v !== null && (!bestCell || c.v > bestCell.v)) bestCell = c;

  const scope = describeScope();
  host.innerHTML =
    `<div class="answer-hero">${fmtHour(shiftHour(best.key))}` +
    `<span class="unit"> ${escapeHTML(tzAbbrev() || 'UTC')}</span></div>` +
    `<div class="answer-text">Busiest hour ${escapeHTML(scope)}: <strong>${fmtInt(best.count)} spots</strong> ` +
    `(${share}% of the period's total)` +
    (best.avg_snr !== null ? `, averaging <strong>${fmtNum(best.avg_snr)} dB</strong> SNR` : '') +
    (bestCell ? `. The strongest hour-and-band combination is <strong>${escapeHTML(bestCell.y)} at ` +
      `${fmtHour(shiftHour(bestCell.x))} ${escapeHTML(tzAbbrev() || 'UTC')}</strong>.` : '.') +
    `</div>`;
}

// describeScope summarises the active filters for the answer sentence.
function describeScope() {
  const bits = [];
  const bands = multiVals('f-band'), modes = multiVals('f-mode');
  if (bands.length) bits.push('on ' + bands.join('/'));
  if (modes.length) bits.push('in ' + modes.join('/'));
  const ccSel = $('f-country').selectedOptions[0];
  if (ccSel && ccSel.value) bits.push('for ' + ccSel.textContent.replace(/\s*\([\d,]+\)$/, '').trim());
  const conts = multiVals('f-continent');
  if (conts.length && !ccSel?.value) bits.push('in ' + conts.join('/'));
  return bits.length ? bits.join(' ') : 'across all spots';
}

/* ── Tab: Rankings ─────────────────────────────────────────────────────── */
async function renderRankings() {
  get('summary').catch(() => {}); // header only; failure is not worth surfacing
  const dim = $('rk-dim').value, metric = $('rk-metric').value, limit = $('rk-limit').value;
  const res = await get('breakdown', { group_by: dim, sort: metric, limit });
  const metricLabel = $('rk-metric').selectedOptions[0].textContent;
  const pick = {
    count: r => r.count, calls: r => r.calls, countries: r => r.countries, spotters: r => r.spotters,
    avg_snr: r => r.avg_snr, max_snr: r => r.max_snr, avg_dist: r => r.avg_dist,
    max_dist: r => r.max_dist, avg_wpm: r => r.avg_wpm,
  }[metric] || (r => r.count);

  renderHBars($('rk-chart'), {
    rows: res.rows.map(r => ({ key: r.key, meta: r.meta, v: pick(r), n: r.count })), dim, metricLabel,
  });
  renderDataTable('rk-table', breakdownCols(dim), res.rows, `ranking-by-${dim}`);
}

function breakdownCols(dim) {
  return [
    { label: dimLabel(dim), key: true, get: r => keyLabel(dim, r.key, r.meta) },
    { label: 'Spots', num: true, get: r => fmtInt(r.count) },
    { label: 'Callsigns', num: true, get: r => fmtInt(r.calls) },
    // A country count is always 1 when the rows *are* countries — drop it.
    ...(dim === 'country' || dim === 'cc' ? [] :
      [{ label: 'Countries', num: true, get: r => fmtInt(r.countries) }]),
    { label: 'Avg SNR', num: true, get: r => fmtNum(r.avg_snr) },
    { label: 'Peak SNR', num: true, get: r => fmtNum(r.max_snr) },
    { label: 'Avg km', num: true, get: r => fmtInt(r.avg_dist) },
    { label: 'Max km', num: true, get: r => fmtInt(r.max_dist) },
    { label: 'First seen', get: r => fmtTS(r.first_ts) },
    { label: 'Last seen', get: r => fmtTS(r.last_ts) },
  ];
}

let META = { dimensions: [], metrics: [] };
const dimLabel = (k) => (META.dimensions.find(d => d.key === k) || {}).label || k;

/* ── Tab: Spotters ─────────────────────────────────────────────────────── */

// Only these streams carry a spotter callsign. Digital and voice spots are
// produced by this receiver's own decoders and have no submitting station, so
// counting them here would make every total meaningless.
const SPOTTER_STREAMS = 'dxcluster,localspot,cwskimmer';

// spotterScope narrows the shared filter to spotter-bearing streams, unless the
// user has already picked specific sources in the filter row.
function spotterScope(extra) {
  const chosen = Array.from(document.querySelectorAll('.f-stream:checked')).map(c => c.value);
  return Object.assign({ stream: chosen.length ? chosen.join(',') : SPOTTER_STREAMS }, extra || {});
}

async function renderSpotters() {
  // The tiles below are scoped to spotter-bearing streams; this unscoped call
  // is purely so the header keeps reporting the whole filtered period.
  get('summary').catch(() => {});
  const metric = $('sr-metric').value, limit = $('sr-limit').value;
  const [sum, rank] = await Promise.all([
    get('summary', spotterScope()),
    get('breakdown', spotterScope({ group_by: 'spotter', sort: metric, limit })),
  ]);

  const s = sum.summary;
  const top = rank.rows[0];
  $('sr-tiles').innerHTML = [
    tile('Unique spotters', fmtInt(s.spotters), 'stations submitting spots'),
    tile('Spots submitted', fmtInt(s.spots), s.per_hour ? `${fmtNum(s.per_hour)} per hour` : ''),
    tile('Callsigns spotted', fmtInt(s.callsigns), `${fmtInt(s.countries)} countries`),
    tile('Most active', top ? top.key : '—',
      top ? `${fmtInt(top.count)} spots · ${fmtInt(top.calls)} callsigns` : ''),
  ].join('');

  const metricLabel = $('sr-metric').selectedOptions[0].textContent;
  const pick = { count: r => r.count, calls: r => r.calls, countries: r => r.countries, bands: r => r.bands };
  renderHBars($('sr-chart'), {
    rows: rank.rows.map(r => ({ key: r.key, v: (pick[metric] || pick.count)(r), n: r.count })),
    dim: 'spotter', metricLabel,
    onPick: (key) => exclude('spotter', key),
  });
  setTableTwin('sr-chart', spotterCols(), rank.rows, 'spotters');
  renderDataTable('sr-table', spotterCols(), rank.rows, 'spotter-activity');
}

function spotterCols() {
  return [
    { label: 'Spotter', key: true, exclude: 'spotter', get: r => r.key },
    { label: 'Spots', num: true, get: r => fmtInt(r.count) },
    { label: 'Callsigns', num: true, get: r => fmtInt(r.calls) },
    { label: 'Countries', num: true, get: r => fmtInt(r.countries) },
    { label: 'Bands', num: true, get: r => fmtInt(r.bands) },
    { label: 'Avg SNR', num: true, get: r => fmtNum(r.avg_snr) },
    { label: 'Max km', num: true, get: r => fmtInt(r.max_dist) },
    { label: 'First spot', get: r => fmtTS(r.first_ts) },
    { label: 'Last spot', get: r => fmtTS(r.last_ts) },
  ];
}

// lookupCallsign answers "who spotted this station, when, and on what?" — an
// exact-match query, so G3ABC never picks up G3ABCD.
async function lookupCallsign() {
  const call = $('lk-call').value.trim().toUpperCase();
  const summary = $('lk-summary'), table = $('lk-table');
  if (!call) {
    summary.innerHTML = '';
    table.innerHTML = '<div class="no-data">Enter a callsign to see who spotted it</div>';
    return;
  }
  $('lk-hint').textContent = 'Searching…';

  const scope = { callsign_exact: call };
  const [sum, spots, bySpotter, byBand] = await Promise.all([
    get('summary', scope),
    get('spots', Object.assign({ limit: 200 }, scope)),
    get('breakdown', Object.assign({ group_by: 'spotter', sort: 'count', limit: 50 }, scope)),
    get('breakdown', Object.assign({ group_by: 'band', sort: 'count', limit: 20 }, scope)),
  ]);
  $('lk-hint').textContent = '';

  if (!spots.total) {
    summary.innerHTML = '';
    table.innerHTML = `<div class="no-data">No spots of ${escapeHTML(call)} in this period</div>`;
    return;
  }

  // Split "submitted by a spotter" from "heard by this receiver's own decoders",
  // which carry no spotter. Both are spots of the callsign, but only the former
  // has anyone to credit. Counts come from the aggregates, not the capped page,
  // so they stay right however many spots there are.
  const submitted = bySpotter.rows.reduce((a, r) => a + r.count, 0);
  const ownDecoder = sum.summary.spots - submitted;
  const bands = byBand.rows.map(r => r.key);
  const home = spots.spots.find(sp => sp.country_code);
  const n = bySpotter.rows.length;

  const credit = n
    ? `<strong>${fmtInt(submitted)}</strong> submitted by <strong>${fmtInt(n)} spotter${n === 1 ? '' : 's'}</strong>`
    : 'none submitted by a spotter';
  const own = ownDecoder > 0
    ? `, <strong>${fmtInt(ownDecoder)}</strong> heard directly by this receiver` : '';

  summary.innerHTML =
    `<div class="lookup-result"><div class="lookup-hero">` +
    (home ? escapeHTML(countryFlag(home.country_code)) + ' ' : '') +
    `${escapeHTML(call)}</div>` +
    `<div class="lookup-sub">Spotted <strong>${fmtInt(sum.summary.spots)} times</strong>` +
    (bands.length ? ` on ${escapeHTML(bands.join(', '))}` : '') + ` — ${credit}${own}. ` +
    `Last heard <strong>${escapeHTML(fmtTS(sum.summary.last_ts))}</strong>, ` +
    `first <strong>${escapeHTML(fmtTS(sum.summary.first_ts))}</strong>.` +
    (spots.total > spots.spots.length
      ? ` The table below shows the latest ${fmtInt(spots.spots.length)}.` : '') +
    `</div>` +
    (n ? '<div class="spotter-chips">' + bySpotter.rows.map(r =>
      `<span class="spotter-chip">${escapeHTML(r.key)} <b>${fmtInt(r.count)}</b></span>`).join('') + '</div>' : '') +
    `</div>`;

  renderDataTable('lk-table', [
    { label: timeColLabel(), get: s => fmtSpotTime(s.timestamp) },
    { label: 'Spotter', key: true, get: s => s.spotter || '— (own decoder)' },
    { label: 'Freq kHz', num: true, get: s => (s.freq_hz / 1000).toFixed(1) },
    { label: 'Band', get: s => s.band },
    { label: 'Mode', get: s => spotMode(s) },
    { label: 'SNR', num: true, get: s => fmtNum(s.snr) },
    { label: 'WPM', num: true, get: s => s.wpm || '' },
    { label: 'km', num: true, get: s => s.distance_km ? fmtInt(s.distance_km) : '' },
    { label: 'Source', get: s => streamLabel(s.stream) },
    { label: 'Comment', get: s => s.comment || s.message || '' },
  ], spots.spots, `spots-of-${call}`);
}

/* ── Tab: Compare ──────────────────────────────────────────────────────── */
async function renderCompare() {
  get('summary').catch(() => {}); // header only; failure is not worth surfacing
  const x = $('cm-x').value, y = $('cm-y').value, metric = $('cm-metric').value;
  const res = await get('matrix', { x, y, metric, limit_y: 30 });
  const metricLabel = $('cm-metric').selectedOptions[0].textContent;
  renderHeatmap($('cm-heat'), {
    cells: res.cells, xKeys: res.x_keys, yKeys: res.y_keys, xDim: x, yDim: y, metricLabel,
    xMeta: res.x_meta, yMeta: res.y_meta,
  });
  setTableTwin('cm-heat', [
    { label: dimLabel(y), key: true, get: c => keyLabel(y, c.y, (res.y_meta || {})[c.y]) },
    { label: dimLabel(x), key: true, get: c => keyLabel(x, c.x, (res.x_meta || {})[c.x]) },
    { label: metricLabel, num: true, get: c => fmtNum(c.v) },
    { label: 'Spots', num: true, get: c => fmtInt(c.n) },
  ], res.cells.slice().sort((a, b) => (b.v ?? -Infinity) - (a.v ?? -Infinity)), `${y}-by-${x}`);
}

/* ── Tab: Spots ────────────────────────────────────────────────────────── */
let spotOffset = 0;
const SPOT_PAGE = 100;

async function renderSpots() {
  const res = await get('spots', { limit: SPOT_PAGE, offset: spotOffset });
  $('sp-count').textContent = `${fmtInt(res.total)} spots · showing ${spotOffset + 1}–${Math.min(spotOffset + SPOT_PAGE, res.total)}`;
  $('sp-prev').disabled = spotOffset === 0;
  $('sp-next').disabled = spotOffset + SPOT_PAGE >= res.total;

  const cols = [
    { label: timeColLabel(), get: s => fmtSpotTime(s.timestamp) },
    { label: 'Callsign', key: true, get: s => s.callsign },
    { label: 'Freq kHz', num: true, get: s => (s.freq_hz / 1000).toFixed(1) },
    { label: 'Band', get: s => s.band },
    { label: 'Mode', get: s => spotMode(s) },
    { label: 'SNR', num: true, get: s => fmtNum(s.snr) },
    { label: 'Country', get: s => withFlag(s.country_code, s.country || '') },
    { label: 'Cont', get: s => s.continent || '' },
    { label: 'Grid', get: s => s.locator || '' },
    { label: 'km', num: true, get: s => s.distance_km ? fmtInt(s.distance_km) : '' },
    { label: 'Spotter', get: s => s.spotter || '' },
    { label: 'Source', get: s => streamLabel(s.stream) },
    { label: 'Comment', get: s => s.comment || s.message || '' },
  ];
  renderDataTable('sp-table', cols, res.spots, 'spots');
}

// csvValue exports the underlying value, not the display string: thousands
// separators would import as text, and an em dash is not "no data" to a
// spreadsheet. A column can override with its own `csv` accessor.
function csvValue(col, row) {
  if (col.csv) return col.csv(row);
  const v = col.get(row);
  if (v === null || v === undefined || v === '—') return '';
  return col.num ? String(v).replace(/,/g, '') : v;
}

function toCSV(cols, rows) {
  const q = (v) => `"${String(v ?? '').replace(/"/g, '""')}"`;
  // CRLF per RFC 4180, and a BOM so Excel reads the UTF-8 in country names.
  return '\ufeff' + [cols.map(c => q(c.label)).join(','),
    ...rows.map(r => cols.map(c => q(csvValue(c, r))).join(','))].join('\r\n');
}

// csvFilename stamps the export with its dataset and the active time window,
// so a folder of these stays tellable apart.
function csvFilename(name) {
  const win = ($('hdr-window').textContent || '').replace(/[^0-9-]+/g, '_').replace(/^_|_$/g, '');
  return `dxcluster-${name}${win ? '-' + win : ''}.csv`;
}

function downloadCSV(hostId) {
  const t = tableTwins.get(hostId);
  if (!t || !t.rows || !t.rows.length) return;
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([toCSV(t.cols, t.rows)], { type: 'text/csv;charset=utf-8' }));
  a.download = csvFilename(t.name);
  a.click();
  URL.revokeObjectURL(a.href);
}

document.addEventListener('click', (e) => {
  const btn = e.target.closest('[data-csv]');
  if (btn) downloadCSV(btn.dataset.csv);
});

/* ── Timezone switch ───────────────────────────────────────────────────── */

// applyTZ re-labels everything zone-dependent and re-renders. No refetch is
// needed for the absolute timestamps — the API already returned UTC instants,
// and the choice is purely how they are displayed.
function applyTZ(mode, { rerender = true } = {}) {
  tzMode = mode;
  try { localStorage.setItem(TZ_KEY, mode); } catch (e) { /* private mode */ }

  document.querySelectorAll('.tz-btn').forEach(b =>
    b.classList.toggle('active', b.dataset.tz === mode));
  const abbrev = tzAbbrev() || 'UTC';
  document.querySelectorAll('.tz-name').forEach(e => { e.textContent = abbrev; });

  const inst = $('tz-instance');
  inst.title = INSTANCE_ZONE
    ? `Receiver time — ${INSTANCE_ZONE}`
    : 'This instance does not report a timezone';
  if (rerender) refresh();
}

function initTZ() {
  // Without a zone from /api/description there is no meaningful "instance"
  // time, so the option is disabled rather than quietly showing UTC.
  if (!INSTANCE_ZONE) {
    $('tz-instance').disabled = true;
  }
  let saved = 'utc';
  try { saved = localStorage.getItem(TZ_KEY) || 'utc'; } catch (e) { /* ignore */ }
  if (saved === 'instance' && !INSTANCE_ZONE) saved = 'utc';

  document.querySelectorAll('.tz-btn').forEach(b =>
    b.addEventListener('click', () => { if (!b.disabled) applyTZ(b.dataset.tz); }));
  applyTZ(saved, { rerender: false });
}

/* ── Tab wiring ────────────────────────────────────────────────────────── */
const RENDERERS = {
  overview: renderOverview, besttime: renderBestTime,
  rankings: renderRankings, spotters: renderSpotters,
  compare: renderCompare, spots: renderSpots,
};
let activeTab = 'overview';

function selectTab(name) {
  activeTab = name;
  document.querySelectorAll('.tab').forEach(t => t.classList.toggle('active', t.dataset.tab === name));
  document.querySelectorAll('.tabpanel').forEach(p => { p.hidden = p.id !== 'panel-' + name; });
  location.hash = name;
  refresh();
}

function refresh() {
  run(RENDERERS[activeTab]);
}

/* ── Facets: keep the dropdowns to values that actually exist ──────────── */
async function loadFacets() {
  // Facets are scoped by period alone — narrowing on band shouldn't remove the
  // other bands from the picker you'd use to widen again.
  const p = new URLSearchParams();
  const period = $('f-period').value;
  if (period === 'custom') {
    if ($('f-from').value) p.set('from', $('f-from').value);
    if ($('f-to').value) p.set('to', $('f-to').value);
  } else p.set('hours', period);

  const res = await fetch(api('facets', p.toString()));
  if (!res.ok) return;
  const { facets } = await res.json();

  fillMulti('f-band', facets.band);
  fillModes(facets.mode);
  fillMulti('f-continent', facets.continent);

  const sel = $('f-country'), prev = sel.value;
  sel.innerHTML = '<option value="">All countries</option>' +
    (facets.country || []).map(c =>
      `<option value="${escapeHTML(c.key)}">` +
      `${escapeHTML(withFlag(c.key, c.name || c.key))} (${fmtInt(c.count)})</option>`).join('');
  sel.value = prev;
}

// fillModes builds the mode picker from the cluster's own list of modes
// (META.mode_groups, grouped by the source that produces them) rather than from
// whatever the database happens to hold. Counts come from the facets, so a mode
// with no spots this period is still offered — shown as (0) — instead of
// silently disappearing from the filter.
function fillModes(observed) {
  const sel = $('f-mode');
  const prev = new Set(multiVals('f-mode'));
  const counts = new Map((observed || []).map(v => [v.key, v.count]));
  const known = new Set();

  let html = '';
  for (const g of (META.mode_groups || [])) {
    html += `<optgroup label="${escapeHTML(g.label)} source">`;
    for (const mode of g.modes) {
      known.add(mode);
      const n = counts.get(mode) || 0;
      html += `<option value="${escapeHTML(mode)}"${prev.has(mode) ? ' selected' : ''}` +
        `${n ? '' : ' class="empty"'}>${escapeHTML(mode)} (${fmtInt(n)})</option>`;
    }
    html += '</optgroup>';
  }

  // Anything the database holds that the canonical list doesn't know about —
  // a new upstream mode, or historical data from an older decoder.
  const extra = [...counts.keys()].filter(k => k && !known.has(k));
  if (extra.length) {
    html += '<optgroup label="Other">' + extra.map(k =>
      `<option value="${escapeHTML(k)}"${prev.has(k) ? ' selected' : ''}>` +
      `${escapeHTML(k)} (${fmtInt(counts.get(k))})</option>`).join('') + '</optgroup>';
  }
  sel.innerHTML = html;
}

function fillMulti(id, values) {
  const sel = $(id), prev = new Set(multiVals(id));
  sel.innerHTML = (values || []).map(v =>
    `<option value="${escapeHTML(v.key)}"${prev.has(v.key) ? ' selected' : ''}>${escapeHTML(v.key)} (${fmtInt(v.count)})</option>`
  ).join('');
}

/* ── Init ──────────────────────────────────────────────────────────────── */
async function init() {
  if (window.RX_CALLSIGN) {
    $('rx-call').textContent = window.RX_CALLSIGN;
    $('rx-loc').textContent = window.RX_LOCATION || '';
    $('rx-info').style.display = '';
  }

  try {
    META = await (await fetch(`${BASE}/api/stats/meta`)).json();
  } catch (e) { /* pickers fall back to empty; the error surfaces on first query */ }

  const dimOpts = META.dimensions.map(d => `<option value="${d.key}">${escapeHTML(d.label)}</option>`).join('');
  const metricOpts = META.metrics.map(m => `<option value="${m.key}">${escapeHTML(m.label)}</option>`).join('');
  $('rk-dim').innerHTML = dimOpts; $('rk-dim').value = 'country';
  $('rk-metric').innerHTML = metricOpts; $('rk-metric').value = 'count';
  $('cm-x').innerHTML = dimOpts; $('cm-x').value = 'hour';
  $('cm-y').innerHTML = dimOpts; $('cm-y').value = 'country';
  $('cm-metric').innerHTML = metricOpts; $('cm-metric').value = 'count';

  $('f-period').addEventListener('change', async () => {
    $('custom-range').style.display = $('f-period').value === 'custom' ? '' : 'none';
    await run(loadFacets, 'filter options');
    refresh();
  });
  $('btn-apply').addEventListener('click', () => { spotOffset = 0; refresh(); });
  $('btn-clear').addEventListener('click', () => {
    document.querySelectorAll('.filter-body input').forEach(i => {
      if (i.type === 'checkbox') i.checked = false; else i.value = '';
    });
    ['f-band', 'f-mode', 'f-continent'].forEach(id =>
      Array.from($(id).options).forEach(o => { o.selected = false; }));
    $('f-country').value = '';
    $('f-period').value = '24';
    $('custom-range').style.display = 'none';
    excluded.spotter.clear(); excluded.callsign.clear();
    saveExclusions(); renderExclusions();
    spotOffset = 0;
    run(loadFacets, 'filter options').then(refresh);
  });
  // Enter in any text field applies, matching the Apply button.
  document.querySelectorAll('.filter-body input[type=text], .filter-body input:not([type])').forEach(i =>
    i.addEventListener('keydown', e => { if (e.key === 'Enter') { spotOffset = 0; refresh(); } }));

  document.querySelectorAll('.tab').forEach(t =>
    t.addEventListener('click', () => selectTab(t.dataset.tab)));

  ['ov-bucket', 'ov-split', 'bt-metric', 'rk-dim', 'rk-metric', 'rk-limit',
   'sr-metric', 'sr-limit', 'cm-x', 'cm-y', 'cm-metric']
    .forEach(id => $(id).addEventListener('change', refresh));

  $('btn-unexclude-all').addEventListener('click', () => {
    excluded.spotter.clear(); excluded.callsign.clear();
    saveExclusions(); renderExclusions(); refresh();
  });

  const doLookup = () => run(lookupCallsign, `spots of ${$('lk-call').value.trim().toUpperCase() || 'callsign'}`);
  $('lk-go').addEventListener('click', doLookup);
  $('lk-call').addEventListener('keydown', e => { if (e.key === 'Enter') doLookup(); });

  $('sp-prev').addEventListener('click', () => { spotOffset = Math.max(0, spotOffset - SPOT_PAGE); refresh(); });
  $('sp-next').addEventListener('click', () => { spotOffset += SPOT_PAGE; refresh(); });
  // Redraw on resize — the SVGs are sized from their container's pixel width.
  let rt;
  window.addEventListener('resize', () => { clearTimeout(rt); rt = setTimeout(refresh, 250); });

  initTZ();
  loadExclusions();
  renderExclusions();

  await run(loadFacets, 'filter options');
  const hash = location.hash.slice(1);
  selectTab(RENDERERS[hash] ? hash : 'overview');
}

init();
})();
