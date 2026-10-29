package logger

import (
	"context"
	"log/slog"
	"testing"

	"healgo-scaffold/configs"
)

func TestLoggerLevelAndConfiguration(t *testing.T) {
	log, err := New(configs.Logging{Level: "warn", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	if log.Enabled(context.Background(), slog.LevelInfo) || !log.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("log threshold is not enforced")
	}
	for _, cfg := range []configs.Logging{{Level: "trace", Format: "json"}, {Level: "info", Format: "xml"}} {
		if _, err := New(cfg); err == nil {
			t.Fatal("unknown logger option accepted")
		}
	}
}
