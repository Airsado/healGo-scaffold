package bootstrap

import (
	"context"
	"testing"
	"time"

	"healgo-scaffold/configs"
	"healgo-scaffold/internal/stream"
)

// A callback that ignores cancellation must consume only one shutdown budget.
// Close cannot grant it more time after Serve has already finished draining.
func TestExpiredStreamDrainIsNotRetriedDuringClose(t *testing.T) {
	manager, _ := stream.NewManager(1)
	session, err := manager.Begin(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer session.End()
	cleaned := false
	runtime := &Runtime{Config: configs.Config{HTTP: configs.HTTP{ShutdownTimeout: 15 * time.Millisecond}},
		Streams: manager, cleanup: func() { cleaned = true }}
	first := runtime.drain(nil)
	if first == nil || manager.Active() != 1 {
		t.Fatal("unfinished work must report a bounded drain failure")
	}
	// The same safe result is cached, rather than starting another timed wait.
	if second := runtime.drain(nil); second != first {
		t.Fatal("drain retried instead of reusing its completed budget")
	}
	runtime.Close()
	runtime.Close()
	if !cleaned || runtime.drainErr != first {
		t.Fatal("Close did not reuse the existing drain before resource cleanup")
	}
}
