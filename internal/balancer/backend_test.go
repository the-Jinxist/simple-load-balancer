package balancer

import (
	"context"
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

// TestBackendPerformHealthCheckMarksDeadWithoutRequest documents the current
// health-check behavior. The error check is inverted: a successfully built
// request immediately marks the backend dead and returns nil, so no request is
// ever sent. Replace this with real success/failure tests when P2-14 is fixed.
func TestBackendPerformHealthCheckMarksDeadWithoutRequest(t *testing.T) {
	hosts := []string{"example.com", "127.0.0.1:1"}

	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			b := &Backend{Url: &url.URL{Host: host}}

			if err := b.performHealthCheck(); err != nil {
				t.Fatalf("performHealthCheck() = %v, want nil (current behavior)", err)
			}
			if b.IsAlive() {
				t.Fatal("IsAlive() = true, want false (current behavior)")
			}
		})
	}
}

// TestNewBackendInvalidURL covers the URL parse-error path, which returns before
// the health-check goroutine starts.
func TestNewBackendInvalidURL(t *testing.T) {
	if _, err := NewBackend(context.Background(), "%zz"); err == nil {
		t.Fatal("NewBackend() = nil error for an invalid URL")
	}
}
