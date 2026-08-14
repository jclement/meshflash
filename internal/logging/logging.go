// Package logging wires up the two log sinks meshflash always wants: a pretty
// console stream for the operator, and a verbose rolling file that survives the
// session so a failed field flash can be diagnosed after the fact.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Options configures Setup.
type Options struct {
	// Dir is where session logs are written. Empty disables file logging.
	Dir string
	// ConsoleLevel gates what reaches the terminal.
	ConsoleLevel slog.Level
	// Console receives human-readable output. Nil disables console logging,
	// which is what the TUI does while it owns the screen.
	Console io.Writer
	// NoColor forces plain text even on a TTY.
	NoColor bool
	// Keep is how many past session logs to retain.
	Keep int
}

// Session is a configured logger plus the file it is writing to.
type Session struct {
	Logger *slog.Logger
	// Path is the session log file, or "" when file logging is disabled.
	Path string

	file *os.File
	fan  *fanout
}

// Close flushes and closes the session log file.
func (s *Session) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	return s.file.Close()
}

// Setup builds the logger. The file sink always records at Debug so a
// post-mortem has everything even when the console was quiet.
func Setup(opts Options) (*Session, error) {
	if opts.Keep <= 0 {
		opts.Keep = 20
	}
	s := &Session{fan: &fanout{}}

	if opts.Console != nil {
		s.fan.add(newConsoleHandler(opts.Console, opts.ConsoleLevel, !opts.NoColor))
	}

	if opts.Dir != "" {
		if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
			return nil, fmt.Errorf("create log dir: %w", err)
		}
		name := fmt.Sprintf("meshflash-%s.log", time.Now().Format("20060102-150405"))
		path := filepath.Join(opts.Dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		s.file = f
		s.Path = path
		s.fan.add(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
		pruneLogs(opts.Dir, opts.Keep)
	}

	s.Logger = slog.New(s.fan)
	return s, nil
}

// Discard returns a logger that drops everything, for tests and library use.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// pruneLogs keeps the newest `keep` session logs and deletes the rest. Field
// units run for months on a small disk; unbounded logs are a real failure mode.
func pruneLogs(dir string, keep int) {
	entries, err := filepath.Glob(filepath.Join(dir, "meshflash-*.log"))
	if err != nil || len(entries) <= keep {
		return
	}
	sort.Strings(entries) // timestamped names sort chronologically
	for _, p := range entries[:len(entries)-keep] {
		_ = os.Remove(p)
	}
}

// fanout dispatches every record to all configured handlers. slog has no
// built-in multi-handler and we need console and file to disagree on level.
type fanout struct {
	mu       sync.Mutex
	handlers []slog.Handler
}

func (f *fanout) add(h slog.Handler) {
	f.handlers = append(f.handlers, h)
}

func (f *fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f *fanout) Handle(ctx context.Context, r slog.Record) error {
	// Handlers write to shared sinks (stderr, one file); serialise so records
	// from concurrent download workers don't interleave mid-line.
	f.mu.Lock()
	defer f.mu.Unlock()
	var firstErr error
	for _, h := range f.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &fanout{handlers: make([]slog.Handler, 0, len(f.handlers))}
	for _, h := range f.handlers {
		next.handlers = append(next.handlers, h.WithAttrs(attrs))
	}
	return next
}

func (f *fanout) WithGroup(name string) slog.Handler {
	next := &fanout{handlers: make([]slog.Handler, 0, len(f.handlers))}
	for _, h := range f.handlers {
		next.handlers = append(next.handlers, h.WithGroup(name))
	}
	return next
}
