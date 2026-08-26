package agent

// runServices' two exits — issue #90.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunServices_SSHDeathEndsTheDaemon is the runtime half of issue #90. Before
// this, an accept loop that died left the daemon registered, heartbeating and
// listed online with an SSH port it no longer held. runServices must now treat
// that as the end of the daemon: cancel everything, deregister, and return the
// SSH failure as the reason.
func TestRunServices_SSHDeathEndsTheDaemon(t *testing.T) {
	d, _ := buildTestDaemon(t)
	d.sshDied = make(chan error, 1)

	var stopped atomic.Bool
	stopAll := func() { stopped.Store(true) }

	boom := errors.New("listener died")
	d.noteSSHDeath(boom)

	done := make(chan error, 1)
	// A context that is never cancelled: the SSH death alone has to end this,
	// or the test would pass for the wrong reason.
	go func() { done <- d.runServices(context.Background(), stopAll) }()

	select {
	case err := <-done:
		require.Error(t, err, "a dead SSH server must end the daemon with a reason")
		assert.ErrorIs(t, err, boom, "the cause must survive to the exit status")
		assert.Contains(t, err.Error(), "ssh server stopped serving",
			"the message must name what stopped")
	case <-time.After(10 * time.Second):
		t.Fatal("runServices did not return after the SSH server died")
	}

	assert.True(t, stopped.Load(),
		"supervise must be cancelled before Shutdown deregisters, or its next Register "+
			"puts the device back online after the Deregister")
}

// TestRunServices_CtxCancelUnchanged is the negative control: the ordinary
// shutdown path must still be the ordinary shutdown path. A select that
// preferred sshDied, or a Shutdown moved under the new branch, would break
// every clean `hubfuse stop` — and pass a test that only checked the new one.
func TestRunServices_CtxCancelUnchanged(t *testing.T) {
	d, _ := buildTestDaemon(t)
	d.sshDied = make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.runServices(ctx, cancel) }()

	// Let the services come up before asking them to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err, "a cancelled daemon with nothing wrong shuts down cleanly")
	case <-time.After(10 * time.Second):
		t.Fatal("runServices did not return after ctx cancel")
	}
}

// TestRunServices_NilSSHDiedChannelIsInert pins that a Daemon built as a struct
// literal — which every unit fixture is — parks on ctx alone rather than
// spinning on a nil channel or panicking. (#90)
func TestRunServices_NilSSHDiedChannelIsInert(t *testing.T) {
	d, _ := buildTestDaemon(t)
	require.Nil(t, d.sshDied, "fixture precondition: the channel is unset")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.runServices(ctx, cancel) }()

	select {
	case err := <-done:
		t.Fatalf("runServices returned before it was asked to: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("runServices did not return after ctx cancel")
	}
}
