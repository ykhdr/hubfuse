package helpers

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// FreePort returns a TCP port currently free on localhost. There is a small
// race between returning the port and using it; tests should call it just
// before binding.
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return port
}

// WaitForPort polls until something is listening on 127.0.0.1:port, or the
// deadline elapses.
//
// SOMETHING — read that literally before reusing this. It proves a listener
// exists, never whose it is. Issue #90's reproduction had it dial a squatter
// holding the agent's SSH port and report the agent as up, while the daemon's
// own bind had failed. It is used unqualified for the hub (StartHub), where the
// port is freshly chosen and no impostor can exist; anything started on a port
// a test deliberately contests must also confirm the process's own output, the
// way launchDaemon does.
func WaitForPort(t *testing.T, port int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(end) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after %s", addr, deadline)
}
