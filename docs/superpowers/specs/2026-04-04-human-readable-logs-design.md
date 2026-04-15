# Human-Readable Console Logs + Debug Log File

**Issue**: ykhdr/hubfuse#6
**Date**: 2026-04-04

## Problem

Both hub and agent output raw JSON structured logs to the console, which is hard to read. There's no way to save detailed logs to a file for troubleshooting.

## Design

### ConsoleHandler

New custom `slog.Handler` in `internal/common/console_handler.go`.

Output format:

```
21:25:24 [INFO]  connecting to hub addr=192.168.31.158:9090
21:25:24 [WARN]  connection lost, retrying err=timeout backoff=5s
21:25:24 [ERROR] mount failed path=/mnt/share err=permission denied
21:25:24 [DEBUG] heartbeat sent device_id=abc-123
```

Behaviour:
- **Timestamp**: `15:04:05` format (time only, no date)
- **Level prefixes**: `[INFO]`, `[WARN]`, `[ERROR]`, `[DEBUG]` — padded to 7 chars for alignment
- **Colors**: green for INFO, yellow for WARN, red for ERROR, cyan for DEBUG. Only when output is a terminal (`golang.org/x/term`, `term.IsTerminal()`)
- **Attributes**: appended as `key=value` pairs after the message, space-separated
- **Groups**: supported via `WithGroup()`/`WithAttrs()` as required by `slog.Handler` interface

### MultiHandler

New `slog.Handler` in `internal/common/multi_handler.go` that fans out records to multiple child handlers.

```go
type MultiHandler struct {
    handlers []slog.Handler
}
```

- `Enabled()` returns true if any child is enabled for that level
- `Handle()` calls all children, returns first error
- `WithAttrs()`/`WithGroup()` return new `MultiHandler` with the method applied to all children

Used for dual output: `MultiHandler{ConsoleHandler(stderr, info), JSONHandler(file, debug)}`. When no log file is requested, just a bare `ConsoleHandler` — no wrapping.

### SetupLogger Refactor

Replace current `SetupLogger(level, output string)` with:

```go
type LoggerOptions struct {
    ConsoleLevel slog.Level  // console log level (default: Info)
    LogFile      string      // file path ("" = no file logging)
    FileLevel    slog.Level  // file log level (default: Debug)
    Verbose      bool        // if true, console also shows Debug
}

func SetupLogger(opts LoggerOptions) (*slog.Logger, error)
```

Logic:
- Always creates `ConsoleHandler` on stderr
- If `Verbose` — console level is Debug regardless of `ConsoleLevel`
- If `LogFile` is non-empty — creates parent directory, opens file (O_CREATE|O_APPEND|O_WRONLY, 0644), wraps in `MultiHandler{console, jsonFile}`
- If `LogFile` is empty — returns logger with just `ConsoleHandler`

### CLI Flags

**Agent** (`hubfuse-agent start`):
- `--log-file <path>` — enables file logging (default: empty, no file)
- `--log-level <level>` — file log level (debug/info/warn/error, default: debug)
- `--verbose` / `-v` — show debug logs in console

Other agent commands (join, pair, devices, rename) use bare `ConsoleHandler` at info level, no configurable flags.

**Hub** (`hubfuse-hub start`):
- `--log-file <path>` — replaces old `--log-output`
- `--log-level <level>` — file log level (default: debug), replaces old `--log-level`
- `--verbose` / `-v` — show debug in console

Old flags `--log-output` and `--log-level` (in old meaning) are removed.

### Changes to Existing Code

**`internal/hub/hub.go`**: `NewHub()` switches to `SetupLogger(LoggerOptions{...})`. `HubConfig` updated: `LogOutput` -> `LogFile`, `Verbose` added.

**`cmd/hubfuse-agent/main.go`**:
- `startCmd()` adds `--log-file`, `--log-level`, `-v` flags, calls `SetupLogger(LoggerOptions{...})`
- Other commands replace inline `slog.NewJSONHandler(...)` with `slog.New(common.NewConsoleHandler(os.Stderr, slog.LevelInfo))`

**`cmd/hubfuse-hub/main.go`**: flags replaced as described above.

Internal components (daemon, connector, mounter, registry, heartbeat, server) are **unchanged** — they accept `*slog.Logger` and continue to work. Only the logger creation point changes.

### Testing

**Unit tests** in `internal/common/`:
- `console_handler_test.go` — format validation: timestamp, levels, attributes, groups. Colors tested via `bytes.Buffer` (not a terminal = no escape codes)
- `multi_handler_test.go` — writes to all children, `Enabled()` returns true if any child enabled, `WithAttrs`/`WithGroup` propagate
- `logging_test.go` — updated for new `SetupLogger(LoggerOptions)` signature: with/without file, levels, verbose mode

**Integration tests** — update logger creation in `tests/integration/` to new API, no logic changes.

### Dependencies

New dependency: `golang.org/x/term` for terminal detection.
