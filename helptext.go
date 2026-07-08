package main

// helpText is the DX cluster command reference shown via the HELP command,
// the web UI's help modal, and the desktop client's help dialog.
// It lives in its own file so the client build can cp it directly.
const helpText = `
UberSDR DX Cluster — Command Reference
=======================================

Commands can be abbreviated: SET/FILTER → SET/F, SHOW/DX → SH/DX, etc.

SIMPLE FILTERS  (AND-combined; multiple values within a field are OR-combined)
  set/filter band <bands>       Filter by band (comma-separated)
                                  e.g. set/filter band 20m
                                       set/filter band 40m,20m,15m
                                  Bands: 2200m 630m 600m 160m 80m 60m 40m 30m 20m 17m 15m 12m 11m 10m 6m

  set/filter mode <modes>       Filter by mode (comma-separated)
                                  Digital: FT8 FT4 WSPR JS8 FT2
                                  CW:      CW
                                  Voice:   USB LSB
                                  e.g. set/filter mode FT8,FT4,WSPR

  set/filter type <types>       Filter by activity type (comma-separated)
                                  Types: digital  cw  voice  dx
                                  e.g. set/filter type cw,digital

  set/filter cont <conts>       Filter by continent (comma-separated)
                                  Codes: EU NA SA AF AS OC AN
                                  e.g. set/filter cont EU,NA

  set/filter country <codes>    Filter by country ISO 3166-1 alpha-2 (comma-separated)
                                  e.g. set/filter country DE,PA,ON

  set/filter call <prefixes>    Filter by callsign prefix (comma-separated)
                                  e.g. set/filter call DL,VK,ZL

  set/filter snr <dB>           Minimum SNR threshold, e.g. set/filter snr 10
  set/filter maxsnr <dB>        Maximum SNR threshold, e.g. set/filter maxsnr 30

CLEARING SIMPLE FILTERS
  clear/filter                  Clear ALL active filters (simple + slots)
  clear/filter band             Clear band filter only
  clear/filter mode             Clear mode filter only
  clear/filter type             Clear type filter only
  clear/filter cont             Clear continent filter only
  clear/filter country          Clear country filter only
  clear/filter call             Clear callsign prefix filter only
  clear/filter snr              Clear minimum SNR filter
  clear/filter maxsnr           Clear maximum SNR filter

DX SPIDER ACCEPT/REJECT FILTERS  (numbered slots 0-9, default slot 1)
  accept/spots [N] <expr>       Accept spots matching expression (slot N, default 1)
  reject/spots [N] <expr>       Reject spots matching expression (slot N, default 1)
  accept/rbn [N] <expr>         Accept RBN spots matching expression
  reject/rbn [N] <expr>         Reject RBN spots matching expression

  Filter expression fields (combine with AND / OR / NOT and parentheses):
    on <freq>                   Band, region or kHz range:
                                  on 20m            (band)
                                  on hf             (region: 1800-30000 kHz)
                                  on hf/cw          (all HF CW segments)
                                  on 20m/cw         (that band's CW segment)
                                  on 20m/ssb        (that band's phone segment)
                                  on 14000/14070    (explicit kHz range)
    freq <freq>                 Alias for 'on'
    call <prefix>               Callsign prefix (comma-separated), e.g. call DL,PA
    by <call>                   Spotter callsign prefix
    cont <code>                 Continent, e.g. cont EU,NA
    country <code>              Country code, e.g. country DE
    mode <mode>                 Mode, e.g. mode FT8,FT4
    type <type>                 Stream type: digital cw voice dx
    info <text>                 Comment/message substring
    iota                        IOTA spots
    qsl                         QSL/VIA spots
    all                         Match everything

  BOOLEAN LOGIC:
    Use 'and', 'or', 'not' and parentheses to build complex expressions.
    IMPORTANT: when using OR, always use brackets to group terms.
    Adjacent terms without 'and'/'or' are implicitly AND-combined.

  SLOT SEMANTICS (matching DX Spider):
    Each numbered slot (0-9) can hold BOTH a reject rule and an accept rule.
    Slots are evaluated in order. Within a slot, reject is checked first:
      - if reject matches → spot is DROPPED
      - if accept matches → spot is KEPT
    If only reject rules exist: everything passes EXCEPT matching spots.
    If only accept rules exist: ONLY matching spots pass.

  Examples:
    accept/spots on 20m
    accept/spots 1 on hf and call DL
    accept/spots on hf and (cont EU or cont NA)
    reject/spots on hf/cw
    reject/spots on hf/cw and not info iota
    accept/spots not on hf/cw or info iota
    reject/spots 1 on hf/cw
    accept/spots 2 on hf
    accept/rbn on 40m
    clear/spots 1
    clear/spots all

CLEARING SLOT FILTERS
  clear/spots [N|all]           Clear accept/reject spot filter slot N (or all slots)
  clear/rbn [N|all]             Clear accept/reject RBN filter slot N (or all slots)

SPOT STREAM TOGGLES  (each stream can be enabled/disabled independently)
  set/dx                        Enable ALL spots (DX Spider compat, default: on)
  unset/dx                      Disable ALL spots

  set/digital                   Enable digital decoder spots (FT8/FT4/WSPR/JS8, default: off)
  unset/digital                 Disable digital decoder spots

  set/rbn                       Enable CW/RBN skimmer spots (default: on)
  unset/rbn                     Disable CW/RBN skimmer spots
  set/skimmer                   Alias for set/rbn
  unset/skimmer                 Alias for unset/rbn

  set/voice                     Enable voice activity spots (default: on)
  unset/voice                   Disable voice activity spots

  set/dxcluster                 Enable DX cluster spots (default: off)
  unset/dxcluster               Disable DX cluster spots
  set/cluster                   Alias for set/dxcluster
  unset/cluster                 Alias for unset/dxcluster

INFORMATION
  show/filter                   Show all currently active filters
  show/dx [N] [options]         Query spot history (up to 30 days, default: last 20 spots)
    Options (can be combined):
      <N>                         Number of spots to show (default: 20, max: 200)
      <from>-<to>                 Spot offset range, e.g. 30-40 (spots 30 to 40)
      on <band>                   Filter by band, e.g. on 20m
      on <kHz>-<kHz>              Filter by frequency range, e.g. on 14000-14033
      call <prefix>               Filter by callsign prefix, e.g. call DL
      prefix <prefix>             Alias for call
      <callsign>                  Bare callsign/prefix (no keyword needed), e.g. g0vgs
      by <call>                   Filter by spotter callsign, e.g. by G3ABC
      info <text>                 Search comment/message text, e.g. info iota
      iota [<ref>]                Search for IOTA spots, e.g. iota or iota EU-064
      qsl                         Search for QSL/VIA info in comments
      day <N>                     Look back N days (default: 1, max: 30)
      day <from>-<to>             Day range, e.g. day 7-14 (7 to 14 days ago)
      cont <code>                 Filter by continent, e.g. cont EU
      country <code>              Filter by country code, e.g. country DE
      mode <mode>                 Filter by mode, e.g. mode FT8
      type <type>                 Filter by stream type: digital cw voice dx
    Examples:
      show/dx
      show/dx 5
      show/dx 20
      show/dx on 20m
      show/dx 10 on 20m
      show/dx g0vgs
      show/dx 10 g0vgs
      show/dx 30-40
      show/dx 14000-14033
      show/dx iota
      show/dx iota EU-064
      show/dx qsl
      show/dx day 30
      show/dx 20 call 9a on 20m day 30
      show/dx on 40m mode CW day 3
  show/status                   Show cluster status: uptime, clients, DB stats
                                  e.g. show/status  (or sh/stat)
  show/qrz <callsign>           Look up callbook details for a callsign
                                  e.g. show/qrz g1tlh  (or sh/qrz g1tlh)
                                  Data provided by qrz.com via UberSDR
  show/prefix <call|pfx> ...    Show DXCC country, CQ/ITU zone, lat/lon
                                  e.g. show/prefix G  (or sh/pre G)
  show/dxcc <prefix> [opts]     Show recent spots for a DXCC country
                                  e.g. show/dxcc G on 20m
  show/heading <call|pfx> ...   Show beam heading + distance from receiver
                                  e.g. show/heading VK  (or sh/hea VK)
  show/dxstats [days]           Show spot totals per day (default: 31 days)
  show/hfstats [days]           Show spot totals per band (default: 31 days)
  show/time                     Show current UTC time
  show/version                  Show cluster software version
  help [<command>]              Show this help text

SESSION
  bye / quit                    Disconnect from the cluster

NOTES
  - Simple filters (set/filter) are AND-combined across fields
  - Multiple values within a field are OR-matched (e.g. band 40m,20m = 40m OR 20m)
  - Accept/reject slots follow DX Spider semantics:
      reject is checked first; if any reject slot matches, the spot is dropped
      if accept slots are set, the spot must match at least one to pass
  - Callsign prefix matching is case-insensitive prefix match (DL matches DL1ABC)
  - Country codes are exact ISO 3166-1 alpha-2 match (case-insensitive)
  - Filters persist for the duration of your connection only
  - show/dx queries the persistent database (up to 30 days of history)

`
