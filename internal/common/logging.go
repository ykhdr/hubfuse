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
