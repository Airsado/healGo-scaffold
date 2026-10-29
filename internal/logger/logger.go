// Package logger builds the process logger without choosing any business fields.
// Logs go to stdout; the container/platform owns retention and rotation.
package logger

import (
	"errors"
	"log/slog"
	"os"

	"healgo-scaffold/configs"
)

// New rejects unknown levels instead of silently hiding operational errors.
// A logger is passed explicitly to other packages, avoiding a mutable global singleton.
func New(cfg configs.Logging) (*slog.Logger, error) {
	// slog handlers filter by level before encoding attributes, keeping normal logs inexpensive.
	// Match exact configuration values so a typo cannot silently lower the logging threshold.
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, errors.New("invalid logging level")
	}
	options := &slog.HandlerOptions{Level: level}
	// Source locations are omitted by default: request IDs and event names are stable across builds.
	var handler slog.Handler
	switch cfg.Format {
	case "json":
		// JSON escapes control characters and gives the log collector structured attributes.
		handler = slog.NewJSONHandler(os.Stdout, options)
	case "text":
		// Text is useful for local terminals while preserving the same structured slog API.
		handler = slog.NewTextHandler(os.Stdout, options)
	default:
		return nil, errors.New("invalid logging format")
	}
	// Do not call slog.SetDefault here; constructor injection keeps tests and applications isolated.
	// Rotation is deliberately absent because Docker and the application must not race over files.
	return slog.New(handler), nil
}
