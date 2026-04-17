package helpers

import (
	"bytes"
	"sync"
	"testing"
)

// LogBuffer is a thread-safe bytes.Buffer for capturing subprocess output.
// Subprocess stdout/stderr are typically written from a goroutine, so plain
// bytes.Buffer would race with reads from t.Logf.
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// DumpOnFailure attaches a t.Cleanup that prints the buffer's contents when
// the test has failed, prefixed with the given label. On success it stays silent.
func DumpOnFailure(t *testing.T, label string, buf *LogBuffer) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("--- %s log ---\n%s", label, buf.String())
	})
}
