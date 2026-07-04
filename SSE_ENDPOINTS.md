# UberSDR Server-Sent Events (SSE) Endpoints

UberSDR exposes three public SSE endpoints for real-time spot and signal data.
All three share the same connection model:

- **Protocol:** `text/event-stream` (standard SSE / EventSource)
- **Access:** Public — no authentication required
- **Rate limit:** Maximum **2 concurrent connections per IP address**
  (exempt for IPs listed in `timeout_bypass_ips` or presenting a valid `bypass_password`)
- **On connect:** An initial comment frame confirms the connection and sets the
  browser reconnect delay:
  ```
  : connected to <stream name>
  retry: 3000
  ```
- **503 response:** Returned when the underlying subsystem (decoder, CW skimmer,
  or noise floor monitor) is not enabled on this instance.

---

## 1. Digital Decoder Stream

```
GET /api/decoder/stream
```

Streams real-time digital mode decodes (FT8, FT4, WSPR, JS8, etc.) as they are
produced by the on-board decoder.

### Query Parameters

| Parameter  | Type   | Description |
|------------|--------|-------------|
| `mode`     | string | Filter to a single mode: `FT8`, `FT4`, `WSPR`, `JS8`, `FT2`. Omit for all modes. |
| `band`     | string | Filter to a single band name, e.g. `20m_FT8`. Omit for all bands. |
| `callsign` | string | Comma-delimited list of up to 20 callsigns (exact match, case-insensitive), e.g. `G4ABC,W1AW`. Omit for all callsigns. |

### Data Event

One event per decode. Fired immediately when the decoder produces a result.

```
data: { ... }
```

#### JSON Fields

| Field          | Type    | Always present | Description |
|----------------|---------|:--------------:|-------------|
| `type`         | string  | ✓ | Always `"decode"` |
| `mode`         | string  | ✓ | Decode mode: `FT8`, `FT4`, `WSPR`, `JS8`, `FT2` |
| `band`         | string  | ✓ | Band name, e.g. `"20m_FT8"` |
| `callsign`     | string  | ✓ | Decoded callsign |
| `frequency`    | integer | ✓ | Dial frequency in Hz |
| `snr`          | integer | ✓ | Signal-to-noise ratio in dB |
| `timestamp`    | string  | ✓ | ISO 8601 UTC timestamp of the decode |
| `locator`      | string  | — | Maidenhead grid locator (when present in message) |
| `country`      | string  | — | DXCC entity name, e.g. `"Germany"` |
| `country_code` | string  | — | ISO 3166-1 alpha-2 country code, e.g. `"DE"` |
| `continent`    | string  | — | Continent code: `EU`, `NA`, `SA`, `AF`, `AS`, `OC`, `AN` |
| `message`      | string  | — | Full decoded message text |
| `distance_km`  | float   | — | Great-circle distance from the station to this receiver (km) |
| `bearing_deg`  | float   | — | True bearing from this receiver to the station (degrees) |

#### Example

```json
{
  "type": "decode",
  "mode": "FT8",
  "band": "20m_FT8",
  "callsign": "DL1ABC",
  "locator": "JO31",
  "country": "Germany",
  "country_code": "DE",
  "continent": "EU",
  "snr": -10,
  "frequency": 14074000,
  "message": "DL1ABC G4XYZ -10",
  "timestamp": "2026-07-04T15:00:00Z",
  "distance_km": 850.2,
  "bearing_deg": 112.5
}
```

### Heartbeat Event

Sent every **1 second** to keep the connection alive and provide the timestamp
of the most recent decode.

```
event: heartbeat
data: {"last_spot": "2026-07-04T15:00:00Z"}
```

`last_spot` is `null` if no decode has been received since the server started.

---

## 2. CW Skimmer Stream

```
GET /api/cwskimmer/stream
```

Streams real-time CW spots produced by the on-board CW Skimmer.

### Query Parameters

| Parameter  | Type   | Description |
|------------|--------|-------------|
| `band`     | string | Filter to a single amateur band, e.g. `40m`. Omit for all bands. |
| `callsign` | string | Comma-delimited list of up to 20 callsigns (exact match, case-insensitive). Omit for all callsigns. |

### Data Event

One event per CW spot. Fired immediately when the skimmer produces a spot.

```
data: { ... }
```

#### JSON Fields

| Field          | Type    | Always present | Description |
|----------------|---------|:--------------:|-------------|
| `type`         | string  | ✓ | Always `"cw_spot"` |
| `band`         | string  | ✓ | Amateur band label, e.g. `"40m"` |
| `frequency`    | float   | ✓ | Spot frequency in Hz |
| `callsign`     | string  | ✓ | Spotted callsign |
| `spotter`      | string  | ✓ | Callsign of the skimmer that produced the spot |
| `snr`          | integer | ✓ | Signal-to-noise ratio in dB |
| `wpm`          | integer | ✓ | Detected CW speed in words per minute |
| `timestamp`    | string  | ✓ | ISO 8601 UTC timestamp of the spot |
| `comment`      | string  | — | Decoded CW comment (e.g. `"CQ"`) |
| `country`      | string  | — | DXCC entity name |
| `country_code` | string  | — | ISO 3166-1 alpha-2 country code |
| `continent`    | string  | — | Continent code: `EU`, `NA`, `SA`, `AF`, `AS`, `OC`, `AN` |
| `cq_zone`      | integer | — | CQ zone number |
| `name_fmt`     | string  | — | Operator name from QRZ lookup (when QRZ lookup is enabled) |
| `state`        | string  | — | State or region from QRZ lookup |
| `grid`         | string  | — | Maidenhead grid square from QRZ lookup |
| `latitude`     | float   | — | Station latitude in decimal degrees |
| `longitude`    | float   | — | Station longitude in decimal degrees |
| `distance_km`  | float   | — | Great-circle distance from the station to this receiver (km) |
| `bearing_deg`  | float   | — | True bearing from this receiver to the station (degrees) |

#### Example

```json
{
  "type": "cw_spot",
  "band": "40m",
  "frequency": 7035300,
  "callsign": "SM4SEF",
  "spotter": "MM3NDH",
  "snr": 12,
  "wpm": 19,
  "comment": "CQ",
  "country": "Sweden",
  "country_code": "SE",
  "continent": "EU",
  "cq_zone": 18,
  "distance_km": 1234.5,
  "bearing_deg": 45.0,
  "timestamp": "2026-07-04T15:00:00Z"
}
```

### Heartbeat Event

Sent every **~30 seconds** to keep the connection alive.

```
event: heartbeat
data: {"last_spot": "2026-07-04T15:00:00Z"}
```

`last_spot` is `null` if no spot has been received since the server started.

---

## 3. Voice Activity Stream

```
GET /api/voice-activity/stream
```

Streams real-time SSB voice activity detected by the noise floor monitor's
multi-frame FFT analysis pipeline. Each event represents a confirmed active
voice signal on a specific band and dial frequency.

### Broadcast Cadence

Unlike the decoder and CW skimmer streams (which fire on every new event),
the voice activity stream uses a **diff + periodic replay** strategy:

- A station is broadcast **immediately** when first confirmed active.
- If the **DX callsign changes** (e.g. a DX cluster spot arrives after first
  detection), the station is re-broadcast immediately.
- A station that remains continuously active is re-broadcast every **60 seconds**,
  ensuring a client that connects mid-session sees all active stations within
  at most 60 seconds.
- When a station stops transmitting, no explicit "gone" event is sent. Clients
  should expire entries not updated within their chosen window (e.g. 10 minutes).

### Query Parameters

| Parameter | Type   | Description |
|-----------|--------|-------------|
| `band`    | string | Filter to a single amateur band, e.g. `20m`. Omit for all bands. |

### Data Event

One event per active voice signal per broadcast cycle.

```
data: { ... }
```

#### JSON Fields

| Field                | Type    | Always present | Description |
|----------------------|---------|:--------------:|-------------|
| `type`               | string  | ✓ | Always `"voice_activity"` |
| `band`               | string  | ✓ | Amateur band label, e.g. `"20m"` |
| `timestamp`          | string  | ✓ | ISO 8601 UTC timestamp of this broadcast |
| `estimated_dial_freq`| integer | ✓ | Estimated dial frequency in Hz (rounded to nearest 100 Hz, preferring 500 Hz boundaries) |
| `mode`               | string  | ✓ | Inferred mode: `"USB"` (bands ≥ 10 MHz) or `"LSB"` (bands < 10 MHz) |
| `snr`                | float   | ✓ | Signal-to-noise ratio in dB |
| `confidence`         | float   | ✓ | Detection confidence score 0.0–1.0 (minimum 0.7 for confirmed signals) |
| `avg_signal_db`      | float   | ✓ | Average signal power in dBFS |
| `peak_signal_db`     | float   | ✓ | Peak signal power in dBFS |
| `bandwidth`          | integer | ✓ | Detected signal bandwidth in Hz (typically 1500–4000 Hz for SSB) |
| `dx_callsign`        | string  | — | Callsign from DX cluster spot at this frequency (within configured TTL) |
| `dx_country`         | string  | — | DXCC entity name for the DX callsign |
| `dx_country_code`    | string  | — | ISO 3166-1 alpha-2 country code for the DX callsign |
| `dx_continent`       | string  | — | Continent code for the DX callsign |

#### Example — with DX enrichment

```json
{
  "type": "voice_activity",
  "band": "20m",
  "timestamp": "2026-07-04T15:00:00Z",
  "estimated_dial_freq": 14225000,
  "mode": "USB",
  "snr": 18.9,
  "confidence": 0.87,
  "avg_signal_db": -105.1,
  "peak_signal_db": -98.8,
  "bandwidth": 2500,
  "dx_callsign": "DL1ABC",
  "dx_country": "Germany",
  "dx_country_code": "DE",
  "dx_continent": "EU"
}
```

#### Example — without DX enrichment

```json
{
  "type": "voice_activity",
  "band": "40m",
  "timestamp": "2026-07-04T15:00:00Z",
  "estimated_dial_freq": 7155000,
  "mode": "LSB",
  "snr": 12.3,
  "confidence": 0.81,
  "avg_signal_db": -108.4,
  "peak_signal_db": -102.1,
  "bandwidth": 2700
}
```

### Heartbeat Event

Sent every **30 seconds** to keep the connection alive.

```
event: heartbeat
data: {"last_activity": "2026-07-04T15:00:00Z"}
```

`last_activity` is `null` if no voice activity has been broadcast since the
server started.

### Client Implementation Notes

- Upsert each event into a local map keyed by `(band, estimated_dial_freq)`.
- Store the `timestamp` of the last received event for each entry.
- Expire (hide) entries whose `timestamp` is older than your chosen window
  (e.g. 10 minutes) — this is how station disappearance is handled client-side.
- The `dx_callsign` field may be absent on the first event for a station and
  appear on a subsequent event once a DX cluster spot is indexed. Always use
  the most recently received value.

---

## Connection Example (JavaScript)

```javascript
const es = new EventSource('/api/voice-activity/stream?band=20m');

es.addEventListener('message', (e) => {
  const spot = JSON.parse(e.data);
  console.log(spot.type, spot.band, spot.estimated_dial_freq, spot.dx_callsign);
});

es.addEventListener('heartbeat', (e) => {
  const hb = JSON.parse(e.data);
  console.log('heartbeat, last activity:', hb.last_activity);
});

es.onerror = () => {
  // EventSource auto-reconnects after retry: 3000 ms
};
```

The same pattern applies to `/api/decoder/stream` and `/api/cwskimmer/stream`,
substituting the appropriate event field names.
