//go:build unix

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common/daemonize"
)

// TestSignalStop_ImmediateRestart verifies that after SignalStop returns the
// pidfile is gone and a second "start" (CheckRunning) does not see "already
// running". This catches the original bug where SignalStop returned before the
// process had actually exited, causing the next start to fail.
func TestSignalStop_ImmediateRestart(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "hub.pid")

	// Spawn a real process that honors SIGTERM and exits cleanly. Using
	// Setsid=true makes it a session leader, and the background Wait reaps
	// the zombie so that syscall.Kill(pid,0) returns ESRCH as soon as the
	// process exits — the same topology SignalStop sees in production.
	cmd := exec.Command("/bin/sh", "-c", "sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, cmd.Start())
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Write the pidfile manually so SignalStop can find it.
	require.NoError(t, os.WriteFile(pidPath,
		[]byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644))

	// Wait for the pid to be live.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive, _ := daemonize.CheckRunning(pidPath); alive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, alive, err := daemonize.CheckRunning(pidPath)
	require.NoError(t, err)
	require.True(t, alive, "child did not start within 3s")

	// Stop must wait for the process to die before returning.
	require.NoError(t, daemonize.SignalStop(pidPath, "hub"))

	// Immediately after SignalStop returns, CheckRunning must see it gone —
	// this is the heart of the regression test.
	_, alive, err = daemonize.CheckRunning(pidPath)
	require.NoError(t, err)
	assert.False(t, alive, "process still alive immediately after SignalStop returned (restart would fail with 'already running')")
}
