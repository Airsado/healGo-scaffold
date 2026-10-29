package websocket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"healgo-scaffold/internal/response"
	"healgo-scaffold/internal/stream"

	coder "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

type harness struct {
	manager *stream.Manager
	server  *httptest.Server
	errors  chan error
}

func start(t *testing.T, opts Options, handler func(context.Context, *Conn) error, wrap func(http.Handler) http.Handler) *harness {
	t.Helper()
	manager, err := stream.NewManager(4)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(manager, opts)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{manager: manager, errors: make(chan error, 32)}
	var endpoint http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.errors <- server.Serve(w, r, handler)
	})
	if wrap != nil {
		endpoint = wrap(endpoint)
	}
	h.server = httptest.NewUnstartedServer(endpoint)
	h.server.Start()
	t.Cleanup(func() {
		manager.Close()
		h.server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Wait(ctx); err != nil {
			t.Errorf("connection cleanup: %v", err)
		}
	})
	return h
}

func (h *harness) dial(t *testing.T, opts *coder.DialOptions) *coder.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, response, err := coder.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http"), opts)
	if err != nil {
		if response != nil {
			t.Fatalf("dial status %d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func receive(t *testing.T, channel <-chan error) error {
	t.Helper()
	select {
	case err := <-channel:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server callback")
		return nil
	}
}

func readClient(t *testing.T, conn *coder.Conn) (coder.MessageType, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return conn.Read(ctx)
}

func echo(ctx context.Context, conn *Conn) error {
	for {
		typ, payload, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if err := conn.Write(ctx, typ, payload); err != nil {
			return err
		}
	}
}

func TestOptionsDefaultsAndValidation(t *testing.T) {
	manager, _ := stream.NewManager(1)
	server, err := New(manager, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if server.opts.MaxDuration != 5*time.Minute || server.opts.WriteTimeout != 5*time.Second ||
		server.opts.PingInterval != 20*time.Second || server.opts.PongTimeout != 10*time.Second ||
		server.opts.RecheckInterval != 30*time.Second || server.opts.MaxMessageBytes != 64<<10 {
		t.Fatal("unexpected default budgets")
	}
	invalid := []Options{
		{MaxDuration: -1}, {WriteTimeout: -1}, {PingInterval: -1}, {PongTimeout: -1},
		{RecheckInterval: -1}, {MaxMessageBytes: -1}, {MaxMessageBytes: math.MaxInt64},
		{AllowedOrigins: []string{"*"}}, {AllowedOrigins: []string{"https://*.example.com"}},
		{AllowedOrigins: []string{"https://example.com/path"}},
	}
	for _, opts := range invalid {
		if _, err := New(manager, opts); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("accepted invalid options: %+v", opts)
		}
	}
	if _, err := New(nil, Options{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatal("accepted nil manager")
	}
	origins := []string{"https://console.example"}
	server, err = New(manager, Options{AllowedOrigins: origins})
	if err != nil {
		t.Fatal(err)
	}
	origins[0] = "https://attacker.example"
	if server.opts.AllowedOrigins[0] != "https://console.example" {
		t.Fatal("retained caller-owned origin slice")
	}
}

func TestTextBinaryAndJSONEcho(t *testing.T) {
	h := start(t, Options{}, echo, nil)
	conn := h.dial(t, nil)
	for _, message := range []struct {
		typ     coder.MessageType
		payload []byte
	}{
		{coder.MessageText, []byte("你好，Gin")},
		{coder.MessageBinary, []byte{0, 255, 1}},
	} {
		if err := conn.Write(context.Background(), message.typ, message.payload); err != nil {
			t.Fatal(err)
		}
		typ, payload, err := readClient(t, conn)
		if err != nil || typ != message.typ || string(payload) != string(message.payload) {
			t.Fatalf("echo mismatch: %v %q %v", typ, payload, err)
		}
	}
	_ = conn.CloseNow()
	if err := receive(t, h.errors); !errors.Is(err, ErrClosed) {
		t.Fatalf("disconnect: %v", err)
	}

	jsonServer := start(t, Options{}, func(ctx context.Context, conn *Conn) error {
		var input struct {
			Name string `json:"name"`
		}
		if err := conn.ReadJSON(ctx, &input); err != nil {
			return err
		}
		return conn.WriteJSON(ctx, input)
	}, nil)
	jsonConn := jsonServer.dial(t, nil)
	if err := jsonConn.Write(context.Background(), coder.MessageText, []byte(`{"name":"demo"}`)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := readClient(t, jsonConn)
	if err != nil || string(payload) != `{"name":"demo"}` {
		t.Fatalf("JSON echo: %q %v", payload, err)
	}
	if err := receive(t, jsonServer.errors); err != nil {
		t.Fatal(err)
	}
}

func TestLocalWriteValidationDoesNotSendInvalidFrames(t *testing.T) {
	checks := make(chan error, 1)
	h := start(t, Options{MaxMessageBytes: 16}, func(ctx context.Context, conn *Conn) error {
		for _, check := range []struct{ actual, want error }{
			{conn.Write(ctx, MessageType(99), []byte("secret")), ErrInvalidMessage},
			{conn.Write(ctx, MessageText, []byte{255}), ErrInvalidMessage},
			{conn.Write(ctx, MessageBinary, make([]byte, 17)), ErrMessageTooBig},
			{conn.WriteJSON(ctx, math.Inf(1)), ErrInvalidMessage},
		} {
			if !errors.Is(check.actual, check.want) {
				checks <- fmt.Errorf("validation got %v want %v", check.actual, check.want)
				return ErrHandler
			}
		}
		checks <- nil
		return conn.WriteJSON(ctx, map[string]bool{"ok": true})
	}, nil)
	conn := h.dial(t, nil)
	_, payload, err := readClient(t, conn)
	if err != nil || string(payload) != `{"ok":true}` {
		t.Fatalf("invalid frame was sent or connection closed: %q %v", payload, err)
	}
	if err := receive(t, checks); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, h.errors); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingLimitsAndValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		typ     coder.MessageType
		payload []byte
		json    bool
		want    error
	}{
		{"oversize", coder.MessageBinary, make([]byte, 17), false, ErrMessageTooBig},
		{"invalid_utf8", coder.MessageText, []byte{255}, false, ErrInvalidMessage},
		{"invalid_json", coder.MessageText, []byte(`{"secret":`), true, ErrInvalidMessage},
		{"binary_json", coder.MessageBinary, []byte(`{}`), true, ErrInvalidMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := start(t, Options{MaxMessageBytes: 16}, func(ctx context.Context, conn *Conn) error {
				if test.json {
					var value any
					return conn.ReadJSON(ctx, &value)
				}
				_, _, err := conn.Read(ctx)
				return err
			}, nil)
			conn := h.dial(t, nil)
			_ = conn.Write(context.Background(), test.typ, test.payload)
			_, _, _ = readClient(t, conn)
			if err := receive(t, h.errors); !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
		})
	}
}

func TestExactOriginsAndNoForwardedTrust(t *testing.T) {
	for _, test := range []struct {
		name    string
		origins []string
		allowed bool
	}{
		{"no_origin", nil, true},
		{"same_origin", []string{"same"}, true},
		{"default_port", []string{"https://console.example"}, true},
		{"ipv6", []string{"http://[::1]:5173"}, true},
		{"scheme_mismatch", []string{"http://console.example"}, false},
		{"subdomain", []string{"https://evil.console.example"}, false},
		{"duplicate_origin", []string{"https://console.example", "https://console.example"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := start(t, Options{AllowedOrigins: []string{"https://console.example:443", "http://[::1]:5173"}}, echo, nil)
			headers := make(http.Header)
			for _, origin := range test.origins {
				if origin == "same" {
					origin = h.server.URL
				}
				headers.Add("Origin", origin)
			}
			headers.Set("X-Forwarded-Host", "console.example")
			headers.Set("X-Forwarded-Proto", "https")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, response, err := coder.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http"), &coder.DialOptions{HTTPHeader: headers})
			if test.allowed {
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.CloseNow()
				_ = receive(t, h.errors)
			} else {
				if err == nil {
					_ = conn.CloseNow()
					t.Fatal("forbidden origin accepted")
				}
				if response == nil || response.StatusCode != http.StatusForbidden {
					t.Fatalf("unexpected response: %v", response)
				}
				if err := receive(t, h.errors); !errors.Is(err, ErrOriginForbidden) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPreUpgradeErrorsAndExpiredRequests(t *testing.T) {
	manager, _ := stream.NewManager(1)
	server, _ := New(manager, Options{})
	handler := func(context.Context, *Conn) error { t.Error("unexpected upgrade"); return nil }
	for _, test := range []struct {
		name   string
		method string
		status int
		want   error
	}{
		{"method", http.MethodPost, http.StatusMethodNotAllowed, ErrInvalidOptions},
		{"unsupported", http.MethodGet, http.StatusNotImplemented, ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			err := server.Serve(recorder, httptest.NewRequest(test.method, "http://localhost/ws", nil), handler)
			if !errors.Is(err, test.want) || recorder.Code != test.status || !strings.Contains(recorder.Body.String(), `"error"`) {
				t.Fatalf("response %d %q: %v", recorder.Code, recorder.Body.String(), err)
			}
		})
	}
	base := stream.PreserveRequestContext(context.Background())
	ctx, cancel := context.WithCancel(base)
	cancel()
	recorder := httptest.NewRecorder()
	err := server.Serve(recorder, httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil).WithContext(ctx), handler)
	if !errors.Is(err, context.Canceled) || manager.Active() != 0 {
		t.Fatalf("expired request revived: %v", err)
	}
	assertExpiredEnvelope(t, recorder)
}

func assertExpiredEnvelope(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid error envelope: %v", err)
	}
	if recorder.Code != http.StatusRequestTimeout || envelope.Error == nil || envelope.Error.Code != "stream_request_expired" || envelope.TimeStamp == 0 {
		t.Fatalf("unexpected expired response: %d %s", recorder.Code, recorder.Body.String())
	}
}

type preflightRecorder struct{ *httptest.ResponseRecorder }

func (w preflightRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, ErrUnavailable
}

func TestAdmissionAndAuthorizationExpirationReturn408(t *testing.T) {
	for _, phase := range []string{"admission", "authorization"} {
		t.Run(phase, func(t *testing.T) {
			manager, _ := stream.NewManager(1)
			opts := Options{}
			request := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
			if phase == "admission" {
				original, cancel := context.WithCancel(request.Context())
				preserved := stream.PreserveRequestContext(original)
				cancel()
				// Simulate a live budget wrapper whose preserved parent was canceled.
				request = request.WithContext(context.WithoutCancel(preserved))
			} else {
				opts.MaxDuration = 10 * time.Millisecond
				opts.Authorize = func(ctx context.Context) error { <-ctx.Done(); return nil }
			}
			server, err := New(manager, opts)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			err = server.Serve(preflightRecorder{recorder}, request, func(context.Context, *Conn) error {
				t.Error("expired request reached handler")
				return nil
			})
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expiration cause lost: %v", err)
			}
			assertExpiredEnvelope(t, recorder)
			if manager.Active() != 0 {
				t.Fatal("expired admission leaked capacity")
			}
		})
	}
}

func TestShutdownClosesHijackedSocketAndWaitsForCallback(t *testing.T) {
	readDone := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	h := start(t, Options{}, func(_ context.Context, conn *Conn) error {
		_, _, err := conn.Read(context.Background())
		close(readDone)
		<-release
		return err
	}, nil)
	t.Cleanup(unblock)
	conn := h.dial(t, nil)
	h.manager.Close()
	if _, _, err := readClient(t, conn); err == nil {
		t.Fatal("shutdown left hijacked socket open")
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not unblock server read")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.manager.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) || h.manager.Active() != 1 {
		t.Fatalf("capacity released before callback returned: active=%d err=%v", h.manager.Active(), err)
	}
	unblock()
	if err := receive(t, h.errors); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestHeartbeatAndMaximumDuration(t *testing.T) {
	t.Run("reading_client_processes_pong", func(t *testing.T) {
		h := start(t, Options{PingInterval: 20 * time.Millisecond, PongTimeout: 100 * time.Millisecond, MaxDuration: 250 * time.Millisecond}, echo, nil)
		conn := h.dial(t, nil)
		clientRead := make(chan error, 1)
		go func() { _, _, err := conn.Read(context.Background()); clientRead <- err }()
		select {
		case err := <-h.errors:
			t.Fatalf("heartbeat prematurely closed healthy connection: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		if err := receive(t, h.errors); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("maximum duration: %v", err)
		}
		if err := receive(t, clientRead); err == nil {
			t.Fatal("maximum duration left client open")
		}
	})
	t.Run("client_without_reader_times_out", func(t *testing.T) {
		h := start(t, Options{PingInterval: 20 * time.Millisecond, PongTimeout: 30 * time.Millisecond}, echo, nil)
		_ = h.dial(t, nil)
		if err := receive(t, h.errors); !errors.Is(err, ErrClosed) {
			t.Fatalf("missing pong: %v", err)
		}
	})
}

func TestAuthorizationFailureRevocationAndPanic(t *testing.T) {
	for _, panicCheck := range []bool{false, true} {
		t.Run(fmt.Sprintf("initial_panic_%v", panicCheck), func(t *testing.T) {
			h := start(t, Options{Authorize: func(context.Context) error {
				if panicCheck {
					panic("secret-auth-value")
				}
				return errors.New("secret-auth-value")
			}}, echo, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, response, err := coder.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http"), nil)
			if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
				t.Fatalf("initial authorization: response=%v err=%v", response, err)
			}
			if err := receive(t, h.errors); err != ErrUnauthorized || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe authorization error: %v", err)
			}
		})
	}
	for _, panicCheck := range []bool{false, true} {
		t.Run(fmt.Sprintf("revocation_panic_%v", panicCheck), func(t *testing.T) {
			var revoked atomic.Bool
			h := start(t, Options{RecheckInterval: 10 * time.Millisecond, Authorize: func(context.Context) error {
				if revoked.Load() {
					if panicCheck {
						panic("secret-revocation-value")
					}
					return errors.New("secret-revocation-value")
				}
				return nil
			}}, func(_ context.Context, conn *Conn) error {
				_, _, err := conn.Read(context.Background())
				return err
			}, nil)
			conn := h.dial(t, nil)
			revoked.Store(true)
			_, _, _ = readClient(t, conn)
			if err := receive(t, h.errors); err != ErrUnauthorized {
				t.Fatalf("authorization cause lost: %v", err)
			}
		})
	}
}

func TestSafeHandlerAndRemoteCloseErrors(t *testing.T) {
	for _, panicHandler := range []bool{false, true} {
		t.Run(fmt.Sprintf("handler_panic_%v", panicHandler), func(t *testing.T) {
			h := start(t, Options{}, func(context.Context, *Conn) error {
				if panicHandler {
					panic("secret-handler-value")
				}
				return errors.New("secret-handler-value")
			}, nil)
			conn := h.dial(t, nil)
			_, _, _ = readClient(t, conn)
			if err := receive(t, h.errors); err != ErrHandler || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe handler error: %v", err)
			}
		})
	}
	h := start(t, Options{}, echo, nil)
	conn := h.dial(t, nil)
	_ = conn.Close(coder.StatusNormalClosure, "secret-close-value")
	if err := receive(t, h.errors); err != ErrClosed || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe remote reason: %v", err)
	}
}

func TestInheritedHTTPDeadlineDoesNotKillWebSocket(t *testing.T) {
	h := start(t, Options{}, echo, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := stream.PreserveRequestContext(r.Context())
			ctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
			defer cancel()
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(70 * time.Millisecond))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	conn := h.dial(t, nil)
	<-time.After(140 * time.Millisecond)
	if err := conn.Write(context.Background(), coder.MessageText, []byte("after HTTP deadline")); err != nil {
		t.Fatal(err)
	}
	_, payload, err := readClient(t, conn)
	if err != nil || string(payload) != "after HTTP deadline" {
		t.Fatalf("inherited deadline was not cleared: %q %v", payload, err)
	}
	_ = conn.CloseNow()
	_ = receive(t, h.errors)
}

func TestRequestCancellationClosesSocket(t *testing.T) {
	cancelRequest := make(chan context.CancelFunc, 1)
	h := start(t, Options{}, echo, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			cancelRequest <- cancel
			next.ServeHTTP(w, r.WithContext(stream.PreserveRequestContext(ctx)))
		})
	})
	conn := h.dial(t, nil)
	(<-cancelRequest)()
	if _, _, err := readClient(t, conn); err == nil {
		t.Fatal("request cancellation left socket open")
	}
	if err := receive(t, h.errors); !errors.Is(err, context.Canceled) {
		t.Fatalf("request cancellation: %v", err)
	}
}

func TestGinUpgradeFlushesHandshake(t *testing.T) {
	h := start(t, Options{}, echo, func(next http.Handler) http.Handler {
		router := gin.New()
		router.GET("/", func(c *gin.Context) { next.ServeHTTP(c.Writer, c.Request) })
		return router
	})
	conn := h.dial(t, nil)
	if err := conn.Write(context.Background(), coder.MessageText, []byte("gin")); err != nil {
		t.Fatal(err)
	}
	_, payload, err := readClient(t, conn)
	if err != nil || string(payload) != "gin" {
		t.Fatalf("Gin upgrade: %q %v", payload, err)
	}
	_ = conn.CloseNow()
	_ = receive(t, h.errors)
}

func TestAuthorizationChecksHaveFiniteBudget(t *testing.T) {
	checkDone := make(chan error, 1)
	h := start(t, Options{WriteTimeout: 20 * time.Millisecond, Authorize: func(ctx context.Context) error {
		<-ctx.Done()
		checkDone <- ctx.Err()
		return ctx.Err()
	}}, echo, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, response, err := coder.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http"), nil)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("authorization timeout: %v %v", response, err)
	}
	if err := receive(t, checkDone); err != context.DeadlineExceeded {
		t.Fatalf("authorization callback received no deadline: %v", err)
	}
	if err := receive(t, h.errors); err != ErrUnauthorized {
		t.Fatalf("authorization timeout leaked detail: %v", err)
	}
}

func TestSlowClientWriteHasFiniteBudget(t *testing.T) {
	writeResult := make(chan error, 1)
	h := start(t, Options{MaxMessageBytes: 8 << 20, WriteTimeout: 40 * time.Millisecond}, func(ctx context.Context, conn *Conn) error {
		payload := make([]byte, 8<<20)
		// Windows may accept the first large WSASend into kernel-managed buffers.
		// Keep sending bounded messages until TCP backpressure reaches a later write.
		for range 64 {
			if err := conn.Write(ctx, MessageBinary, payload); err != nil {
				writeResult <- err
				return err
			}
		}
		writeResult <- nil
		return nil
	}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(smallBufferWriter{w}, r)
		})
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetReadBuffer(1024)
			}
		}
		return conn, err
	}}
	t.Cleanup(transport.CloseIdleConnections)
	_ = h.dial(t, &coder.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err := receive(t, writeResult); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow write did not expire: %v", err)
	}
	_ = receive(t, h.errors)
}

type smallBufferWriter struct{ http.ResponseWriter }

func (w smallBufferWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w smallBufferWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(1024)
		}
	}
	return conn, buffer, err
}

func TestCapacityAndClosedManagerRejectBeforeUpgrade(t *testing.T) {
	h := start(t, Options{}, echo, nil)
	sessions := make([]*stream.Session, 0, 4)
	for range 4 {
		session, err := h.manager.Begin(context.Background(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session)
	}
	t.Cleanup(func() {
		for _, session := range sessions {
			session.End()
		}
	})
	for _, closed := range []bool{false, true} {
		if closed {
			h.manager.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, response, err := coder.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http"), nil)
		cancel()
		if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("capacity was not rejected: %v %v", response, err)
		}
		if response.Body != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		want := stream.ErrCapacity
		if closed {
			want = stream.ErrClosed
		}
		if err := receive(t, h.errors); err != want {
			t.Fatalf("capacity error: %v", err)
		}
	}
}
