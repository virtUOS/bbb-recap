// Package logging builds the service logger: log/slog JSON lines with the
// keys time, level and msg (docs/reference/README.md "Logging").
package logging

import (
	"fmt"
	"io"
	"log/slog"
)

// ParseLevel parses a RECAP_LOG_LEVEL value: debug, info, warn or error.
func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("want debug, info, warn or error, got %q", s)
}

// New returns a JSON logger that writes to w (stderr in the binaries).
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
