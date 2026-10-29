package sse_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"heago-scaffold/internal/stream"
	"heago-scaffold/internal/stream/sse"
)

type fixture struct {
	manager *stream.Manager
	server  *httptest.Server
	result  chan error
}

func newFixture(t *testing.T, opts sse.Options, callback func(context.Context, *sse.Writer) error) fixture {
	return newFixtureWithWriter(t, opts, callback, nil)
}

func newFixtureWithWriter(t *testing.T, opts sse.Options, callback func(context.Context, *sse.Writer) error, wrap func(http.ResponseWriter) http.ResponseWriter) fixture {
	t.Helper()
	manager, err := stream.NewManager(1)
	if err != nil {
		t.Fatal(err)
	}
	server, err := sse.New(manager, opts)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 8)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wrap != nil {
			w = wrap(w)
		}
		result <- server.Serve(w, r, callback)
	}))
	t.Cleanup(func() {
		manager.Close()
		httpServer.Close()
	})
	return fixture{manager, httpServer, result}
}

func open(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func frame(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var out strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString(line)
		if line == "\n" {
			return out.String()
		}
	}
}

func outcome(t *testing.T, results <-chan error, want error) {
	t.Helper()
	select {
	case err := <-results:
		if !errors.Is(err, want) {
			t.Fatalf("Serve error = %v, want %v", err, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

func TestRealHTTPInitialFlushMultilineAndJSON(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f := newFixture(t, sse.Options{}, func(ctx context.Context, out *sse.Writer) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		if err := out.Send(sse.Event{ID: "7", Name: "multi", Data: []byte("one\r\ntwo\rthree\n"), Retry: 1250 * time.Millisecond}); err != nil {
			return err
		}
		return out.SendJSON("json", map[string]string{"text": "one\ntwo"})
	})
	// Get must return while the callback is still waiting: this proves the first
	// comment flushed response headers over a real network connection.
	resp := open(t, f.server.URL)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("unexpected initial response: %d %v", resp.StatusCode, resp.Header)
	}
	r := bufio.NewReader(resp.Body)
	if got := frame(t, r); got != ": connected\n\n" {
		t.Fatalf("initial frame = %q", got)
	}
	once.Do(func() { close(release) })
	if got := frame(t, r); got != "id: 7\nevent: multi\nretry: 1250\ndata: one\ndata: two\ndata: three\ndata: \n\n" {
		t.Fatalf("multiline frame = %q", got)
	}
	if got := frame(t, r); got != "event: json\ndata: {\"text\":\"one\\ntwo\"}\n\n" {
		t.Fatalf("JSON frame = %q", got)
	}
	if body, err := io.ReadAll(r); err != nil || len(body) != 0 {
		t.Fatalf("normal completion must have clean EOF: body=%q err=%v", body, err)
	}
	outcome(t, f.result, nil)
}

func TestRealHTTPHeartbeatAndClientDisconnect(t *testing.T) {
	// The idle interval deliberately exceeds WriteTimeout: each successful write
	// must disarm its deadline instead of treating idle time as a slow write.
	f := newFixture(t, sse.Options{WriteTimeout: 15 * time.Millisecond, HeartbeatInterval: 50 * time.Millisecond}, func(ctx context.Context, _ *sse.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	})
	resp := open(t, f.server.URL)
	r := bufio.NewReader(resp.Body)
	_ = frame(t, r)
	if got := frame(t, r); got != ": heartbeat\n\n" {
		t.Fatalf("heartbeat = %q", got)
	}
	_ = resp.Body.Close()
	outcome(t, f.result, context.Canceled)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.manager.Wait(ctx); err != nil || f.manager.Active() != 0 {
		t.Fatalf("client disconnect retained a connection: %v", err)
	}
}

func TestRealHTTPLifetimeAndAuthorizationRevocation(t *testing.T) {
	for _, kind := range []string{"duration", "manager-close", "revoke"} {
		t.Run(kind, func(t *testing.T) {
			var revoked atomic.Bool
			opts := sse.Options{MaxDuration: time.Second, HeartbeatInterval: 5 * time.Millisecond, RecheckInterval: 10 * time.Millisecond}
			want := error(context.Canceled)
			if kind == "duration" {
				opts.MaxDuration = 60 * time.Millisecond
				want = context.DeadlineExceeded
			}
			if kind == "revoke" {
				opts.Authorize = func(context.Context) error {
					if revoked.Load() {
						return errors.New("private authorization diagnostic")
					}
					return nil
				}
				want = sse.ErrUnauthorized
			}
			f := newFixture(t, opts, func(ctx context.Context, _ *sse.Writer) error {
				<-ctx.Done()
				return ctx.Err()
			})
			resp := open(t, f.server.URL)
			_ = frame(t, bufio.NewReader(resp.Body))
			if kind == "manager-close" {
				f.manager.Close()
			}
			revoked.Store(true)
			outcome(t, f.result, want)
			if f.manager.Active() != 0 {
				t.Fatal("completed stream retained capacity")
			}
		})
	}
}

func TestRevocationRetainsCapacityUntilCallbackReturns(t *testing.T) {
	var revoked atomic.Bool
	canceled, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f := newFixture(t, sse.Options{RecheckInterval: 5 * time.Millisecond, Authorize: func(context.Context) error {
		if revoked.Load() {
			return errors.New("revoked")
		}
		return nil
	}}, func(ctx context.Context, _ *sse.Writer) error {
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	})
	_ = open(t, f.server.URL)
	revoked.Store(true)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("callback did not observe revocation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if f.manager.Active() != 1 || !errors.Is(f.manager.Wait(ctx), context.DeadlineExceeded) {
		t.Fatal("revocation released capacity before callback stopped")
	}
	once.Do(func() { close(release) })
	outcome(t, f.result, sse.ErrUnauthorized)
}

func TestRealHTTPStreamBudgetOutlivesOrdinaryDeadlineAndPreservesValues(t *testing.T) {
	type key struct{}
	manager, _ := stream.NewManager(1)
	srv, _ := sse.New(manager, sse.Options{MaxDuration: time.Second})
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		original := stream.PreserveRequestContext(r.Context())
		ordinary, cancel := context.WithTimeout(original, 20*time.Millisecond)
		defer cancel()
		ctx := context.WithValue(ordinary, key{}, "kept")
		result <- srv.Serve(w, r.WithContext(ctx), func(ctx context.Context, out *sse.Writer) error {
			select {
			case <-time.After(70 * time.Millisecond):
				return out.SendJSON("late", ctx.Value(key{}))
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}))
	t.Cleanup(func() { manager.Close(); server.Close() })
	resp := open(t, server.URL)
	r := bufio.NewReader(resp.Body)
	_ = frame(t, r)
	if got := frame(t, r); got != "event: late\ndata: \"kept\"\n\n" {
		t.Fatalf("stream lost budget/value: %q", got)
	}
	outcome(t, result, nil)
}

func TestInvalidOptionsAndPrestreamRejections(t *testing.T) {
	manager, _ := stream.NewManager(1)
	defer manager.Close()
	for _, opts := range []sse.Options{{MaxDuration: -1}, {WriteTimeout: -1}, {HeartbeatInterval: -1}, {RecheckInterval: -1}, {MaxEventBytes: -1}, {MaxEventBytes: 1}} {
		if _, err := sse.New(manager, opts); !errors.Is(err, sse.ErrInvalidOptions) {
			t.Fatalf("options %+v: %v", opts, err)
		}
	}
	if _, err := sse.New(nil, sse.Options{}); !errors.Is(err, sse.ErrInvalidOptions) {
		t.Fatal("nil manager accepted")
	}
	for _, mode := range []string{"authorize", "authorize-panic", "closed", "capacity", "unsupported", "expired", "method"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := stream.NewManager(1)
			defer m.Close()
			opts := sse.Options{}
			wantStatus, wantErr := 403, error(sse.ErrUnauthorized)
			if strings.HasPrefix(mode, "authorize") {
				opts.Authorize = func(context.Context) error {
					if mode == "authorize-panic" {
						panic("super-private")
					}
					return errors.New("super-private")
				}
			}
			if mode == "closed" {
				m.Close()
				wantStatus, wantErr = 503, stream.ErrClosed
			}
			if mode == "capacity" {
				occupied, _ := m.Begin(context.Background(), time.Second)
				defer occupied.End()
				wantStatus, wantErr = 503, stream.ErrCapacity
			}
			if mode == "unsupported" {
				wantStatus, wantErr = 500, sse.ErrUnsupported
			}
			if mode == "expired" {
				wantStatus, wantErr = 408, context.DeadlineExceeded
			}
			if mode == "method" {
				wantStatus, wantErr = 405, sse.ErrInvalidOptions
			}
			srv, _ := sse.New(m, opts)
			results := make(chan error, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "expired" {
					ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(-time.Second))
					defer cancel()
					r = r.WithContext(ctx)
				}
				if mode == "method" {
					r.Method = http.MethodPost
				}
				if mode == "unsupported" {
					w = opaqueWriter{w}
				}
				results <- srv.Serve(w, r, func(context.Context, *sse.Writer) error {
					return errors.New("callback must not run")
				})
			}))
			defer s.Close()
			resp := open(t, s.URL)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != wantStatus || strings.Contains(string(body), "super-private") || strings.Contains(string(body), "callback") || strings.Contains(string(body), "data:") {
				t.Fatalf("unsafe rejection: %d %q", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), `"error":`) {
				t.Fatalf("rejection did not use shared envelope: %q", body)
			}
			outcome(t, results, wantErr)
		})
	}
}

// Intentionally hides Unwrap/deadline support, like an incompatible middleware.
type opaqueWriter struct{ http.ResponseWriter }

type secretMarshaler struct{}

func (secretMarshaler) MarshalJSON() ([]byte, error) { return nil, errors.New("super-private") }

func TestRealHTTPRejectsInvalidEventsWithoutInjectingFrames(t *testing.T) {
	checks := make(chan error, 1)
	f := newFixture(t, sse.Options{MaxEventBytes: 64}, func(_ context.Context, out *sse.Writer) error {
		invalid := []sse.Event{{ID: "a\nb"}, {Name: "x\ry"}, {ID: "a\x00b"}, {Data: []byte{0xff}}, {Retry: -1}, {Retry: time.Nanosecond}, {Retry: 1500 * time.Microsecond}}
		for _, event := range invalid {
			if err := out.Send(event); !errors.Is(err, sse.ErrInvalidEvent) {
				checks <- fmt.Errorf("invalid event returned %v", err)
				return nil
			}
		}
		for _, event := range []sse.Event{{Data: []byte(strings.Repeat("x", 65))}, {Data: []byte(strings.Repeat("\n", 12))}, {Name: strings.Repeat("x", 64)}} {
			if err := out.Send(event); !errors.Is(err, sse.ErrEventTooLarge) {
				checks <- fmt.Errorf("oversize event returned %v", err)
				return nil
			}
		}
		if err := out.SendJSON("private", secretMarshaler{}); !errors.Is(err, sse.ErrInvalidEvent) || strings.Contains(err.Error(), "super-private") {
			checks <- fmt.Errorf("unsafe marshaler classification: %v", err)
			return nil
		}
		if err := out.SendJSON("large", strings.Repeat("x", 65)); !errors.Is(err, sse.ErrEventTooLarge) {
			checks <- fmt.Errorf("oversize JSON returned %v", err)
			return nil
		}
		checks <- nil
		return out.SendJSON("ok", true)
	})
	resp := open(t, f.server.URL)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != ": connected\n\nevent: ok\ndata: true\n\n" {
		t.Fatalf("bad frames reached the wire: %q err=%v", body, err)
	}
	if err := <-checks; err != nil {
		t.Fatal(err)
	}
	outcome(t, f.result, nil)
}

func TestRealHTTPHandlerErrorsAndPanicNeverAppendJSON(t *testing.T) {
	for _, shouldPanic := range []bool{false, true} {
		t.Run(fmt.Sprint(shouldPanic), func(t *testing.T) {
			f := newFixture(t, sse.Options{}, func(context.Context, *sse.Writer) error {
				if shouldPanic {
					panic("super-private")
				}
				return errors.New("super-private")
			})
			resp := open(t, f.server.URL)
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != ": connected\n\n" {
				t.Fatalf("handler error leaked into stream: %q err=%v", body, err)
			}
			outcome(t, f.result, sse.ErrHandler)
		})
	}
}

func TestRealTCPSlowReaderWriteDeadline(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f := newFixture(t, sse.Options{WriteTimeout: 60 * time.Millisecond, MaxDuration: 3 * time.Second}, func(ctx context.Context, out *sse.Writer) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		data := []byte(strings.Repeat("x", 60<<10))
		for i := 0; i < 1024; i++ {
			if err := out.Send(sse.Event{Data: data}); err != nil {
				return err
			}
		}
		return errors.New("reader unexpectedly accepted all data")
	})
	conn := rawConnection(t, f.server.URL)
	once.Do(func() { close(release) })
	// No further reads: the small TCP receive window eventually fills. This tests
	// actual socket backpressure instead of a fake ResponseWriter's sleep behavior.
	outcome(t, f.result, sse.ErrWrite)
	_ = conn.Close()
}

func TestRealTCPCancellationInterruptsWriteAndWaitingSender(t *testing.T) {
	release, second, secondStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	observation := &networkWriteObservation{}
	var releaseOnce, secondOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		secondOnce.Do(func() { close(second) })
	})
	senders := make(chan error, 2)
	f := newFixtureWithWriter(t, sse.Options{MaxEventBytes: 512 << 10, WriteTimeout: 3 * time.Second, MaxDuration: 8 * time.Second}, func(ctx context.Context, out *sse.Writer) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			// Reuse a small payload so -race encoding work cannot be mistaken for
			// holding the write gate. The finite 256 MiB ceiling fills TCP buffers
			// across platforms without assuming one particular socket buffer size.
			data := []byte(strings.Repeat("x", 256<<10))
			for i := 0; i < 1024; i++ {
				if err := out.Send(sse.Event{Data: data}); err != nil {
					senders <- err
					return
				}
			}
			senders <- errors.New("non-reading TCP client unexpectedly accepted 256 MiB")
		}()
		go func() {
			defer workers.Done()
			select {
			case <-ctx.Done():
				senders <- ctx.Err()
				return
			case <-second:
			}
			close(secondStarted)
			senders <- out.SendJSON("queued", true)
		}()
		workers.Wait()
		return ctx.Err()
	}, func(w http.ResponseWriter) http.ResponseWriter {
		return &observedNetworkWriter{ResponseWriter: w, observation: observation}
	})
	_ = rawConnection(t, f.server.URL)
	releaseOnce.Do(func() { close(release) })
	// Synchronize with an actual socket Write/Flush that has not returned. A
	// signal before Send would also include encoding and waiting for scheduling.
	blockedSequence := awaitBlockedNetworkWrite(t, observation)
	secondOnce.Do(func() { close(second) })
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second sender did not start")
	}
	<-time.After(15 * time.Millisecond)
	if observation.finished.Load() >= blockedSequence {
		t.Fatal("observed socket write completed before cancellation")
	}
	// The first sender owns the gate inside the observed network operation, so
	// the second sender must wait. Cancellation must stop both before 3s expires.
	started := time.Now()
	f.manager.Close()
	outcome(t, f.result, context.Canceled)
	if time.Since(started) > time.Second {
		t.Fatal("cancellation waited for the full write timeout")
	}
	for i := 0; i < 2; i++ {
		if err := <-senders; !errors.Is(err, context.Canceled) {
			t.Fatalf("sender %d error = %v", i, err)
		}
	}
}

// Observation adds no fake delays or simulated I/O. Both methods delegate to the
// real net/http writer; Unwrap lets ResponseController reach the socket deadline.
type networkWriteObservation struct {
	entered  atomic.Uint64
	finished atomic.Uint64
}

type observedNetworkWriter struct {
	http.ResponseWriter
	observation *networkWriteObservation
}

func (w *observedNetworkWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedNetworkWriter) Write(p []byte) (int, error) {
	sequence := w.observation.entered.Add(1)
	defer w.observation.finished.Store(sequence)
	return w.ResponseWriter.Write(p)
}

func (w *observedNetworkWriter) FlushError() error {
	sequence := w.observation.entered.Add(1)
	defer w.observation.finished.Store(sequence)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func awaitBlockedNetworkWrite(t *testing.T, observation *networkWriteObservation) uint64 {
	t.Helper()
	budget := time.NewTimer(2 * time.Second)
	defer budget.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	var candidate uint64
	var observedAt time.Time
	for {
		select {
		case <-budget.C:
			t.Fatal("TCP backpressure did not produce an observable blocked write")
		case <-poll.C:
			entered, finished := observation.entered.Load(), observation.finished.Load()
			if entered > finished && entered == candidate {
				if time.Since(observedAt) >= 80*time.Millisecond {
					return entered
				}
				continue
			}
			candidate, observedAt = entered, time.Now()
		}
	}
}

func rawConnection(t *testing.T, url string) *net.TCPConn {
	t.Helper()
	address, err := net.ResolveTCPAddr("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTCP("tcp", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadBuffer(1024)
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", address); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			return conn
		}
	}
}
