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
