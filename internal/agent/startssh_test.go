package agent

// startSSH and the path a dead SSH server takes to the daemon — issue #90.

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartSSH_ReportsBindFailure is issue #90 at the daemon boundary: startSSH
// returned nil unconditionally, so Run walked straight into registerAndSubscribe
// and the hub advertised a port this process had failed to take.
//
// Run itself is not driven here — it opens with d.connector.Connect, which
// needs live TLS material and a reachable hub, and faking that would test the
// fake. The end-to-end ordering claim ("Register is never reached") is made by
// the scenario test instead, where the hub is real and can be asked whether it
// ever saw the device.
func TestStartSSH_ReportsBindFailure(t *testing.T) {
	d, dir := buildTestDaemon(t)

	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "squat a port")
	defer squatter.Close()
	port := squatter.Addr().(*net.TCPAddr).Port

	srv, err := NewSSHServer(port, filepath.Join(dir, "keys", privateKeyFile), discardLogger())
	require.NoError(t, err, "NewSSHServer()")
	d.sshServer = srv

	err = d.startSSH(context.Background())
	require.Error(t, err, "startSSH() must report a port it could not bind")
	assert.Contains(t, err.Error(), "address already in use",
		"the operator-facing reason must survive the wrapping")
}

// TestStartSSH_ListenerDeathReachesTheDaemon walks the whole runtime chain the
// issue asks for: "the bind can also fail after a successful start (the
// listener is closed…)". The listener is closed WITHOUT going through Stop, so
// nothing marked it deliberate — exactly how a real accident presents — and the
// accept loop's error has to travel out of its goroutine and reach the daemon
// instead of being logged and forgotten.
func TestStartSSH_ListenerDeathReachesTheDaemon(t *testing.T) {
	d, dir := buildTestDaemon(t)
	d.sshDied = make(chan error, 1)

	srv, err := NewSSHServer(0, filepath.Join(dir, "keys", privateKeyFile), discardLogger())
	require.NoError(t, err, "NewSSHServer()")
	d.sshServer = srv

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, d.startSSH(ctx), "startSSH() on a free port")

	srv.mu.RLock()
	ln := srv.listener
	srv.mu.RUnlock()
	require.NotNil(t, ln, "startSSH must have bound a listener")
	require.NoError(t, ln.Close(), "close the listener out from under the accept loop")

	select {
	case err := <-d.sshDied:
		assert.Error(t, err, "a listener that died must be reported, not logged and dropped")
	case <-time.After(3 * time.Second):
		t.Fatal("the death of the SSH server never reached the daemon")
	}
}

// TestNoteSSHDeath_NeverBlocks pins the two properties runServices depends on:
// a second death is discarded rather than parking its goroutine, and a nil
// channel is inert. The nil case is not hypothetical — buildTestDaemon builds
// Daemon as a struct literal, so every daemon in the unit suite has one. (#90)
func TestNoteSSHDeath_NeverBlocks(t *testing.T) {
	t.Run("second death is discarded", func(t *testing.T) {
		d := &Daemon{sshDied: make(chan error, 1)}
		first := assert.AnError
		d.noteSSHDeath(first)

		done := make(chan struct{})
		go func() { d.noteSSHDeath(assert.AnError); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("noteSSHDeath blocked on a full channel")
		}

		assert.Equal(t, first, <-d.sshDied, "the first death is the one that ends the daemon")
	})

	t.Run("nil channel is inert", func(t *testing.T) {
		d := &Daemon{}
		done := make(chan struct{})
		go func() { d.noteSSHDeath(assert.AnError); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("noteSSHDeath blocked on a nil channel")
		}
	})
}
