package bootstrap_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"heago-scaffold/configs"
	"heago-scaffold/internal/middleware"
	"heago-scaffold/internal/stream"
	"heago-scaffold/internal/stream/sse"
	streamws "heago-scaffold/internal/stream/websocket"
	"heago-scaffold/pkg/bootstrap"

	clientws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// Real Runtime testing matters here: httptest.Server has no WriteTimeout by
// default and would hide the old absolute HTTP deadlines inherited by hijacks.
func TestRuntimeDrainsBothProtocolsBeforeCleanupAndKeepsHTTPBudget(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	manager, _ := stream.NewManager(4)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := gin.New()
	engine.Use(middleware.RequestID(), middleware.Recovery(logger), middleware.Limits(1024, 25*time.Millisecond))
	engine.GET("/ready", func(c *gin.Context) { c.Status(204) })
	engine.GET("/ordinary", func(c *gin.Context) { <-c.Request.Context().Done(); c.Status(408) })
	sseServer, err := sse.New(manager, sse.Options{HeartbeatInterval: 10 * time.Millisecond, WriteTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	wsServer, err := streamws.New(manager, streamws.Options{PingInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var callbacks atomic.Int32
	engine.GET("/events", func(c *gin.Context) {
		_ = sseServer.Serve(c.Writer, c.Request, func(ctx context.Context, out *sse.Writer) error {
			callbacks.Add(1)
			defer callbacks.Add(-1)
			if err := out.SendJSON("ready", true); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		})
	})
	engine.GET("/socket", func(c *gin.Context) {
		_ = wsServer.Serve(c.Writer, c.Request, func(ctx context.Context, connection *streamws.Conn) error {
			callbacks.Add(1)
			defer callbacks.Add(-1)
			for {
				kind, payload, err := connection.Read(ctx)
				if err != nil {
					return err
				}
				if err := connection.Write(ctx, kind, payload); err != nil {
					return err
				}
			}
		})
	})
	var cleaned atomic.Bool
	runtime := bootstrap.NewRuntime(bootstrap.Components{
		Config: configs.Config{App: configs.App{Name: "stream-runtime-test"}, HTTP: configs.HTTP{
			Addr: address, ReadHeaderTimeout: time.Second, ReadTimeout: 55 * time.Millisecond,
			WriteTimeout: 55 * time.Millisecond, IdleTimeout: time.Second, ShutdownTimeout: time.Second,
		}},
		Resources: bootstrap.Resources{Streams: manager, Logger: logger}, Router: engine,
	}, func() { cleaned.Store(true) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- runtime.Serve(ctx) }()
	defer runtime.Close()
	// Closing the test transports is a failure-path escape hatch; success relies
	// on Runtime shutting them down before these deferred client closes execute.
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	base := "http://" + address
	readyBy := time.Now().Add(2 * time.Second)
	for {
		res, err := client.Get(base + "/ready")
		if err == nil {
			_ = res.Body.Close()
			break
		}
		if time.Now().After(readyBy) {
			t.Fatal("runtime never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ordinary, err := client.Get(base + "/ordinary")
	if err != nil {
		t.Fatal(err)
	}
	_ = ordinary.Body.Close()
	if ordinary.StatusCode != 408 {
		t.Fatal("ordinary HTTP lost its request budget")
	}
	events, err := client.Get(base + "/events")
	if err != nil || events.StatusCode != 200 {
		t.Fatal("open SSE", err)
	}
	defer events.Body.Close()
	reader := bufio.NewReader(events.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal("read initial SSE event", err)
		}
		if line == "data: true\n" {
			break
		}
	}
	ioCtx, ioCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer ioCancel()
	socket, _, err := clientws.Dial(ioCtx, "ws://"+address+"/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	// Both the ordinary request budget and the server's absolute I/O deadlines
	// expire during this interval; explicit streams must remain usable.
	time.Sleep(100 * time.Millisecond)
	if err := socket.Write(ioCtx, clientws.MessageText, []byte("after-http-timeout")); err != nil {
		t.Fatal("WebSocket inherited ordinary HTTP deadline", err)
	}
	_, echoed, err := socket.Read(ioCtx)
	if err != nil || string(echoed) != "after-http-timeout" {
		t.Fatal("WebSocket failed after ordinary timeout", err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal("SSE inherited ordinary HTTP deadline", err)
	}
	if manager.Active() != 2 || callbacks.Load() != 2 {
		t.Fatal("active transports not tracked")
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal("graceful streaming shutdown failed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not drain SSE and hijacked WebSocket")
	}
	if manager.Active() != 0 || callbacks.Load() != 0 || cleaned.Load() {
		t.Fatal("resources were released before callbacks ended")
	}
	runtime.Close()
	if !cleaned.Load() {
		t.Fatal("runtime cleanup was not called")
	}
	if _, _, err := socket.Read(ioCtx); err == nil {
		t.Fatal("hijacked connection survived shutdown")
	}
	// SSE may finish with EOF or an interrupted chunk when its cancellation
	// forces a blocked writer to stop; either must terminate the client read.
	_, _ = io.Copy(io.Discard, reader)
}

func TestRecoveryDoesNotAppendJSONToCommittedResponse(t *testing.T) {
	engine := gin.New()
	engine.Use(middleware.Recovery(slog.New(slog.NewTextHandler(io.Discard, nil))))
	engine.GET("/partial", func(c *gin.Context) {
		_, _ = c.Writer.WriteString("already-streaming")
		panic("private-panic-payload")
	})
	server := &http.Server{Handler: engine}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go server.Serve(listener)
	res, err := http.Get("http://" + listener.Addr().String() + "/partial")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if strings.TrimSpace(string(body)) != "already-streaming" {
		t.Fatal("recovery appended a second protocol payload")
	}
}
