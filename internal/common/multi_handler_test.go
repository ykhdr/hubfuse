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

	if buf1.Len() != 0 {
		t.Errorf("handler 1 (Error level) should have no output, got %q", buf1.String())
	}
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
