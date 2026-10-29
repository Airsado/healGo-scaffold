package sse

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Writer has no event queue: each Send waits for the previous write and its flush.
// A bounded channel acts as a context-aware mutex so canceled waiters stop promptly.
type Writer struct {
	ctx        context.Context
	cancel     context.CancelCauseFunc
	response   http.ResponseWriter
	controller *http.ResponseController
	gate       chan struct{}
	deadlineMu sync.Mutex
	timeout    time.Duration
	limit      int
	writeError atomic.Bool
}

func (w *Writer) Send(event Event) error {
	// Check before encoding so canceled connections do not keep allocating frame
	// buffers while a producer notices the cancellation on its next iteration.
	if w.ctx.Err() != nil {
		return w.cause()
	}
	frame, err := encodeEvent(event, w.limit)
	if err != nil {
		// Invalid data is recoverable: the producer can choose to send a valid
		// event instead. Only actual transport failures cancel the connection.
		return err
	}
	return w.write(frame)
}

// SendJSON uses the same framing/size rules as Send. Encoder failures are reduced
// to public categories because a custom Marshaler can return credential values.
func (w *Writer) SendJSON(name string, value any) error {
	if w.ctx.Err() != nil {
		return w.cause()
	}
	if !validField(name) {
		// Validate before invoking user-supplied MarshalJSON code for this event.
		return ErrInvalidEvent
	}
	data, err := encodeJSON(value, w.limit)
	if err != nil {
		return err
	}
	return w.Send(Event{Name: name, Data: data})
}

func (w *Writer) write(frame []byte) error {
	// There is no buffer of pending events. Backpressure stays with each caller
	// and cancellation wakes callers waiting for exclusive write access.
	select {
	case <-w.ctx.Done():
		return w.cause()
	case <-w.gate:
	}
	defer func() { w.gate <- struct{}{} }()
	// The second cancellation check closes the race where both a free token and
	// ctx.Done were ready and select happened to choose the token.
	w.deadlineMu.Lock()
	if w.ctx.Err() != nil {
		w.deadlineMu.Unlock()
		return w.cause()
	}
	deadline := time.Now().Add(w.timeout)
	// Serialize deadline changes with the cancellation watcher, not the write:
	// the watcher must remain able to interrupt a write that already blocks.
	deadlineErr := w.controller.SetWriteDeadline(deadline)
	w.deadlineMu.Unlock()
	if deadlineErr != nil {
		w.writeError.Store(true)
		w.cancel(ErrWrite)
		return ErrWrite
	}
	// Flushing is part of the bounded operation, not a later unprotected step:
	// a slow client may block on Flush even when Write fits the server's buffer.
	n, err := w.response.Write(frame)
	if err == nil && n == len(frame) {
		err = w.controller.Flush()
	}
	if err != nil || n != len(frame) {
		if w.ctx.Err() != nil && time.Now().Before(deadline) {
			return w.cause()
		}
		// net/http can cancel the request as a consequence of a write timeout
		// before our cancel runs. Preserve the transport failure category rather
		// than misreporting that deadline as an ordinary client cancellation.
		w.writeError.Store(true)
		w.cancel(ErrWrite)
		return ErrWrite
	}
	// A deadline bounds this write, not the idle interval until the next event.
	// Leaving it armed would make the default 15s heartbeat fail after 5s idle.
	w.deadlineMu.Lock()
	if w.ctx.Err() == nil {
		err = w.controller.SetWriteDeadline(time.Time{})
	}
	w.deadlineMu.Unlock()
	if err != nil {
		w.writeError.Store(true)
		w.cancel(ErrWrite)
		return ErrWrite
	}
	if w.ctx.Err() != nil {
		return w.cause()
	}
	return nil
}

func (w *Writer) cause() error {
	// Atomic storage is needed because the heartbeat worker and the business
	// callback may observe a write failure on different goroutines.
	// The flag contains no network error text and can safely be inspected even
	// after the request context was canceled by the HTTP server itself.
	if w.writeError.Load() {
		return ErrWrite
	}
	return safeCause(w.ctx)
}
