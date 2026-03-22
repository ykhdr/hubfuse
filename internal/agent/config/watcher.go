package config

import (
	"context"
	"fmt"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// Watcher monitors a config file for changes and calls onChange whenever the
// file is modified or recreated.
type Watcher struct {
	path     string
	onChange func(old, new *Config)
	fw       *fsnotify.Watcher
	mu       sync.Mutex
	current  *Config
}

// NewWatcher creates a Watcher that monitors the config file at path.
// onChange is called with the old and new configs whenever the file changes.
// The current config is loaded immediately so that a baseline is available.
func NewWatcher(path string, onChange func(old, new *Config)) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	if err := fw.Add(path); err != nil {
		_ = fw.Close()
		return nil, fmt.Errorf("watch config file %q: %w", path, err)
	}

	// Load the initial config; errors here are non-fatal — the watcher will
	// try again on the next file event.
	initial, _ := Load(path)

	return &Watcher{
		path:     path,
		onChange: onChange,
		fw:       fw,
		current:  initial,
	}, nil
}

// Start begins watching for file changes. It blocks until ctx is cancelled.
// Callers should run Start in a goroutine. Returns any error that caused the
// watcher to stop (other than context cancellation).
func (w *Watcher) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil

		case event, ok := <-w.fw.Events:
			if !ok {
				return nil
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				w.reload()
			}

		case err, ok := <-w.fw.Errors:
			if !ok {
				return nil
			}
			// Non-fatal: log-worthy in production but we have no logger here.
			_ = err
		}
	}
}

// Stop releases the underlying fsnotify watcher.
func (w *Watcher) Stop() error {
	return w.fw.Close()
}

// reload loads the config from disk and, if successful, invokes onChange.
func (w *Watcher) reload() {
	newCfg, err := Load(w.path)
	if err != nil {
		return
	}

	w.mu.Lock()
	old := w.current
	w.current = newCfg
	w.mu.Unlock()

	w.onChange(old, newCfg)
}
