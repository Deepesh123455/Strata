// Package logger is the process-wide structured logger for Powerhouse Cache.
//
// It wraps the standard library's log/slog behind a singleton so every package
// logs through ONE configured handler — consistent levels, RFC3339 timestamps,
// and machine-parseable fields — without threading a *slog.Logger through every
// constructor. Using slog (stdlib) keeps the zero-dependency, distroless build
// story intact: no new go.sum entries, no extra attack surface.
//
// Two output shapes, selected by LOG_FORMAT:
//
//   - text (default): human-readable "key=value" lines for local dev.
//   - json:           one JSON object per line, for CloudWatch / Loki / etc.
//
// Either way every line carries a timestamp, a level, and structured fields, so
// production logs can be filtered by level and queried by client, command, etc.
//
// Configuration is environment-driven (LOG_LEVEL, LOG_FORMAT) so the same binary
// logs verbosely in dev and as filtered JSON in prod with no code change.
package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Format selects the on-the-wire encoding of each log line.
type Format string

const (
	FormatText Format = "text" // human-readable key=value (dev default)
	FormatJSON Format = "json" // one JSON object per line (prod)
)

// Config controls how the singleton is built. The zero value is valid: it logs
// INFO and above as text to stdout.
type Config struct {
	// Level is the minimum level emitted; lines below it are dropped cheaply.
	Level slog.Level
	// Format selects text vs JSON encoding. Empty defaults to text.
	Format Format
	// Output is where lines are written. nil defaults to os.Stdout (12-factor:
	// the process logs to stdout; Docker/ECS capture it).
	Output io.Writer
	// AddSource annotates each line with the source file:line of the call site.
	AddSource bool
}

var (
	mu       sync.RWMutex
	instance *slog.Logger
	format   = FormatText
	ready    bool
)

// Init builds the singleton from cfg. The FIRST call wins; later calls are
// ignored so a stray re-init can't swap the logger out from under goroutines
// that already captured child loggers. Safe for concurrent use.
func Init(cfg Config) {
	mu.Lock()
	defer mu.Unlock()
	if ready {
		return
	}
	instance, format = build(cfg)
	slog.SetDefault(instance)
	ready = true
}

// InitFromEnv configures the singleton from the environment:
//
//	LOG_LEVEL  = debug | info | warn | error   (default info)
//	LOG_FORMAT = text  | json                  (default text)
func InitFromEnv() {
	Init(Config{
		Level:  parseLevel(os.Getenv("LOG_LEVEL")),
		Format: parseFormat(os.Getenv("LOG_FORMAT")),
	})
}

// build constructs a *slog.Logger and reports the format actually used.
func build(cfg Config) (*slog.Logger, Format) {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}
	opts := &slog.HandlerOptions{Level: cfg.Level, AddSource: cfg.AddSource}

	f := cfg.Format
	if f == "" {
		f = FormatText
	}
	var h slog.Handler
	if f == FormatJSON {
		h = slog.NewJSONHandler(out, opts)
	} else {
		h = slog.NewTextHandler(out, opts)
	}
	return slog.New(h), f
}

// L returns the singleton, lazily initialising it from the environment on first
// use so packages (and tests) that log before main calls Init still get a valid
// logger instead of a nil panic.
func L() *slog.Logger {
	mu.RLock()
	if ready {
		l := instance
		mu.RUnlock()
		return l
	}
	mu.RUnlock()

	InitFromEnv()

	mu.RLock()
	defer mu.RUnlock()
	return instance
}

// With returns a child logger that tags every line with the given fields — e.g.
// a per-connection logger carrying {component, client}. Children are cheap and
// share the parent's handler.
func With(args ...any) *slog.Logger { return L().With(args...) }

// Enabled reports whether the singleton would emit at level. Guard hot-path
// Debug logging with this once (outside the loop) so production never pays for
// building field slices that the handler would immediately discard.
func Enabled(level slog.Level) bool { return L().Enabled(context.Background(), level) }

// TextMode reports whether the active format is human-readable text. Callers use
// it to gate dev-only decorative output (e.g. the boot banner) so JSON logs stay
// clean and parseable.
func TextMode() bool {
	mu.RLock()
	defer mu.RUnlock()
	return format == FormatText
}

// --- Package-level convenience wrappers over the singleton ---

func Debug(msg string, args ...any) { L().Debug(msg, args...) }
func Info(msg string, args ...any)  { L().Info(msg, args...) }
func Warn(msg string, args ...any)  { L().Warn(msg, args...) }
func Error(msg string, args ...any) { L().Error(msg, args...) }

// Fatal logs at ERROR and terminates the process with status 1. Reserve it for
// unrecoverable boot/runtime failures where continuing would be unsafe.
func Fatal(msg string, args ...any) {
	L().Error(msg, args...)
	os.Exit(1)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func parseFormat(s string) Format {
	if strings.EqualFold(strings.TrimSpace(s), string(FormatJSON)) {
		return FormatJSON
	}
	return FormatText
}
