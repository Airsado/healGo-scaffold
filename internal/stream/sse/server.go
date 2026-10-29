package sse

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"healgo-scaffold/internal/stream"
)

// Options bound each stream independently; zero values select conservative defaults.
// Authorize must honor ctx and recheck current permissions rather than cached roles.
type Options struct {
	// Independent duration limits keep long streams separate from normal API
	// latency budgets while still giving every admitted connection an end time.
	MaxDuration       time.Duration
	WriteTimeout      time.Duration
	HeartbeatInterval time.Duration
	RecheckInterval   time.Duration
	MaxEventBytes     int
	Authorize         func(context.Context) error
}

// Server is immutable after construction and can serve concurrent HTTP requests.
// The shared manager limits aggregate connections across SSE and other transports.
type Server struct {
	manager *stream.Manager
	opts    Options
}

func New(manager *stream.Manager, opts Options) (*Server, error) {
	// Reject negative values before applying defaults so a configuration typo
	// cannot accidentally disable the corresponding resource protection.
	if manager == nil || opts.MaxDuration < 0 || opts.WriteTimeout < 0 || opts.HeartbeatInterval < 0 || opts.RecheckInterval < 0 || opts.MaxEventBytes < 0 {
		return nil, ErrInvalidOptions
	}
	if opts.MaxDuration == 0 {
		// Zero means a usable default, rather than an unlimited connection.
		opts.MaxDuration = 5 * time.Minute
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 5 * time.Second
	}
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = 15 * time.Second
	}
	if opts.RecheckInterval == 0 {
		opts.RecheckInterval = 30 * time.Second
	}
	if opts.MaxEventBytes == 0 {
		opts.MaxEventBytes = 64 << 10
	}
	// Even the smallest empty data frame needs eight bytes; a smaller limit cannot
	// carry any valid application event and almost certainly indicates a typo.
	if opts.MaxEventBytes < len("data: \n\n") {
		return nil, ErrInvalidOptions
	}
	return &Server{manager: manager, opts: opts}, nil
}

// Serve owns the entire HTTP response, including safe errors before streaming.
// The callback runs on the request goroutine and MUST stop when ctx is canceled.
// After Serve returns callers must not write JSON or retain/use the Writer.
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, handler func(context.Context, *Writer) error) error {
	// Invalid setup is rejected before beginning a manager session; a broken
	// route must not consume the shared connection capacity.
	if w == nil || r == nil || handler == nil {
		if w != nil {
			stream.Reject(w, 500, "stream_invalid_options", "流连接配置错误")
		}
		return ErrInvalidOptions
	}
	if err := r.Context().Err(); err != nil {
		// An already expired ordinary request must not revive itself as a stream.
		stream.Reject(w, 408, "stream_request_expired", "流连接请求已过期")
		return err
	}
	if r.Method != http.MethodGet {
		// Standard EventSource clients use GET; rejecting other methods prevents
		// request bodies from becoming an undocumented bidirectional protocol.
		w.Header().Set("Allow", http.MethodGet)
		stream.Reject(w, 405, "stream_method_not_allowed", "流连接仅支持 GET")
		return ErrInvalidOptions
	}
	// Admission precedes headers so capacity and shutdown failures can still use
	// the normal JSON envelope instead of corrupting an established event stream.
	session, err := s.manager.Begin(stream.RequestContext(r.Context()), s.opts.MaxDuration)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			stream.Reject(w, 408, "stream_request_expired", "流连接请求已过期")
		} else {
			stream.Reject(w, 503, "stream_unavailable", "流连接暂不可用")
		}
		return err
	}
	defer session.End()
	// A local cause distinguishes permission revocation from client cancellation
	// without changing or canceling the original HTTP request context.
	ctx, cancel := context.WithCancelCause(session.Context())
	defer cancel(nil)
	if !authorize(ctx, s.opts.Authorize) {
		if ctx.Err() != nil {
			stream.Reject(w, 408, "stream_request_expired", "流连接请求已过期")
			return safeCause(ctx)
		}
		stream.Reject(w, 403, "stream_forbidden", "流连接授权已失效")
		return ErrUnauthorized
	}
	controller := http.NewResponseController(w)
	// Fail before headers if the wrapper hides deadline support. An unbounded
	// write would otherwise keep shutdown and disconnected clients stuck forever.
	if err := controller.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout)); err != nil {
		stream.Reject(w, 500, "stream_unsupported", "服务器不支持流连接")
		return ErrUnsupported
	}
	out := &Writer{ctx: ctx, cancel: cancel, response: w, controller: controller, gate: make(chan struct{}, 1), timeout: s.opts.WriteTimeout, limit: s.opts.MaxEventBytes}
	out.gate <- struct{}{}
	// The sole token grants exclusive write ownership. A channel, unlike Mutex,
	// lets a queued sender select ctx.Done while another write is blocked.
	interruptStop := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		select {
		case <-interruptStop:
			return
		case <-ctx.Done():
		}
		select {
		case <-interruptStop:
			return
		default:
		}
		// SetWriteDeadline is safe alongside a blocked network write. This lock
		// prevents a sender from overwriting cancellation with a future deadline.
		out.deadlineMu.Lock()
		_ = controller.SetWriteDeadline(time.Now())
		out.deadlineMu.Unlock()
	}()
	defer func() {
		finished := ctx.Err() == nil
		// Normal completion must let net/http write its final chunk. Stop the
		// interrupt watcher before canceling our own workers, rather than forcing
		// an expired socket deadline merely because the callback returned normally.
		close(interruptStop)
		cancel(nil)
		workers.Wait()
		// No transport goroutine may retain a ResponseWriter after handler return;
		// net/http can already be serving the next request on the connection then.
		// Join any in-flight Send before returning the ResponseWriter to net/http.
		// Business-created goroutines must still be joined by the callback itself.
		<-out.gate
		out.gate <- struct{}{}
		if finished {
			_ = controller.SetWriteDeadline(time.Time{})
		}
	}()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// An inherited Content-Length would truncate an open-ended event sequence.
	w.Header().Del("Content-Length")
	// A comment flushes headers immediately, even when the first business event
	// arrives much later. Browser clients must not wait for a full proxy buffer.
	if err := out.write([]byte(": connected\n\n")); err != nil {
		return err
	}
	workers.Add(1)
	go func() {
		// One controlled worker owns both timers; no ticker creates an unbounded
		// queue when the client or permission service is temporarily slow.
		defer workers.Done()
		heartbeat := time.NewTicker(s.opts.HeartbeatInterval)
		defer heartbeat.Stop()
		recheck := time.NewTicker(s.opts.RecheckInterval)
		defer recheck.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				if out.write([]byte(": heartbeat\n\n")) != nil {
					return
				}
			case <-recheck.C:
				if !authorize(ctx, s.opts.Authorize) {
					// Cancel first; defer End releases capacity only after the business
					// callback and our workers have actually finished their cleanup.
					cancel(ErrUnauthorized)
					return
				}
			}
		}
	}()
	if err := callHandler(ctx, out, handler); err != nil {
		if ctx.Err() != nil {
			return out.cause()
		}
		return ErrHandler
	}
	if ctx.Err() != nil {
		return out.cause()
	}
	return nil
}

// A background permission check must never turn a callback panic into a process
// crash, nor expose its panic/error payload in the transport response.
func authorize(ctx context.Context, check func(context.Context) error) (ok bool) {
	if check == nil {
		return ctx.Err() == nil
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return ctx.Err() == nil && check(ctx) == nil && ctx.Err() == nil
}

func callHandler(ctx context.Context, out *Writer, handler func(context.Context, *Writer) error) (err error) {
	// Gin recovery would append JSON to an already started SSE body. Classifying
	// callback panics here keeps the wire format valid and secret values private.
	defer func() {
		if recover() != nil {
			err = ErrHandler
		}
	}()
	return handler(ctx, out)
}

func safeCause(ctx context.Context) error {
	// An upstream WithCancelCause may carry a private error. Only known public
	// categories cross this boundary; other causes become ordinary cancellation.
	for _, known := range []error{ErrUnauthorized, ErrWrite, context.DeadlineExceeded} {
		if errors.Is(context.Cause(ctx), known) {
			return known
		}
	}
	return context.Canceled
}
