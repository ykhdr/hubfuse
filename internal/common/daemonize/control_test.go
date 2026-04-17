//go:build unix

package daemonize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, cmd.Start(), "start helper")
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Wait for the child to write its pidfile.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive, _ := CheckRunning(pidPath); alive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	require.NoError(t, SignalStop(pidPath, "testproc"))

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
	assert.Error(t, err, "SignalStop with missing pidfile should error")
}

func TestReportStatus_Running(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "live.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644), "seed pidfile")
	assert.NoError(t, ReportStatus(pidPath, "self"))
}

func TestReportStatus_NotRunning(t *testing.T) {
	assert.NoError(t, ReportStatus(filepath.Join(t.TempDir(), "nope.pid"), "absent"))
}

// TestReadPID_TrimsNewline: WritePIDFile writes "pid\n"; ReadPID
// must round-trip that correctly.
func TestReadPID_TrimsNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.pid")
	require.NoError(t, os.WriteFile(p, []byte("12345\n"), 0o644), "seed")
	pid, err := ReadPID(p)
	require.NoError(t, err)
	assert.Equal(t, 12345, pid)
}
