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
