//go:build unix

package daemonize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A tiny helper child that waits for SIGTERM and exits 0. The test
// binary's TestMain already branches on HUBFUSE_TEST_ROLE=ready (see
// spawn_unix_test.go) — reuse that infrastructure.

func TestSignalStop_SendsSIGTERM(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "x.pid")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"HUBFUSE_TEST_ROLE=ready",
		"HUBFUSE_TEST_PIDFILE="+pidPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Wait for the child to write its pidfile.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive, _ := CheckRunning(pidPath); alive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := SignalStop(pidPath, "testproc"); err != nil {
		t.Fatalf("SignalStop: %v", err)
	}

	// Wait for the child to exit.
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatalf("child did not exit after SIGTERM")
	}
}

func TestSignalStop_NoPIDFile(t *testing.T) {
	err := SignalStop(filepath.Join(t.TempDir(), "nope.pid"), "x")
	if err == nil {
		t.Fatal("SignalStop with missing pidfile should error")
	}
}

func TestReportStatus_Running(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "live.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatalf("seed pidfile: %v", err)
	}
	if err := ReportStatus(pidPath, "self"); err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
}

func TestReportStatus_NotRunning(t *testing.T) {
	if err := ReportStatus(filepath.Join(t.TempDir(), "nope.pid"), "absent"); err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
}

// TestReadPID_TrimsNewline: WritePIDFile writes "pid\n"; ReadPID
// must round-trip that correctly.
func TestReadPID_TrimsNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.pid")
	if err := os.WriteFile(p, []byte("12345\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pid, err := ReadPID(p)
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if pid != 12345 {
		t.Fatalf("pid=%d; want 12345", pid)
	}
}
