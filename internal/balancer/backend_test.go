package balancer

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

func TestBackendAliveFlag(t *testing.T) {
	b := &Backend{}

	if b.IsAlive() {
		t.Fatal("a new Backend should start as not alive")
	}

	b.SetAlive(true)
	if !b.IsAlive() {
		t.Fatal("IsAlive() = false after SetAlive(true)")
	}

	b.SetAlive(false)
	if b.IsAlive() {
		t.Fatal("IsAlive() = true after SetAlive(false)")
	}
}

// TestBackendAliveFlagConcurrent verifies that Alive is safe to update and read
// from multiple goroutines. Run with -race to check the mutex.
func TestBackendAliveFlagConcurrent(t *testing.T) {
	b := &Backend{}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.SetAlive(true)
				b.SetAlive(false)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = b.IsAlive()
			}
		}()
	}
	wg.Wait()
}

// TestBackendPerformHealthCheckSuccess verifies that a 2xx health response
// marks the backend alive.
func TestBackendPerformHealthCheckSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %s", upstream.URL, err)
	}

	b := &Backend{Url: target, client: http.DefaultClient}
	if err := b.performHealthCheck(); err != nil {
		t.Fatalf("performHealthCheck() = %v, want nil", err)
	}
	if !b.IsAlive() {
		t.Fatal("IsAlive() = false after a 2xx health response")
	}
}

// TestBackendPerformHealthCheckFailure verifies that a connection failure marks
// the backend dead and returns the error.
func TestBackendPerformHealthCheckFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %s", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("listener.Close: %s", err)
	}

	target, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatalf("url.Parse(%q): %s", target, err)
	}

	b := &Backend{Url: target, client: http.DefaultClient}
	if err := b.performHealthCheck(); err == nil {
		t.Fatal("performHealthCheck() = nil, want a connection error")
	}
	if b.IsAlive() {
		t.Fatal("IsAlive() = true after a connection failure")
	}
}

// TestBackendPerformHealthCheckNon2xx verifies that a non-2xx response marks the
// backend dead.
func TestBackendPerformHealthCheckNon2xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unhealthy", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %s", upstream.URL, err)
	}

	b := &Backend{Url: target, client: http.DefaultClient}
	if err := b.performHealthCheck(); err == nil {
		t.Fatal("performHealthCheck() = nil, want an error for a 500 response")
	}
	if b.IsAlive() {
		t.Fatal("IsAlive() = true after a 500 response")
	}
}

// TestNewBackendInvalidURL covers the URL parse-error path, which returns before
// the health-check goroutine starts.
func TestNewBackendInvalidURL(t *testing.T) {
	if _, err := NewBackend(context.Background(), "%zz"); err == nil {
		t.Fatal("NewBackend() = nil error for an invalid URL")
	}
}
