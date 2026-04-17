package daemonize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsChild_EnvUnset(t *testing.T) {
	t.Setenv(EnvDaemonized, "")
	assert.False(t, IsChild(), "IsChild() = true with env unset; want false")
}

func TestIsChild_EnvSet(t *testing.T) {
	t.Setenv(EnvDaemonized, "1")
	assert.True(t, IsChild(), "IsChild() = false with env set; want true")
}

func TestIsChild_EnvZero(t *testing.T) {
	t.Setenv(EnvDaemonized, "0")
	assert.False(t, IsChild(), `IsChild() = true with env=0; want false (only "1" means child)`)
}

func TestWritePIDFile_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.pid")

	require.NoError(t, WritePIDFile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err, "read pidfile")
	got, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err, "parse pidfile contents %q", data)
	assert.Equal(t, os.Getpid(), got, "pidfile contents")
}

func TestWritePIDFile_AtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.pid")

	require.NoError(t, os.WriteFile(path, []byte("99999999\nleftover-junk\n"), 0o644), "seed pidfile")

	require.NoError(t, WritePIDFile(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err, "read pidfile")
	assert.NotContains(t, string(data), "leftover-junk", "pidfile still has old contents")
}

func TestCheckRunning_NoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.pid")

	pid, alive, err := CheckRunning(path)
	require.NoError(t, err)
	assert.False(t, alive, "alive=true with no pidfile; want false")
	assert.Equal(t, 0, pid)
}

func TestCheckRunning_LivePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.pid")
	require.NoError(t, WritePIDFile(path), "seed pidfile")

	pid, alive, err := CheckRunning(path)
	require.NoError(t, err)
	assert.True(t, alive, "alive=false for own pid; want true")
	assert.Equal(t, os.Getpid(), pid)
}

func TestCheckRunning_StalePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.pid")

	// Find a PID that's definitely dead: spawn a short-lived child, wait
	// for it to exit, then write its PID.
	cmd := newDeadPIDCmd(t)
	stalePID := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		// exit error is expected; we just need the process to be gone.
	}
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(stalePID)+"\n"), 0o644), "seed stale pidfile")

	pid, alive, err := CheckRunning(path)
	require.NoError(t, err)
	assert.False(t, alive, "alive=true for dead pid %d; want false", stalePID)
	assert.Equal(t, 0, pid, "want 0 (stale)")
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "stale pidfile still exists: %v", err)
}

// newDeadPIDCmd starts /bin/sh that exits immediately. Caller must Wait()
// to reap it; the returned cmd's Process.Pid is then safe to use as a
// pid that no longer refers to a live process (in practice the kernel
// reuses PIDs only after a long cycle).
func newDeadPIDCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	require.NoError(t, cmd.Start(), "start helper")
	return cmd
}
