package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// minimalKDL is a valid KDL config that Load can parse without error.
const minimalKDL = `device {
    nickname "test"
}
`

// updatedKDL is a second valid KDL config used to trigger a reload.
const updatedKDL = `device {
    nickname "updated"
}
`

// TestNewWatcher_NonExistentFile verifies that NewWatcher returns an error
// when the target file does not exist.
func TestNewWatcher_NonExistentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.kdl")
	_, err := NewWatcher(path, func(_, _ *Config) {})
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

// TestNewWatcher_ValidFile verifies that NewWatcher succeeds for an existing file.
func TestNewWatcher_ValidFile(t *testing.T) {
	path := writeTemp(t, minimalKDL)
	w, err := NewWatcher(path, func(_, _ *Config) {})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestWatcher_OnChangeCalledOnWrite verifies that modifying the watched file
// triggers the onChange callback with the old and new configs.
func TestWatcher_OnChangeCalledOnWrite(t *testing.T) {
	path := writeTemp(t, minimalKDL)

	type call struct {
		old, new *Config
	}
	var (
		mu    sync.Mutex
		calls []call
	)

	w, err := NewWatcher(path, func(old, new *Config) {
		mu.Lock()
		calls = append(calls, call{old, new})
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := w.Start(ctx); err != nil {
			// Non-fatal in test context; Start returns nil on context cancellation.
			_ = err
		}
	}()

	// Give the watcher goroutine time to enter the select loop.
	time.Sleep(50 * time.Millisecond)

	// Overwrite the file to trigger a Write event.
	if err := os.WriteFile(path, []byte(updatedKDL), 0644); err != nil {
		t.Fatalf("write updated config: %v", err)
	}

	// Wait up to 2 s for onChange to be called.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(calls) == 0 {
		t.Fatal("onChange was not called after file write")
	}

	last := calls[len(calls)-1]
	if last.new == nil {
		t.Fatal("onChange called with nil new config")
	}
	if last.new.Device.Nickname != "updated" {
		t.Errorf("new config Nickname = %q, want \"updated\"", last.new.Device.Nickname)
	}
}

// TestWatcher_StopStopsWatching verifies that Stop causes Start to return
// (because the underlying fsnotify channels are closed).
func TestWatcher_StopStopsWatching(t *testing.T) {
	path := writeTemp(t, minimalKDL)

	w, err := NewWatcher(path, func(_, _ *Config) {})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- w.Start(context.Background())
	}()

	// Give Start time to enter the select loop.
	time.Sleep(50 * time.Millisecond)

	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-done:
		// Start returned as expected after Stop.
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Stop within 2 s")
	}
}
