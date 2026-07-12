/**
 * UberSDR DX Cluster — Web Terminal
 *
 * Bidirectional WebSocket terminal proxied to the local telnet server.
 * Security: all server output is inserted via textContent (never innerHTML)
 * to prevent XSS. The WebSocket URL is derived from the current page origin
 * and BASE_PATH — never from user input.
 */

(function () {
  'use strict';

  // ── State ──────────────────────────────────────────────────────────────────

  const SCROLLBACK_LIMIT = 2000; // max <span> children in the output div

  let ws = null;
  let connected = false;
  let callsignSent = false;
  let pendingCallsign = '';

  // ── DOM references (populated in init) ────────────────────────────────────

  let modal, overlay, output, input, sendBtn, connectBtn, disconnectBtn,
      callsignInput, spotpassInput, loginRow, inputRow, statusEl, closeBtn, termOpenBtn,
      spotBtn, spotOverlay, spotFreqInput, spotCallInput, spotCommentInput, spotSendBtn;

  // ── Helpers ────────────────────────────────────────────────────────────────

  /**
   * Append text to the terminal output div.
   * Uses textContent on a <span> to prevent XSS — no HTML interpretation.
   */
  function appendOutput(text) {
    if (!output) return;
    const span = document.createElement('span');
    span.textContent = text;
    output.appendChild(span);
    // Trim oldest lines to stay within scrollback limit
    while (output.childNodes.length > SCROLLBACK_LIMIT) {
      output.removeChild(output.firstChild);
    }
    // Auto-scroll to bottom
    output.scrollTop = output.scrollHeight;
  }

  function setStatus(msg, cls) {
    if (!statusEl) return;
    statusEl.textContent = msg;
    statusEl.className = 'term-status ' + (cls || '');
  }

  function setConnected(state) {
    connected = state;
    if (inputRow)      inputRow.style.display      = state ? 'flex'  : 'none';
    if (loginRow)      loginRow.style.display      = state ? 'none'  : 'flex';
    if (connectBtn)    connectBtn.disabled          = state;
    if (disconnectBtn) disconnectBtn.style.display  = state ? ''      : 'none';
    if (spotBtn)       spotBtn.disabled             = !state;
    // Toggle the header button's active (green) state
    if (termOpenBtn) termOpenBtn.classList.toggle('active', state);
    if (input && state) input.focus();
  }

  // ── WebSocket URL ──────────────────────────────────────────────────────────

  /**
   * Build the WebSocket URL from the current page origin + BASE_PATH.
   * Handles http→ws and https→wss automatically.
   * Respects the X-Forwarded-Prefix reverse-proxy path (window.BASE_PATH).
   */
  function buildWsUrl() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const base = (window.BASE_PATH || '').replace(/\/$/, '');
    return proto + '//' + location.host + base + '/api/terminal';
  }

  // ── Connection ─────────────────────────────────────────────────────────────

  const LS_CALLSIGN_KEY = 'ubersdr_terminal_callsign';
  const LS_SPOTPASS_KEY = 'ubersdr_terminal_spotpass';

  function connect() {
    if (ws) return;

    const callsign = (callsignInput ? callsignInput.value.trim() : '').toUpperCase();
    if (!callsign) {
      setStatus('Enter your callsign first', 'term-status-error');
      if (callsignInput) callsignInput.focus();
      return;
    }
    const spotpass = spotpassInput ? spotpassInput.value : '';
    // Persist callsign and spotpass so they auto-populate next time
    try { localStorage.setItem(LS_CALLSIGN_KEY, callsign); } catch (_) {}
    try { localStorage.setItem(LS_SPOTPASS_KEY, spotpass); } catch (_) {}
    // Build the login line: "CALLSIGN" or "CALLSIGN PASSWORD"
    pendingCallsign = spotpass ? callsign + ' ' + spotpass : callsign;
    callsignSent = false;

    setStatus('Connecting…', '');
    if (output) output.textContent = '';

    const url = buildWsUrl();
    try {
      ws = new WebSocket(url);
    } catch (e) {
      setStatus('WebSocket error: ' + e.message, 'term-status-error');
      ws = null;
      return;
    }

    ws.onopen = function () {
      setStatus('Connected', 'term-status-ok');
      setConnected(true);
    };

    ws.onmessage = function (evt) {
      const text = evt.data;

      // Auto-respond to the callsign prompt (pendingCallsign may include a password)
      if (!callsignSent && text.indexOf('callsign') !== -1) {
        callsignSent = true;
        ws.send(pendingCallsign + '\r\n');
      }

      appendOutput(text);
    };

    ws.onclose = function (evt) {
      ws = null;
      callsignSent = false;
      setConnected(false);
      if (evt.wasClean) {
        setStatus('Disconnected', '');
      } else {
        setStatus('Connection lost (code ' + evt.code + ')', 'term-status-error');
      }
    };

    ws.onerror = function () {
      // onerror is always followed by onclose — let onclose handle the UI
      setStatus('WebSocket error', 'term-status-error');
    };
  }

  function disconnect() {
    closeSpotModal();
    if (ws) {
      ws.send('bye\r\n');
      ws.close(1000, 'user disconnect');
      ws = null;
    }
    setConnected(false);
    setStatus('Disconnected', '');
  }

  // ── Spot modal ─────────────────────────────────────────────────────────────

  function openSpotModal() {
    if (!spotOverlay || !connected) return;
    if (spotFreqInput)    spotFreqInput.value    = '';
    if (spotCallInput)    spotCallInput.value    = '';
    if (spotCommentInput) spotCommentInput.value = '';
    spotOverlay.style.display = 'flex';
    if (spotFreqInput) spotFreqInput.focus();
  }

  function closeSpotModal() {
    if (spotOverlay) spotOverlay.style.display = 'none';
  }

  function sendSpot() {
    if (!ws || !connected) return;
    const freq    = (spotFreqInput    ? spotFreqInput.value.trim()    : '');
    const call    = (spotCallInput    ? spotCallInput.value.trim().toUpperCase() : '');
    const comment = (spotCommentInput ? spotCommentInput.value.trim() : '');
    if (!freq || !call) {
      if (spotFreqInput && !freq) { spotFreqInput.focus(); }
      else if (spotCallInput)     { spotCallInput.focus(); }
      return;
    }
    let cmd = 'DX ' + freq + ' ' + call;
    if (comment) cmd += ' ' + comment;
    appendOutput('> ' + cmd + '\n');
    ws.send(cmd + '\r\n');
    closeSpotModal();
    if (input) input.focus();
  }

  // Expose so the inline onclick in index.html can call it
  window.closeSpotModal = closeSpotModal;

  // ── Send ───────────────────────────────────────────────────────────────────

  function sendInput() {
    if (!ws || !connected) return;
    const line = input.value;
    // Echo the command locally so the user sees what they typed
    appendOutput('> ' + line + '\n');
    ws.send(line + '\r\n');
    input.value = '';
  }

  // ── Modal ──────────────────────────────────────────────────────────────────

  function openModal() {
    if (overlay) {
      overlay.style.display = 'flex';
      // Restore saved callsign if the field is empty
      if (callsignInput && !callsignInput.value.trim()) {
        try {
          const saved = localStorage.getItem(LS_CALLSIGN_KEY);
          if (saved) callsignInput.value = saved;
        } catch (_) {}
      }
      // Restore saved spot password if the field is empty
      if (spotpassInput && !spotpassInput.value) {
        try {
          const savedPass = localStorage.getItem(LS_SPOTPASS_KEY);
          if (savedPass) spotpassInput.value = savedPass;
        } catch (_) {}
      }
      // Scroll output to bottom so the latest content is visible on re-open
      if (output) output.scrollTop = output.scrollHeight;
      if (!connected) {
        // Auto-connect if we have a callsign (either just restored or already typed)
        if (callsignInput && callsignInput.value.trim()) {
          connect();
        } else if (callsignInput) {
          callsignInput.focus();
        }
      } else if (input) {
        input.focus();
      }
    }
  }

  function closeModal() {
    if (overlay) overlay.style.display = 'none';
    // Disconnect the session when the modal is closed
    disconnect();
  }

  // ── Init ───────────────────────────────────────────────────────────────────

  function init() {
    modal            = document.getElementById('term-modal');
    overlay          = document.getElementById('term-overlay');
    output           = document.getElementById('term-output');
    input            = document.getElementById('term-input');
    sendBtn          = document.getElementById('term-send');
    connectBtn       = document.getElementById('term-connect');
    disconnectBtn    = document.getElementById('term-disconnect');
    callsignInput    = document.getElementById('term-callsign');
    spotpassInput    = document.getElementById('term-spotpass');
    loginRow         = document.getElementById('term-login-row');
    inputRow         = document.getElementById('term-input-row');
    statusEl         = document.getElementById('term-status');
    closeBtn         = document.getElementById('term-close');
    termOpenBtn      = document.getElementById('term-open-btn');
    spotBtn          = document.getElementById('term-spot-btn');
    spotOverlay      = document.getElementById('term-spot-overlay');
    spotFreqInput    = document.getElementById('spot-freq');
    spotCallInput    = document.getElementById('spot-call');
    spotCommentInput = document.getElementById('spot-comment');
    spotSendBtn      = document.getElementById('term-spot-send');

    if (!modal) return; // terminal HTML not present

    // Close on overlay click
    if (overlay) {
      overlay.addEventListener('click', function (e) {
        if (e.target === overlay) closeModal();
      });
    }

    // Close button
    if (closeBtn) closeBtn.addEventListener('click', closeModal);

    // Connect / Disconnect buttons
    if (connectBtn)    connectBtn.addEventListener('click', connect);
    if (disconnectBtn) disconnectBtn.addEventListener('click', disconnect);

    // Callsign / spotpass inputs: Enter to connect
    if (callsignInput) {
      callsignInput.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') connect();
      });
    }
    if (spotpassInput) {
      spotpassInput.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') connect();
      });
    }

    // Spot button
    if (spotBtn) spotBtn.addEventListener('click', openSpotModal);
    if (spotSendBtn) spotSendBtn.addEventListener('click', sendSpot);

    // Auto-uppercase the callsign field as the user types
    if (spotCallInput) {
      spotCallInput.addEventListener('input', function () {
        var pos = spotCallInput.selectionStart;
        var up  = spotCallInput.value.toUpperCase();
        if (up !== spotCallInput.value) {
          spotCallInput.value = up;
          spotCallInput.setSelectionRange(pos, pos);
        }
      });
    }

    // Spot modal field keyboard handling
    [spotFreqInput, spotCallInput, spotCommentInput].forEach(function (el) {
      if (!el) return;
      el.addEventListener('keydown', function (e) {
        if (e.key === 'Enter')  sendSpot();
        if (e.key === 'Escape') closeSpotModal();
      });
    });

    // Close spot overlay on backdrop click
    if (spotOverlay) {
      spotOverlay.addEventListener('click', function (e) {
        if (e.target === spotOverlay) closeSpotModal();
      });
    }

    // Send button
    if (sendBtn) sendBtn.addEventListener('click', sendInput);

    // Input: Enter to send, Escape to close modal
    if (input) {
      input.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') {
          sendInput();
        } else if (e.key === 'Escape') {
          closeModal();
        }
      });
    }

    // Terminal icon button in the header
    const termBtn = document.getElementById('term-open-btn');
    if (termBtn) termBtn.addEventListener('click', openModal);

    setConnected(false);
    setStatus('Not connected', '');
  }

  // Expose openModal globally so the header button can call it
  window.openTerminal  = openModal;
  window.closeTerminal = closeModal;

  /**
   * Send a command via the terminal, opening the modal first if needed.
   * If not connected but a callsign is saved in localStorage, auto-connects
   * and sends the command once the session handshake completes.
   * If no callsign is saved, opens the modal and lets the user connect manually.
   * @param {string} cmd
   */
  window.termSendCommand = function(cmd) {
    if (connected && ws) {
      // Already connected — open modal, echo locally and send immediately
      openModal();
      appendOutput('> ' + cmd + '\n');
      ws.send(cmd + '\r\n');
    } else {
      // openModal() will auto-connect if a callsign is available.
      // Hook ws.onopen *before* calling openModal so we catch the connection.
      const savedCall = (() => {
        try { return localStorage.getItem(LS_CALLSIGN_KEY) || ''; } catch (_) { return ''; }
      })();
      if (savedCall) {
        // openModal → connect() creates ws synchronously; patch onopen right after
        openModal();
        if (ws) {
          const prevOnOpen = ws.onopen;
          ws.onopen = function(evt) {
            if (prevOnOpen) prevOnOpen.call(ws, evt);
            // Wait for the DX Spider callsign handshake before sending
            setTimeout(function() {
              if (connected && ws) {
                appendOutput('> ' + cmd + '\n');
                ws.send(cmd + '\r\n');
              }
            }, 800);
          };
        }
      } else {
        // No saved callsign — open modal and pre-fill the command input
        openModal();
        if (input) { input.value = cmd; }
      }
    }
  };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
