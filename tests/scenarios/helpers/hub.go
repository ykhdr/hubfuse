package helpers

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// HubBinaryPath is set by tests/scenarios/main_test.go before any test runs.
var HubBinaryPath string

// AgentBinaryPath will be populated by main_test.go as well; declared here so
// the package compiles in Task 4 even though its consumers arrive in Task 5.
var AgentBinaryPath string

type Hub struct {
	Address string // "127.0.0.1:NNNN"
	DataDir string

	port   int
	cmd    *exec.Cmd
	logBuf *LogBuffer
	cancel context.CancelFunc
}

// StartHub launches a hub in a temp data dir and returns once it is listening.
func StartHub(t *testing.T) *Hub {
	t.Helper()
	port := FreePort(t)
	dataDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, HubBinaryPath, "start",
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--data-dir", dataDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logBuf := &LogBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start hub: %v", err)
	}

	h := &Hub{
		Address: fmt.Sprintf("127.0.0.1:%d", port),
		DataDir: dataDir,
		port:    port,
		cmd:     cmd,
		logBuf:  logBuf,
		cancel:  cancel,
	}
	DumpOnFailure(t, "hub", logBuf)
	t.Cleanup(func() { h.Stop(t) })

	WaitForPort(t, port, 5*time.Second)
	return h
}

// Stop signals the hub to exit and waits up to 5s for it to do so.
func (h *Hub) Stop(t *testing.T) {
	t.Helper()
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
	h.cancel()
	h.cmd = nil
}

// Restart stops and starts the hub on the same data dir. Address may change
// if the OS reassigns the port.
func (h *Hub) Restart(t *testing.T) {
	t.Helper()
	h.Stop(t)

	port := FreePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, HubBinaryPath, "start",
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--data-dir", h.DataDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = h.logBuf
	cmd.Stderr = h.logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("restart hub: %v", err)
	}
	h.cmd = cmd
	h.cancel = cancel
	h.port = port
	h.Address = fmt.Sprintf("127.0.0.1:%d", port)
	WaitForPort(t, port, 5*time.Second)
}
