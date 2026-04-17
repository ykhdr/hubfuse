package common

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMultiHandler_WritesToAll(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelInfo})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelInfo})

	multi := NewMultiHandler(h1, h2)
	logger := slog.New(multi)

	logger.Info("hello", "key", "value")

	assert.NotZero(t, buf1.Len(), "handler 1 received no output")
	assert.NotZero(t, buf2.Len(), "handler 2 received no output")
}

func TestMultiHandler_EnabledAny(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelError})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})

	multi := NewMultiHandler(h1, h2)

	assert.True(t, multi.Enabled(context.Background(), slog.LevelDebug),
		"expected Debug to be enabled (h2 accepts it)")
}

func TestMultiHandler_LevelFiltering(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelError})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})

	multi := NewMultiHandler(h1, h2)
	logger := slog.New(multi)

	logger.Debug("debug msg")

	assert.Zero(t, buf1.Len(), "handler 1 (Error level) should have no output, got %q", buf1.String())
	assert.NotZero(t, buf2.Len(), "handler 2 (Debug level) should have output")
}

func TestMultiHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})

	multi := NewMultiHandler(h)
	multi2 := multi.WithAttrs([]slog.Attr{slog.String("comp", "test")})

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	require.NoError(t, multi2.Handle(context.Background(), record))

	assert.Contains(t, buf.String(), "comp=test", "output missing attr")
}

func TestMultiHandler_WithGroup(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})

	multi := NewMultiHandler(h)
	multi2 := multi.WithGroup("grp")

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	record.AddAttrs(slog.String("key", "val"))
	require.NoError(t, multi2.Handle(context.Background(), record))

	assert.Contains(t, buf.String(), "grp.key=val", "output missing grouped attr")
}
