package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/jclement/meshflash/internal/theme"
)

// consoleHandler renders records as a single aligned line:
//
//	15:04:05 INFO  flashing device  device=rak4631 bytes=463104
type consoleHandler struct {
	w      io.Writer
	level  slog.Level
	color  bool
	attrs  []slog.Attr
	groups []string
}

func newConsoleHandler(w io.Writer, level slog.Level, color bool) slog.Handler {
	return &consoleHandler{w: w, level: level, color: color}
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	s := theme.S()
	var b strings.Builder

	b.WriteString(h.paint(s.Muted, r.Time.Format("15:04:05")))
	b.WriteByte(' ')
	b.WriteString(h.levelTag(r.Level))
	b.WriteByte(' ')
	b.WriteString(r.Message)

	// Handler-scoped attrs first (they are the stable context), then the
	// record's own, so repeated fields read consistently down the stream.
	for _, a := range h.attrs {
		h.writeAttr(&b, h.groups, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		h.writeAttr(&b, h.groups, a)
		return true
	})

	b.WriteByte('\n')
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *consoleHandler) writeAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	// Flatten groups into dotted keys; console output is one line per record.
	if a.Value.Kind() == slog.KindGroup {
		sub := a.Value.Group()
		if len(sub) == 0 {
			return
		}
		next := groups
		if a.Key != "" {
			next = append(append([]string{}, groups...), a.Key)
		}
		for _, s := range sub {
			h.writeAttr(b, next, s)
		}
		return
	}

	key := a.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}

	s := theme.S()
	b.WriteByte(' ')
	b.WriteString(h.paint(s.Key, key))
	b.WriteString(h.paint(s.Muted, "="))
	b.WriteString(h.paint(s.Muted, formatValue(a.Value)))
}

func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindDuration:
		return v.Duration().Round(time.Millisecond).String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	case slog.KindString:
		s := v.String()
		// Quote only when the value would otherwise break key=value scanning.
		if s == "" || strings.ContainsAny(s, " \t\"") {
			return fmt.Sprintf("%q", s)
		}
		return s
	default:
		return v.String()
	}
}

func (h *consoleHandler) levelTag(l slog.Level) string {
	s := theme.S()
	var (
		text  string
		style lipgloss.Style
	)
	switch {
	case l < slog.LevelInfo:
		text, style = "DEBUG", s.Debug
	case l < slog.LevelWarn:
		text, style = "INFO ", s.Info
	case l < slog.LevelError:
		text, style = "WARN ", s.Warn
	default:
		text, style = "ERROR", s.Error
	}
	return h.paint(style.Bold(true), text)
}

func (h *consoleHandler) paint(s lipgloss.Style, text string) string {
	if !h.color {
		return text
	}
	return s.Render(text)
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string{}, h.groups...), name)
	return &next
}
