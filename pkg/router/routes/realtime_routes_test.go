package routes_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"healgo-scaffold/internal/testutil"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"healgo-scaffold/configs"
	"healgo-scaffold/internal/auth"
	"healgo-scaffold/internal/middleware"
	"healgo-scaffold/internal/settings"
	"healgo-scaffold/internal/stream"
	"healgo-scaffold/pkg/bootstrap"
	frameworkrouter "healgo-scaffold/pkg/router"
	"healgo-scaffold/pkg/router/routes"
	"healgo-scaffold/pkg/serve/controller"
	"healgo-scaffold/pkg/wire"

	"github.com/coder/websocket"
	"gorm.io/gorm"
)

type safeLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeLog) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}
func (b *safeLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func isolatedDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	database := testutil.OpenDatabase(t)
	if err := database.AutoMigrate(append(auth.Models(), settings.Models()...)...); err != nil {
		t.Fatal("initialize isolated test tables")
	}
	return database
}

type httpFixture struct {
	server  *httptest.Server
	auth    *auth.Service
	manager *stream.Manager
	token   string
	denied  string
	logs    *safeLog
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()
	database := isolatedDatabase(t)
	ctx := context.Background()
	authentication, err := auth.New(database, configs.Auth{SessionTTL: time.Hour, MaxSessions: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authentication.Close() })
	const password = "realtime-test-password-736d4"
	if err := authentication.Bootstrap(ctx, "admin", password); err != nil {
		t.Fatal(err)
	}
	if _, err := authentication.CreateUser(ctx, "denied", password, nil); err != nil {
		t.Fatal(err)
	}
	token, _, err := authentication.Login(ctx, "admin", password)
	if err != nil {
		t.Fatal(err)
	}
	denied, _, err := authentication.Login(ctx, "denied", password)
	if err != nil {
		t.Fatal(err)
	}
	configuration, _ := settings.New(database, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)), nil)
	manager, err := stream.NewManager(16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.Close()
		wait, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Wait(wait); err != nil {
			t.Error("realtime sessions did not stop")
		}
	})
	logs := &safeLog{}
	resources := bootstrap.Resources{DB: database, Auth: authentication, Settings: configuration, Streams: manager,
		Logger: slog.New(slog.NewJSONHandler(logs, nil))}
	origins := []string{"https://console.example"}
	realtime, err := controller.NewRealtimeController(authentication, manager, origins)
	if err != nil {
		t.Fatal(err)
	}
	cfg := configs.Config{App: configs.App{Name: "realtime-test"}, HTTP: configs.HTTP{RequestTimeout: 250 * time.Millisecond},
		Auth:     configs.Auth{SessionTTL: time.Hour, LoginAttemptsPerMinute: 100},
		Security: configs.Security{MaxBodyBytes: 1024, CORSOrigins: origins}}
	container := &wire.Container{Config: cfg, Resources: resources,
		SystemController:   controller.NewSystemController(authentication, configuration, int64(cfg.Auth.SessionTTL/time.Second)),
		RealtimeController: realtime}
	engine, err := frameworkrouter.NewEngine(container)
	if err != nil {
		t.Fatal(err)
	}
	if err := frameworkrouter.New(engine, container); err != nil {
		t.Fatal(err)
	}
	// Register the concrete route functions after composing dependencies.
	// External routes_test avoids a controller → router test import cycle.
	public := engine.Group("/api/v1")
	protected := engine.Group("/api/v1", middleware.Authenticate(authentication))
	routes.RegisterRealtimeRoutes(container, public, protected)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	return &httpFixture{server: server, auth: authentication, manager: manager, token: token, denied: denied, logs: logs}
}

func (f *httpFixture) http(t *testing.T, method, path, token string, status int) *http.Response {
	t.Helper()
	r, _ := http.NewRequest(method, f.server.URL+path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(r)
	if err != nil {
		t.Fatal("realtime HTTP request failed", err)
	}
	if response.StatusCode != status {
		_ = response.Body.Close()
		t.Fatalf("%s %s status=%d want=%d", method, path, response.StatusCode, status)
	}
	return response
}

func (f *httpFixture) ticket(t *testing.T) string {
	t.Helper()
	r := f.http(t, "POST", "/api/v1/examples/realtime/tickets", f.token, 201)
	defer r.Body.Close()
	var body struct {
		Data struct {
			Ticket  string `json:"ticket"`
			Expires int    `json:"expires_in"`
		} `json:"data"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Data.Ticket) != 43 || body.Data.Expires != 30 {
		t.Fatal("ticket endpoint contract invalid")
	}
	return body.Data.Ticket
}

func TestSSEAuthenticationAndFirstFrames(t *testing.T) {
	f := newHTTPFixture(t)
	for _, test := range []struct {
		token  string
		status int
	}{{"", 401}, {f.denied, 403}} {
		r := f.http(t, "GET", "/api/v1/examples/realtime/sse", test.token, test.status)
		_ = r.Body.Close()
		r = f.http(t, "POST", "/api/v1/examples/realtime/tickets", test.token, test.status)
		_ = r.Body.Close()
	}
	r := f.http(t, "GET", "/api/v1/examples/realtime/sse", f.token, 200)
	defer r.Body.Close()
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("SSE media type missing")
	}
	reader := bufio.NewReader(r.Body)
	var content strings.Builder
	for !strings.Contains(content.String(), `"sequence":2`) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal("stream stopped before second tick; ordinary HTTP timeout may have leaked", err)
		}
		content.WriteString(line)
	}
	if !strings.Contains(content.String(), "event: tick") || !strings.Contains(content.String(), `"sequence":1`) {
		t.Fatal("SSE first frames invalid")
	}
}

func TestWebSocketTicketBearerOriginEchoAndRevocation(t *testing.T) {
	f := newHTTPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint := "ws" + strings.TrimPrefix(f.server.URL, "http") + "/api/v1/examples/realtime/ws"
	assertRejected := func(target, token, origin string, status int) {
		t.Helper()
		headers := make(http.Header)
		if token != "" {
			headers.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			headers.Set("Origin", origin)
		}
		connection, response, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: headers})
		if connection != nil {
			_ = connection.CloseNow()
		}
		if response != nil && response.Body != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != status {
			t.Fatalf("WebSocket rejection status mismatch want=%d", status)
		}
	}
	assertRejected(endpoint, "", "", 401)
	assertRejected(endpoint, f.denied, "", 403)
	assertRejected(endpoint+"?token=ignored", f.token, "", 400)
	ticket := f.ticket(t)
	target := endpoint + "?ticket=" + url.QueryEscape(ticket)
	assertRejected(target, "", "https://forbidden.example", 403)
	connection, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{f.server.URL}}})
	if err != nil {
		t.Fatal("same-origin browser ticket handshake failed", err)
	}
	defer connection.CloseNow()
	payload := []byte(`{"hello":"实时连接"}`)
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal("send browser WebSocket message failed")
	}
	kind, echoed, err := connection.Read(ctx)
	if err != nil || kind != websocket.MessageText || !bytes.Equal(echoed, payload) {
		t.Fatal("WebSocket echo failed")
	}
	assertRejected(target, "", f.server.URL, 401)
	_ = connection.CloseNow()
	cli, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + f.token}}})
	if err != nil {
		t.Fatal("bearer WebSocket client handshake failed", err)
	}
	_ = cli.CloseNow()
	unused := f.ticket(t)
	if err := f.auth.Logout(f.token); err != nil {
		t.Fatal(err)
	}
	assertRejected(endpoint+"?ticket="+unused, "", f.server.URL, 401)
	for _, secret := range []string{f.token, f.denied, ticket, unused} {
		if strings.Contains(f.logs.String(), secret) {
			t.Fatal("session or ticket leaked in access logs")
		}
	}
}
