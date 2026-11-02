package balancer

import (
	"context"
	"net"
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

// TestBackendPerformHealthCheckSuccess checks the happy path against a local
// TCP listener and verifies that Alive is set to true.
func TestBackendPerformHealthCheckSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %s", err)
	}
	defer ln.Close()

	// Accept the health-check connection so the listener does not sit on an
	// unread connection. The dial succeeds even without Accept on most systems,
	// but accepting keeps the test well-behaved.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}()

	b := &Backend{Url: &url.URL{Host: ln.Addr().String()}}
	if err := b.performHealthCheck(); err != nil {
		t.Fatalf("performHealthCheck() = %v, want nil", err)
	}
	if !b.IsAlive() {
		t.Fatal("IsAlive() = false after a successful health check")
	}
}

// TestBackendPerformHealthCheckFailure checks that a refused connection marks
// the backend as not alive and returns the dial error.
func TestBackendPerformHealthCheckFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %s", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("listener.Close: %s", err)
	}

	b := &Backend{Url: &url.URL{Host: addr}}
	if err := b.performHealthCheck(); err == nil {
		t.Fatal("performHealthCheck() = nil, want an error for a closed port")
	}
	if b.IsAlive() {
		t.Fatal("IsAlive() = true after a failed health check")
	}
}

// TestNewBackendInvalidURL covers the only NewBackend path that is safe to call
// today: URL parsing fails before the health-check goroutine starts. A valid URL
// currently panics in that goroutine (see IMPROVEMENTS.md P0-1), so it cannot be
// tested in-process.
func TestNewBackendInvalidURL(t *testing.T) {
	if _, err := NewBackend(context.Background(), "%zz"); err == nil {
		t.Fatal("NewBackend() = nil error for an invalid URL")
	}
}
