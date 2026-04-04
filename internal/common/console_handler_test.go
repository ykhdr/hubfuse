package common

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/slogtest"
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

func TestConsoleHandler_SlogConformance(t *testing.T) {
	var buf bytes.Buffer
	slogtest.Run(t, func(t *testing.T) slog.Handler {
		buf.Reset()
		return NewConsoleHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	}, func(t *testing.T) map[string]any {
		return parseConsoleOutput(t, buf.String())
	})
}

// parseConsoleOutput parses a single ConsoleHandler log line into a map
// suitable for slogtest conformance checks.
//
// Format: [HH:MM:SS] [LEVEL]  message key=value G.key=value
func parseConsoleOutput(t *testing.T, line string) map[string]any {
	t.Helper()
	line = strings.TrimRight(line, "\n")
	m := map[string]any{}

	rest := line

	// Parse optional timestamp (HH:MM:SS).
	if len(rest) >= 8 && rest[2] == ':' && rest[5] == ':' {
		ts, err := time.Parse(time.TimeOnly, rest[:8])
		if err == nil {
			m[slog.TimeKey] = ts
			rest = strings.TrimPrefix(rest[8:], " ")
		}
	}

	// Parse level tag: [DEBUG], [INFO] , [WARN] , [ERROR].
	if len(rest) > 0 && rest[0] == '[' {
		end := strings.IndexByte(rest, ']')
		if end > 0 {
			m[slog.LevelKey] = rest[1:end]
			rest = strings.TrimLeft(rest[end+1:], " ")
		}
	}

	// The remainder is: message [key=value ...]
	// Split on first space that precedes a key=value pair.
	// Find where key=value pairs begin by scanning for " word=".
	msgEnd := len(rest)
	for i := 0; i < len(rest); i++ {
		if rest[i] == ' ' {
			// Check if what follows looks like key=value.
			sub := rest[i+1:]
			if eq := strings.IndexByte(sub, '='); eq > 0 {
				// Verify no spaces before the '='.
				if !strings.ContainsAny(sub[:eq], " \t") {
					msgEnd = i
					break
				}
			}
		}
	}
	m[slog.MessageKey] = rest[:msgEnd]
	rest = strings.TrimLeft(rest[msgEnd:], " ")

	// Parse key=value pairs. Values run until the next " key=" or end.
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			break
		}
		key := rest[:eq]
		rest = rest[eq+1:]

		// Value ends at the next " word=" or end of string.
		valEnd := len(rest)
		for i := 0; i < len(rest); i++ {
			if rest[i] == ' ' {
				sub := rest[i+1:]
				if nextEq := strings.IndexByte(sub, '='); nextEq > 0 {
					if !strings.ContainsAny(sub[:nextEq], " \t") {
						valEnd = i
						break
					}
				}
			}
		}
		val := rest[:valEnd]
		rest = strings.TrimLeft(rest[valEnd:], " ")

		// Insert key (possibly dot-prefixed) into nested map structure.
		setNestedKey(m, strings.Split(key, "."), val)
	}

	return m
}

// setNestedKey inserts val into m at the path described by keys,
// creating intermediate map[string]any as needed.
func setNestedKey(m map[string]any, keys []string, val string) {
	if len(keys) == 1 {
		m[keys[0]] = val
		return
	}
	sub, ok := m[keys[0]]
	if !ok {
		sub = map[string]any{}
		m[keys[0]] = sub
	}
	if subMap, ok := sub.(map[string]any); ok {
		setNestedKey(subMap, keys[1:], val)
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
