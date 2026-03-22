package common

import (
	"fmt"
	"log/slog"
	"os"
)

// SetupLogger creates a structured JSON logger with the given level and output
// target. level must be one of "debug", "info", "warn", or "error". output
// must be "stderr" or a writable file path.
func SetupLogger(level string, output string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unsupported log level %q: must be debug, info, warn, or error", level)
	}

	var w *os.File
	if output == "stderr" {
		w = os.Stderr
	} else {
		f, err := os.OpenFile(output, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("open log file %q: %w", output, err)
		}
		w = f
	}

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler), nil
}
