package logging

import (
	"io"
	"log/slog"
	"strings"
)

func New(level string, w io.Writer) *slog.Logger {
	l := new(slog.LevelVar)
	switch strings.ToLower(level) {
	case "debug":
		l.Set(slog.LevelDebug)
	case "warn":
		l.Set(slog.LevelWarn)
	case "error":
		l.Set(slog.LevelError)
	default:
		l.Set(slog.LevelInfo)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: l}))
}
