// Package obs holds the observability surface shared by the server and node
// daemons: structured logging and process metrics.
//
// The package deliberately depends on nothing inside a4s. Logging and metrics
// are cross-cutting, and a dependency from obs back into control or node would
// make the kernel harder to reason about than the thing it observes.
package obs

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format selects how log records are rendered.
type Format string

const (
	// FormatText is human-readable, for an operator watching a terminal.
	FormatText Format = "text"
	// FormatJSON emits one JSON object per record, for log shipping.
	FormatJSON Format = "json"
)

// Config describes a daemon's logging setup.
type Config struct {
	// Level is the minimum severity to emit: debug, info, warn, or error.
	Level string
	// Format is text or json.
	Format Format
	// Output receives records. Defaults to stderr, which keeps logs separate
	// from any result a command prints on stdout.
	Output io.Writer
	// Component names the daemon and is attached to every record, so a
	// collector holding both server and node logs can tell them apart.
	Component string
}

// ParseLevel maps a level name to its slog level. An unrecognized name is an
// error rather than a silent default, because a typo that quietly disables
// warnings is worse than a refused start.
func ParseLevel(name string) (slog.Level, error) {
	var level slog.Level
	if name == "" {
		return slog.LevelInfo, nil
	}
	if err := level.UnmarshalText([]byte(strings.ToUpper(name))); err != nil {
		return 0, err
	}
	return level, nil
}

// New builds a logger from config. It returns an error for an unusable level or
// format so a daemon fails at startup rather than logging incorrectly for its
// whole lifetime.
func New(config Config) (*slog.Logger, error) {
	level, err := ParseLevel(config.Level)
	if err != nil {
		return nil, err
	}
	output := config.Output
	if output == nil {
		output = os.Stderr
	}
	options := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch config.Format {
	case FormatJSON:
		handler = slog.NewJSONHandler(output, options)
	case FormatText, "":
		handler = slog.NewTextHandler(output, options)
	default:
		return nil, &badFormatError{format: string(config.Format)}
	}

	logger := slog.New(handler)
	if config.Component != "" {
		logger = logger.With(slog.String("component", config.Component))
	}
	return logger, nil
}

type badFormatError struct{ format string }

func (e *badFormatError) Error() string {
	return "unknown log format " + e.format + ": want text or json"
}

// Discard returns a logger that drops everything, for tests and for callers
// that have no logger to pass. Returning this instead of a nil *slog.Logger
// means call sites never need a nil check before logging.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
