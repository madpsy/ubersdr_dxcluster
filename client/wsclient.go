package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// wsReadLimit caps a single inbound WebSocket message. Server output arrives
// as small text frames, so 1 MB is far more than enough.
const wsReadLimit = 1 << 20

// Reconnect backoff bounds. After a dropped or failed connection the client
// waits before re-dialling, doubling the delay each consecutive failure up to
// maxReconnectDelay. The delay resets to minReconnectDelay after any session
// that actually connected.
const (
	minReconnectDelay = 1 * time.Second
	maxReconnectDelay = 10 * time.Second
)

// DXClusterClient maintains a WebSocket connection to a single instance's
// DX cluster terminal, auto-logs in with a callsign, relays every chunk of
// received text to a sink, and reconnects automatically until stopped.
//
// The terminal protocol is plain text over WebSocket (identical to the web
// UI's terminal): on connect the server prints a banner ending in a line that
// contains the word "callsign"; the client replies with "<CALLSIGN>\r\n" and
// then receives a continuous stream of DX spots. Commands may be sent at any
// time as "<line>\r\n"; "bye\r\n" ends the session cleanly.
type DXClusterClient struct {
	url      string
	callsign string

	onText   func(string)       // every chunk of server text
	onStatus func(string, bool) // human status message, connected?

	mu      sync.Mutex
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	running bool
	wg      sync.WaitGroup
}

// NewDXClusterClient creates a client for the given terminal WebSocket URL.
// onText receives raw server output; onStatus receives connection-state
// changes. Both callbacks are invoked from the client's own goroutine, so any
// UI work inside them must be marshalled onto the UI thread by the caller.
func NewDXClusterClient(url, callsign string, onText func(string), onStatus func(string, bool)) *DXClusterClient {
	return &DXClusterClient{
		url:      url,
		callsign: callsign,
		onText:   onText,
		onStatus: onStatus,
	}
}

// Start begins connecting in the background. It is safe to call once.
func (c *DXClusterClient) Start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.mu.Unlock()

	c.wg.Add(1)
	go c.run()
}

// Stop closes the connection (sending a polite "bye") and stops reconnecting.
// It blocks until the background goroutine has exited.
func (c *DXClusterClient) Stop() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	c.running = false
	conn := c.conn
	cancel := c.cancel
	c.mu.Unlock()

	if conn != nil {
		byeCtx, byeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = conn.Write(byeCtx, websocket.MessageText, []byte("bye\r\n"))
		byeCancel()
	}
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
}

// Send forwards a command line to the server, appending the CR/LF the
// protocol expects.
func (c *DXClusterClient) Send(line string) error {
	return c.sendRaw(line + "\r\n")
}

// Connected reports whether a live WebSocket connection is currently open.
func (c *DXClusterClient) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

func (c *DXClusterClient) run() {
	defer c.wg.Done()
	backoff := minReconnectDelay
	for {
		if c.ctx.Err() != nil {
			return
		}
		c.status("Connecting…", false)

		connected, err := c.connectOnce()
		if c.ctx.Err() != nil {
			return // stopped by the user
		}

		// A session that actually connected resets the backoff so a normal
		// drop reconnects promptly; repeated dial failures back off.
		if connected {
			backoff = minReconnectDelay
		}

		wait := backoff
		if err != nil {
			c.status(fmt.Sprintf("Disconnected: %s — retrying in %s…", err.Error(), wait), false)
		} else {
			c.status(fmt.Sprintf("Disconnected — retrying in %s…", wait), false)
		}

		select {
		case <-c.ctx.Done():
			return
		case <-time.After(wait):
		}

		if !connected {
			backoff *= 2
			if backoff > maxReconnectDelay {
				backoff = maxReconnectDelay
			}
		}
	}
}

// connectOnce dials, logs in, and pumps messages until the connection ends.
// The bool reports whether the WebSocket connection was actually established
// (as opposed to failing at dial time).
func (c *DXClusterClient) connectOnce() (bool, error) {
	dialCtx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	conn, _, err := websocket.Dial(dialCtx, c.url, nil)
	cancel()
	if err != nil {
		return false, err
	}
	conn.SetReadLimit(wsReadLimit)

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	c.status("Connected", true)

	callsignSent := false
	for {
		typ, data, err := conn.Read(c.ctx)
		if err != nil {
			return true, err
		}
		if typ != websocket.MessageText {
			continue
		}
		text := string(data)

		// Auto-respond to the callsign prompt (matches the web UI logic).
		if !callsignSent && strings.Contains(strings.ToLower(text), "callsign") {
			callsignSent = true
			_ = c.sendRaw(c.callsign + "\r\n")
		}

		if c.onText != nil {
			c.onText(text)
		}
	}
}

func (c *DXClusterClient) sendRaw(s string) error {
	c.mu.Lock()
	conn := c.conn
	ctx := c.ctx
	c.mu.Unlock()
	if conn == nil || ctx == nil {
		return fmt.Errorf("not connected")
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, []byte(s))
}

func (c *DXClusterClient) status(msg string, connected bool) {
	if c.onStatus != nil {
		c.onStatus(msg, connected)
	}
}
