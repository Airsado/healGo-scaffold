package stream

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestManagerCapacityAndShutdownWaitForCallbacks(t *testing.T) {
	m, err := NewManager(1)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Begin(context.Background(), time.Minute)
	if err != nil || m.Active() != 1 {
		t.Fatal("connection was not registered", err)
	}
	if _, err := m.Begin(context.Background(), time.Minute); !errors.Is(err, ErrCapacity) {
		t.Fatal("connection capacity did not reject admission")
	}
	m.Close()
	select {
	case <-s.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel active work")
	}
	if _, err := m.Begin(context.Background(), time.Minute); !errors.Is(err, ErrClosed) {
		t.Fatal("shutdown accepted another stream")
	}
	budget, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if !errors.Is(m.Wait(budget), context.DeadlineExceeded) || m.Active() != 1 {
		t.Fatal("cancellation released capacity before callback actually ended")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); s.End() }()
	}
	wg.Wait()
	if m.Active() != 0 || m.Wait(context.Background()) != nil {
		t.Fatal("idempotent End did not finish shutdown")
	}
	m.Close()
}

func TestManagerLifetimeAndReusedCapacity(t *testing.T) {
	if _, err := NewManager(0); !errors.Is(err, ErrInvalidOptions) {
		t.Fatal("invalid capacity accepted")
	}
	m, _ := NewManager(1)
	defer m.Close()
	s, _ := m.Begin(context.Background(), 15*time.Millisecond)
	select {
	case <-s.Context().Done():
		if !errors.Is(s.Context().Err(), context.DeadlineExceeded) {
			t.Fatal("stream lifetime ended for unexpected reason")
		}
	case <-time.After(time.Second):
		t.Fatal("stream lifetime was not bounded")
	}
	s.End()
	next, err := m.Begin(context.Background(), time.Minute)
	if err != nil {
		t.Fatal("ended stream kept its capacity")
	}
	next.End()
}

func TestOnlyExplicitStreamsBypassHTTPBudgetAndKeepCancellationAndValues(t *testing.T) {
	type valueKey struct{}
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	limited, cancelLimit := context.WithTimeout(PreserveRequestContext(parent), 15*time.Millisecond)
	defer cancelLimit()
	withValue := context.WithValue(limited, valueKey{}, "authenticated-value")
	flow := RequestContext(withValue)
	manager, _ := NewManager(1)
	session, err := manager.Begin(flow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer session.End()
	<-limited.Done()
	if flow.Err() != nil || flow.Value(valueKey{}) != "authenticated-value" {
		t.Fatal("stream lost request values or inherited the ordinary HTTP deadline")
	}
	if RequestContext(limited) == limited {
		t.Fatal("marked HTTP context was not adapted")
	}
	unmarked, cancelUnmarked := context.WithCancel(context.Background())
	cancelUnmarked()
	if !errors.Is(RequestContext(unmarked).Err(), context.Canceled) {
		t.Fatal("unmarked caller cancellation was discarded")
	}
	cancelParent()
	if !errors.Is(flow.Err(), context.Canceled) || !errors.Is(context.Cause(flow), context.Canceled) {
		t.Fatal("real request disconnect did not cancel the stream")
	}
	select {
	case <-session.Context().Done():
		if !errors.Is(context.Cause(session.Context()), context.Canceled) {
			t.Fatal("stream inherited the discarded ordinary HTTP timeout cause")
		}
	case <-time.After(time.Second):
		t.Fatal("real client cancellation did not reach the stream session")
	}
}

func TestOriginsAreExactAndDoNotTrustForwardedHeaders(t *testing.T) {
	for _, bad := range []string{"*", "null", "https://*.example.com", "https://user:pass@example.com", "https://example.com/", "https://example.com?x=1", "https://example.com#", "ftp://example.com", "https://example.com:65536"} {
		if ValidateOrigins([]string{bad}) == nil {
			t.Errorf("invalid origin accepted: %q", bad)
		}
	}
	if ValidateOrigins([]string{"http://localhost:5173", "https://console.example.com"}) != nil {
		t.Fatal("valid exact origins rejected")
	}
	for _, tc := range []struct {
		name, origin string
		allowed      []string
		tls          bool
		want         bool
	}{
		{"CLI", "", nil, false, true},
		{"same origin", "http://api.example.com", nil, false, true},
		{"default port", "http://api.example.com:80", nil, false, true},
		{"https origin", "https://api.example.com", nil, true, true},
		{"explicit cross origin", "https://console.example.com", []string{"https://console.example.com"}, false, true},
		{"different port", "http://api.example.com:5173", nil, false, false},
		{"different scheme", "https://api.example.com", nil, false, false},
		{"wrong host", "http://evil.example.com", nil, false, false},
		{"null origin", "null", nil, false, false},
		{"origin with path", "http://api.example.com/private", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://api.example.com/stream", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Forwarded-Host", "evil.example.com")
			if OriginAllowed(r, tc.allowed) != tc.want {
				t.Fatal("origin boundary differs from policy")
			}
		})
	}
	r := httptest.NewRequest("GET", "http://api.example.com/", nil)
	r.Header.Add("Origin", "http://api.example.com")
	r.Header.Add("Origin", "http://evil.example.com")
	if OriginAllowed(r, nil) {
		t.Fatal("multiple origins accepted")
	}
}
