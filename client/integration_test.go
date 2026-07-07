package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd exercises the full pipeline without the GUI: fetch the instance
// directory, connect to a real DX cluster instance over its WebSocket terminal,
// re-serve the stream via the telnet listener, and confirm a plain TCP client
// receives live spot text. Requires network access; skipped with -short.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test skipped in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	insts, err := FetchDXClusterInstances(ctx)
	if err != nil {
		t.Fatalf("fetch instances: %v", err)
	}
	if len(insts) == 0 {
		t.Skip("no dxcluster instances currently online")
	}
	t.Logf("found %d dxcluster instance(s)", len(insts))

	// Pin the verification to m9psy, a known-stable instance.
	const stableHost = "m9psy"
	var target *Instance
	for i := range insts {
		if strings.Contains(strings.ToLower(insts[i].Host), stableHost) {
			target = &insts[i]
			break
		}
	}
	if target == nil {
		t.Skipf("stable instance %q not present in directory", stableHost)
	}

	t.Logf("verifying against %q -> %s", target.Name, target.TerminalWSURL())
	if !bridgeWorks(t, *target) {
		t.Fatalf("stable instance %q produced no telnet output within the timeout", target.Name)
	}
	t.Logf("telnet bridge verified against %q ✓", target.Name)
}

// bridgeWorks connects to one instance, re-serves it via the telnet listener,
// and returns true if a plain TCP client receives recognisable cluster output.
func bridgeWorks(t *testing.T, inst Instance) bool {
	t.Helper()

	port := freePort(t)
	var client *DXClusterClient
	listener := NewTelnetListener(port,
		func(line string) { _ = client.Send(line) },
		func(int) {},
	)
	if err := listener.Start(); err != nil {
		t.Fatalf("telnet listen: %v", err)
	}
	defer listener.Stop()

	client = NewDXClusterClient(inst.TerminalWSURL(), "N0CALL",
		func(text string) { listener.Broadcast(text) },
		func(msg string, _ bool) {},
	)
	client.Start()
	defer client.Stop()

	time.Sleep(1 * time.Second) // let the WS connect first
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatalf("telnet dial: %v", err)
	}
	defer conn.Close()

	telnetGot := make(chan string, 1)
	go func() {
		r := bufio.NewReader(conn)
		var b strings.Builder
		for {
			line, err := r.ReadString('\n')
			b.WriteString(line)
			if strings.Contains(b.String(), "Welcome") || strings.Contains(b.String(), "DX de") {
				telnetGot <- b.String()
				return
			}
			if err != nil {
				telnetGot <- b.String()
				return
			}
		}
	}()

	select {
	case out := <-telnetGot:
		if strings.Contains(out, "Welcome") || strings.Contains(out, "DX de") {
			t.Logf("received %d bytes of cluster output", len(out))
			return true
		}
		return false
	case <-time.After(8 * time.Second):
		return false
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
