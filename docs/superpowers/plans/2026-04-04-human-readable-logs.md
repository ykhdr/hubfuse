# Human-Readable Logs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace raw JSON console output with human-readable colored logs, add opt-in debug log file for both hub and agent.

**Architecture:** Custom `slog.Handler` (`ConsoleHandler`) for human-readable stderr output. `MultiHandler` fans out to both console and JSON file handlers when file logging is enabled. `SetupLogger` refactored to accept `LoggerOptions` struct and build the appropriate handler chain.

**Tech Stack:** Go `log/slog`, `github.com/mattn/go-isatty` (already indirect dep)

---

## File Structure

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/common/console_handler.go` | Create | `ConsoleHandler` — human-readable `slog.Handler` |
| `internal/common/console_handler_test.go` | Create | Unit tests for ConsoleHandler |
| `internal/common/multi_handler.go` | Create | `MultiHandler` — fan-out `slog.Handler` |
| `internal/common/multi_handler_test.go` | Create | Unit tests for MultiHandler |
| `internal/common/logging.go` | Modify | Refactor `SetupLogger` to use `LoggerOptions` |
| `internal/common/logging_test.go` | Modify | Update tests for new `SetupLogger` signature |
| `internal/hub/hub.go` | Modify | Update `HubConfig` and `NewHub()` |
| `cmd/hubfuse-hub/main.go` | Modify | Replace old CLI flags |
| `cmd/hubfuse-agent/main.go` | Modify | Add logging flags to `start`, update other commands |
| `tests/integration/integration_test.go` | Modify | Update logger creation |
| `tests/integration/reconnect_test.go` | Modify | Update logger creation |

---

### Task 1: ConsoleHandler

**Files:**
- Create: `internal/common/console_handler.go`
- Create: `internal/common/console_handler_test.go`

- [ ] **Step 1: Write failing tests for ConsoleHandler**

Create `internal/common/console_handler_test.go`:

```go
package common

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestConsoleHandler_Format(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(h)

	fixedTime := time.Date(2026, 4, 4, 21, 25, 24, 0, time.UTC)
	record := slog.NewRecord(fixedTime, slog.LevelInfo, "connecting to hub", 0)
	record.AddAttrs(slog.String("addr", "192.168.31.158:9090"))

	if err := h.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "21:25:24") {
		t.Errorf("missing timestamp in %q", got)
	}
	if !strings.Contains(got, "[INFO]") {
		t.Errorf("missing [INFO] in %q", got)
	}
	if !strings.Contains(got, "connecting to hub") {
		t.Errorf("missing message in %q", got)
	}
	if !strings.Contains(got, "addr=192.168.31.158:9090") {
		t.Errorf("missing attr in %q", got)
	}

	// Writing to bytes.Buffer (not a terminal) — no ANSI escape codes.
	if strings.Contains(got, "\033[") {
		t.Errorf("unexpected ANSI codes in non-terminal output: %q", got)
	}
}

func TestConsoleHandler_Levels(t *testing.T) {
	tests := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "[DEBUG]"},
		{slog.LevelInfo, "[INFO]"},
		{slog.LevelWarn, "[WARN]"},
		{slog.LevelError, "[ERROR]"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			var buf bytes.Buffer
			h := NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})

			record := slog.NewRecord(time.Now(), tc.level, "test", 0)
			if err := h.Handle(context.Background(), record); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("output %q does not contain %q", buf.String(), tc.want)
			}
		})
	}
}

func TestConsoleHandler_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})

	// Info should be filtered out.
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Info should not be enabled at Warn level")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("Warn should be enabled at Warn level")
	}
}

func TestConsoleHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h2 := h.WithAttrs([]slog.Attr{slog.String("component", "hub")})

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "starting", 0)
	if err := h2.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(buf.String(), "component=hub") {
		t.Errorf("output %q missing prebuilt attr", buf.String())
	}
}

func TestConsoleHandler_WithGroup(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h2 := h.WithGroup("server")

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "listening", 0)
	record.AddAttrs(slog.String("port", "9090"))
	if err := h2.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(buf.String(), "server.port=9090") {
		t.Errorf("output %q missing grouped attr", buf.String())
	}
}

func TestConsoleHandler_ViaLogger(t *testing.T) {
	var buf bytes.Buffer
	h := NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(h)

	logger.Info("test message", "key", "value")

	got := buf.String()
	if !strings.Contains(got, "[INFO]") {
		t.Errorf("output %q missing [INFO]", got)
	}
	if !strings.Contains(got, "test message") {
		t.Errorf("output %q missing message", got)
	}
	if !strings.Contains(got, "key=value") {
		t.Errorf("output %q missing attr", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/common/ -run TestConsoleHandler -v`
Expected: compilation error — `NewConsoleHandler` not defined.

- [ ] **Step 3: Implement ConsoleHandler**

Create `internal/common/console_handler.go`:

```go
package common

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

// ANSI color codes.
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

// ConsoleHandler is an slog.Handler that writes human-readable log lines.
//
// Format: 21:25:24 [INFO]  message key=value key2=value2
type ConsoleHandler struct {
	opts      slog.HandlerOptions
	w         io.Writer
	mu        *sync.Mutex
	useColor  bool
	preAttrs  []slog.Attr
	groupName string
}

// NewConsoleHandler creates a ConsoleHandler that writes to w.
// Colors are enabled only when w is a terminal.
func NewConsoleHandler(w io.Writer, opts *slog.HandlerOptions) *ConsoleHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}

	useColor := false
	if f, ok := w.(*os.File); ok {
		fd := f.Fd()
		useColor = isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
		if runtime.GOOS == "windows" {
			useColor = useColor || isatty.IsTerminal(fd)
		}
	}

	return &ConsoleHandler{
		opts:     *opts,
		w:        w,
		mu:       &sync.Mutex{},
		useColor: useColor,
	}
}

func (h *ConsoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.opts.Level != nil {
		minLevel = h.opts.Level.Level()
	}
	return level >= minLevel
}

func (h *ConsoleHandler) Handle(_ context.Context, r slog.Record) error {
	var buf []byte

	// Timestamp.
	buf = append(buf, r.Time.Format(time.TimeOnly)...)
	buf = append(buf, ' ')

	// Level.
	levelStr, color := h.levelString(r.Level)
	if h.useColor && color != "" {
		buf = append(buf, color...)
	}
	buf = append(buf, levelStr...)
	if h.useColor && color != "" {
		buf = append(buf, colorReset...)
	}
	buf = append(buf, ' ')

	// Message.
	buf = append(buf, r.Message...)

	// Pre-built attrs (already have group prefix from WithAttrs).
	for _, a := range h.preAttrs {
		buf = h.appendAttr(buf, a, false)
	}

	// Record attrs (need group prefix if handler has a group).
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, a, true)
		return true
	})

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf)
	return err
}

func (h *ConsoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.preAttrs), len(h.preAttrs)+len(attrs))
	copy(newAttrs, h.preAttrs)

	for _, a := range attrs {
		if h.groupName != "" {
			a.Key = h.groupName + "." + a.Key
		}
		newAttrs = append(newAttrs, a)
	}

	return &ConsoleHandler{
		opts:      h.opts,
		w:         h.w,
		mu:        h.mu,
		useColor:  h.useColor,
		preAttrs:  newAttrs,
		groupName: h.groupName,
	}
}

func (h *ConsoleHandler) WithGroup(name string) slog.Handler {
	newGroup := name
	if h.groupName != "" {
		newGroup = h.groupName + "." + name
	}

	newAttrs := make([]slog.Attr, len(h.preAttrs))
	copy(newAttrs, h.preAttrs)

	return &ConsoleHandler{
		opts:      h.opts,
		w:         h.w,
		mu:        h.mu,
		useColor:  h.useColor,
		preAttrs:  newAttrs,
		groupName: newGroup,
	}
}

func (h *ConsoleHandler) levelString(level slog.Level) (string, string) {
	switch {
	case level < slog.LevelInfo:
		return "[DEBUG]", colorCyan
	case level < slog.LevelWarn:
		return "[INFO] ", colorGreen
	case level < slog.LevelError:
		return "[WARN] ", colorYellow
	default:
		return "[ERROR]", colorRed
	}
}

func (h *ConsoleHandler) appendAttr(buf []byte, a slog.Attr, addGroup bool) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}

	buf = append(buf, ' ')
	if addGroup && h.groupName != "" {
		buf = append(buf, h.groupName...)
		buf = append(buf, '.')
	}
	buf = append(buf, a.Key...)
	buf = append(buf, '=')
	buf = append(buf, fmt.Sprintf("%v", a.Value.Any())...)
	return buf
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/common/ -run TestConsoleHandler -v`
Expected: all 6 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/common/console_handler.go internal/common/console_handler_test.go
git commit -m "feat: add ConsoleHandler for human-readable slog output"
```

---

### Task 2: MultiHandler

**Files:**
- Create: `internal/common/multi_handler.go`
- Create: `internal/common/multi_handler_test.go`

- [ ] **Step 1: Write failing tests for MultiHandler**

Create `internal/common/multi_handler_test.go`:

```go
package common

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestMultiHandler_WritesToAll(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelInfo})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelInfo})

	multi := NewMultiHandler(h1, h2)
	logger := slog.New(multi)

	logger.Info("hello", "key", "value")

	if buf1.Len() == 0 {
		t.Error("handler 1 received no output")
	}
	if buf2.Len() == 0 {
		t.Error("handler 2 received no output")
	}
}

func TestMultiHandler_EnabledAny(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelError})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})

	multi := NewMultiHandler(h1, h2)

	// Debug is enabled because h2 accepts it, even though h1 doesn't.
	if !multi.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected Debug to be enabled (h2 accepts it)")
	}
}

func TestMultiHandler_LevelFiltering(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelError})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})

	multi := NewMultiHandler(h1, h2)
	logger := slog.New(multi)

	logger.Debug("debug msg")

	// h1 at Error level should not have output.
	if buf1.Len() != 0 {
		t.Errorf("handler 1 (Error level) should have no output, got %q", buf1.String())
	}
	// h2 at Debug level should have output.
	if buf2.Len() == 0 {
		t.Error("handler 2 (Debug level) should have output")
	}
}

func TestMultiHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})

	multi := NewMultiHandler(h)
	multi2 := multi.WithAttrs([]slog.Attr{slog.String("comp", "test")})

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	if err := multi2.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !bytes.Contains(buf.Bytes(), []byte("comp=test")) {
		t.Errorf("output %q missing attr", buf.String())
	}
}

func TestMultiHandler_WithGroup(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})

	multi := NewMultiHandler(h)
	multi2 := multi.WithGroup("grp")

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	record.AddAttrs(slog.String("key", "val"))
	if err := multi2.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !bytes.Contains(buf.Bytes(), []byte("grp.key=val")) {
		t.Errorf("output %q missing grouped attr", buf.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/common/ -run TestMultiHandler -v`
Expected: compilation error — `NewMultiHandler` not defined.

- [ ] **Step 3: Implement MultiHandler**

Create `internal/common/multi_handler.go`:

```go
package common

import (
	"context"
	"log/slog"
)

// MultiHandler fans out log records to multiple slog.Handlers.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler creates a handler that writes to all given handlers.
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: handlers}
}

func (m *MultiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: handlers}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/common/ -run TestMultiHandler -v`
Expected: all 5 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/common/multi_handler.go internal/common/multi_handler_test.go
git commit -m "feat: add MultiHandler for fan-out slog output"
```

---

### Task 3: Refactor SetupLogger

**Files:**
- Modify: `internal/common/logging.go`
- Modify: `internal/common/logging_test.go`

- [ ] **Step 1: Update tests for new SetupLogger signature**

Replace the entire contents of `internal/common/logging_test.go`:

```go
package common

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupLogger_DefaultConsole(t *testing.T) {
	logger, err := SetupLogger(LoggerOptions{})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}
	if logger == nil {
		t.Fatal("SetupLogger returned nil")
	}
	// Default level is Info.
	if !logger.Enabled(context.TODO(), slog.LevelInfo) {
		t.Error("Info should be enabled by default")
	}
	if logger.Enabled(context.TODO(), slog.LevelDebug) {
		t.Error("Debug should not be enabled by default")
	}
}

func TestSetupLogger_Verbose(t *testing.T) {
	logger, err := SetupLogger(LoggerOptions{Verbose: true})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}
	if !logger.Enabled(context.TODO(), slog.LevelDebug) {
		t.Error("Debug should be enabled in verbose mode")
	}
}

func TestSetupLogger_WithLogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	logger, err := SetupLogger(LoggerOptions{
		LogFile:   logPath,
		FileLevel: slog.LevelDebug,
	})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}

	logger.Info("test message", "key", "value")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("log file is empty")
	}

	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("not valid JSON: %v\nraw: %s", err, raw)
	}
	if entry["msg"] != "test message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "test message")
	}
	if entry["key"] != "value" {
		t.Errorf("key = %v, want %q", entry["key"], "value")
	}
}

func TestSetupLogger_FileLevelFiltering(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "warn.log")

	logger, err := SetupLogger(LoggerOptions{
		LogFile:   logPath,
		FileLevel: slog.LevelWarn,
	})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}

	logger.Info("should not appear in file")
	logger.Warn("should appear in file")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	content := string(raw)
	if strings.Contains(content, "should not appear") {
		t.Error("info message should not be in warn-level file")
	}

	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if entry["msg"] != "should appear in file" {
		t.Errorf("unexpected msg: %v", entry["msg"])
	}
}

func TestSetupLogger_InvalidFilePath(t *testing.T) {
	_, err := SetupLogger(LoggerOptions{
		LogFile: "/nonexistent/dir/app.log",
	})
	if err == nil {
		t.Fatal("expected error for unwritable path")
	}
}

func TestSetupLogger_CreatesLogDir(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "subdir", "nested", "app.log")

	_, err := SetupLogger(LoggerOptions{LogFile: logPath})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}

	if _, err := os.Stat(filepath.Dir(logPath)); os.IsNotExist(err) {
		t.Error("expected log directory to be created")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/common/ -run TestSetupLogger -v`
Expected: compilation error — `LoggerOptions` not defined, `SetupLogger` signature mismatch.

- [ ] **Step 3: Implement refactored SetupLogger**

Replace the entire contents of `internal/common/logging.go`:

```go
package common

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// LoggerOptions configures the logger created by SetupLogger.
type LoggerOptions struct {
	// ConsoleLevel is the minimum level for console output (default: Info).
	ConsoleLevel slog.Level

	// LogFile is the path to a JSON log file. Empty means no file logging.
	LogFile string

	// FileLevel is the minimum level for file output (default: Debug).
	FileLevel slog.Level

	// Verbose overrides ConsoleLevel to Debug.
	Verbose bool
}

// SetupLogger creates a logger with a human-readable console handler on stderr.
// If LogFile is set, it also writes structured JSON to that file via a
// MultiHandler.
func SetupLogger(opts LoggerOptions) (*slog.Logger, error) {
	consoleLevel := opts.ConsoleLevel
	if opts.Verbose {
		consoleLevel = slog.LevelDebug
	}

	consoleHandler := NewConsoleHandler(os.Stderr, &slog.HandlerOptions{
		Level: consoleLevel,
	})

	if opts.LogFile == "" {
		return slog.New(consoleHandler), nil
	}

	// Create parent directories for the log file.
	if err := os.MkdirAll(filepath.Dir(opts.LogFile), 0755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	f, err := os.OpenFile(opts.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", opts.LogFile, err)
	}

	fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level: opts.FileLevel,
	})

	return slog.New(NewMultiHandler(consoleHandler, fileHandler)), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/common/ -run TestSetupLogger -v`
Expected: all 6 tests PASS.

- [ ] **Step 5: Run all common tests**

Run: `go test ./internal/common/ -v`
Expected: all tests PASS (including ConsoleHandler and MultiHandler tests).

- [ ] **Step 6: Commit**

```bash
git add internal/common/logging.go internal/common/logging_test.go
git commit -m "refactor: SetupLogger now uses LoggerOptions with console+file output"
```

---

### Task 4: Update Hub CLI flags and HubConfig

**Files:**
- Modify: `cmd/hubfuse-hub/main.go:32-86`
- Modify: `internal/hub/hub.go:24-50`

- [ ] **Step 1: Update HubConfig struct**

In `internal/hub/hub.go`, replace the `HubConfig` struct (lines 25-31):

Old:
```go
type HubConfig struct {
	ListenAddr string   // e.g. ":9090"
	DataDir    string   // e.g. "~/.hubfuse-hub"
	LogLevel   string   // "debug", "info", "warn", "error"
	LogOutput  string   // "stderr" or file path
	ExtraSANs  []string // additional SANs for the server TLS certificate
}
```

New:
```go
type HubConfig struct {
	ListenAddr string   // e.g. ":9090"
	DataDir    string   // e.g. "~/.hubfuse-hub"
	LogFile    string   // path to JSON log file ("" = no file logging)
	LogLevel   string   // file log level: "debug", "info", "warn", "error" (default: "debug")
	Verbose    bool     // show debug logs in console
	ExtraSANs  []string // additional SANs for the server TLS certificate
}
```

- [ ] **Step 2: Add ParseLogLevel helper**

In `internal/common/logging.go`, add after the `SetupLogger` function:

```go
// ParseLogLevel converts a level name to slog.Level.
// Returns slog.LevelDebug for unrecognised values.
func ParseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelDebug
	}
}
```

- [ ] **Step 3: Update NewHub to use new SetupLogger**

In `internal/hub/hub.go`, replace the `SetupLogger` call in `NewHub()` (line 47):

Old:
```go
	logger, err := common.SetupLogger(config.LogLevel, config.LogOutput)
```

New:
```go
	fileLevel := common.ParseLogLevel(config.LogLevel)
	logger, err := common.SetupLogger(common.LoggerOptions{
		LogFile:   config.LogFile,
		FileLevel: fileLevel,
		Verbose:   config.Verbose,
	})
```

- [ ] **Step 4: Update hub CLI flags**

In `cmd/hubfuse-hub/main.go`, replace the `startCmd` function (lines 32-86):

Old:
```go
func startCmd() *cobra.Command {
	var (
		listen    string
		dataDir   string
		logLevel  string
		logOutput string
		extraSANs []string
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the hub server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := hub.HubConfig{
				ListenAddr: listen,
				DataDir:    dataDir,
				LogLevel:   logLevel,
				LogOutput:  logOutput,
				ExtraSANs:  extraSANs,
			}
```

New:
```go
func startCmd() *cobra.Command {
	var (
		listen    string
		dataDir   string
		logFile   string
		logLevel  string
		verbose   bool
		extraSANs []string
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the hub server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := hub.HubConfig{
				ListenAddr: listen,
				DataDir:    dataDir,
				LogFile:    logFile,
				LogLevel:   logLevel,
				Verbose:    verbose,
				ExtraSANs:  extraSANs,
			}
```

And replace the flags at the bottom (lines 79-84):

Old:
```go
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&logOutput, "log-output", "stderr", "log output (stderr or file path)")
```

New:
```go
	cmd.Flags().StringVar(&logFile, "log-file", "", "write JSON logs to file (disabled by default)")
	cmd.Flags().StringVar(&logLevel, "log-level", "debug", "log file level (debug, info, warn, error)")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show debug logs in console")
```

- [ ] **Step 5: Verify hub builds**

Run: `go build ./cmd/hubfuse-hub/`
Expected: builds successfully with no errors.

- [ ] **Step 6: Run hub tests**

Run: `go test ./internal/hub/ -v`
Expected: all tests PASS. Hub tests create loggers directly (not via `SetupLogger`), so they should be unaffected.

- [ ] **Step 7: Commit**

```bash
git add internal/hub/hub.go internal/common/logging.go cmd/hubfuse-hub/main.go
git commit -m "feat: update hub to use human-readable console + optional file logging"
```

---

### Task 5: Update Agent CLI

**Files:**
- Modify: `cmd/hubfuse-agent/main.go:156-199` (startCmd) and lines 97, 281, 307, 347 (other commands)

- [ ] **Step 1: Add logging flags to agent startCmd**

In `cmd/hubfuse-agent/main.go`, replace `startCmd()` function (lines 156-199):

Old:
```go
func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the agent daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)

			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			daemon, err := agent.NewDaemon(cfgPath, logger)
```

New:
```go
func startCmd() *cobra.Command {
	var (
		logFile  string
		logLevel string
		verbose  bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the agent daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)

			logger, err := common.SetupLogger(common.LoggerOptions{
				LogFile:   logFile,
				FileLevel: common.ParseLogLevel(logLevel),
				Verbose:   verbose,
			})
			if err != nil {
				return fmt.Errorf("setup logger: %w", err)
			}

			daemon, err := agent.NewDaemon(cfgPath, logger)
```

And at the end of the function, before the closing brace of `startCmd()`, add flags:

```go
	cmd.Flags().StringVar(&logFile, "log-file", "", "write JSON logs to file (disabled by default)")
	cmd.Flags().StringVar(&logLevel, "log-level", "debug", "log file level (debug, info, warn, error)")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show debug logs in console")

	return cmd
}
```

Also update the `return` at the end — the function now returns `cmd` instead of using inline `&cobra.Command`.

- [ ] **Step 2: Update other agent commands to use ConsoleHandler**

Replace inline JSON loggers in joinCmd, pairCmd, devicesCmd, renameCmd.

In `joinCmd()` (line 97), replace:
```go
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
```
with:
```go
			logger := slog.New(common.NewConsoleHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
```

Same replacement at lines 164 (remove — now handled above), 281, 307, 347.

Note: line 164 is inside `startCmd()` which was already replaced in Step 1.

- [ ] **Step 3: Verify agent builds**

Run: `go build ./cmd/hubfuse-agent/`
Expected: builds successfully with no errors.

- [ ] **Step 4: Commit**

```bash
git add cmd/hubfuse-agent/main.go
git commit -m "feat: update agent to use human-readable console + optional file logging"
```

---

### Task 6: Update integration tests

**Files:**
- Modify: `tests/integration/integration_test.go:63`
- Modify: `tests/integration/reconnect_test.go:97`

Integration tests create their own `slog.Logger` directly (via `slog.NewTextHandler`), not through `SetupLogger`. No code changes needed — just verify everything compiles and passes.

- [ ] **Step 1: Verify all tests pass**

Run: `make test`
Expected: all unit and integration tests PASS.

- [ ] **Step 3: Run vet**

Run: `make vet`
Expected: no issues.

---

### Task 7: Final verification

- [ ] **Step 1: Full build**

Run: `make build`
Expected: both binaries build successfully.

- [ ] **Step 2: Full test suite**

Run: `make test`
Expected: all tests PASS.

- [ ] **Step 3: Vet**

Run: `make vet`
Expected: no issues.
