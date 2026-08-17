package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestWaitForShutdown_NoSignalDoesNotBlock pins requirement 3: when h.Start
// fails on its own (e.g. "listen: address already in use") and no
// SIGINT/SIGTERM ever arrived, awaitTermination is still parked on sigCh and
// will never close done — so RunE must return the start error immediately
// rather than wait on a channel nothing will ever signal. If waitForShutdown
// regressed to always reading done regardless of signaled, this test would
// hang until the deadline and fail. (#75)
func TestWaitForShutdown_NoSignalDoesNotBlock(t *testing.T) {
	var signaled atomic.Bool // zero value: no signal observed

	// done deliberately never closes: nothing must ever try to read it.
	done := make(chan struct{})

	finished := make(chan struct{})
	go func() {
		waitForShutdown(done, &signaled)
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown blocked despite signaled == false")
	}
}

// TestWaitForShutdown_SignaledWaitsForDone pins the other half of the same
// guard: once a signal has been observed, RunE must actually wait for
// awaitTermination's shutdown to settle before returning, rather than racing
// ahead of it. A stub that ignored signaled and always returned immediately
// would pass the sibling test above but fails this one, so together the two
// tests pin the branch rather than just the function's existence. (#75)
func TestWaitForShutdown_SignaledWaitsForDone(t *testing.T) {
	var signaled atomic.Bool
	signaled.Store(true)

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		waitForShutdown(done, &signaled)
		close(finished)
	}()

	select {
	case <-finished:
		t.Fatal("waitForShutdown returned before done was closed")
	case <-time.After(50 * time.Millisecond):
	}

	close(done)

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown did not return after done closed")
	}
}

// TestAwaitTermination_SettledStopClosesDoneWithoutTouchingPIDFile exercises
// the one branch of awaitTermination that is honestly testable without
// spawning a process or hitting os.Exit: a single signal, a stop func that
// settles. It pins three things at once: the signal is what flips signaled
// (so waitForShutdown above knows to wait), cancel is invoked on receipt (so
// background work is told to unwind before stop's own budget starts), and
// done only closes on the settled path — critically, the PID file is left
// alone here, because on the settled path removing it is RunE's deferred
// job, not awaitTermination's; removing it twice would violate the "exactly
// once" rule. (#75)
func TestAwaitTermination_SettledStopClosesDoneWithoutTouchingPIDFile(t *testing.T) {
	sigCh := make(chan os.Signal, 1)

	var cancelled atomic.Bool
	cancel := func() { cancelled.Store(true) }

	var stopCalls atomic.Int32
	stop := func() (bool, error) {
		stopCalls.Add(1)
		return true, nil
	}

	pidPath := filepath.Join(t.TempDir(), "hub.pid")
	if err := os.WriteFile(pidPath, []byte("1234"), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	var signaled atomic.Bool
	done := make(chan struct{})

	go awaitTermination(sigCh, cancel, stop, pidPath, &signaled, done)

	sigCh <- syscall.SIGTERM

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("awaitTermination did not close done after a settled stop")
	}

	if !signaled.Load() {
		t.Error("signaled was not set on receiving the signal")
	}
	if !cancelled.Load() {
		t.Error("cancel was not called on receiving the signal")
	}
	if got := stopCalls.Load(); got != 1 {
		t.Errorf("stop called %d times, want exactly 1", got)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file was touched on the settled path (want RunE's own defer to own removal): %v", err)
	}
}
