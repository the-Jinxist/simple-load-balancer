package main

import (
	"context"
	"testing"
)

// TestGenerateBackendsFromUrlsEmpty verifies the validation added in 2adc653:
// an empty URL list is rejected before any backend is created.
func TestGenerateBackendsFromUrlsEmpty(t *testing.T) {
	backends, err := generateBackendsFromUrls(context.Background(), nil)
	if err == nil {
		t.Fatal("generateBackendsFromUrls() = nil error, want an error for an empty list")
	}
	if backends != nil {
		t.Fatalf("backends = %v, want nil", backends)
	}
}

// TestGenerateBackendsFromUrlsInvalidURL covers the parse-error path, which
// returns before NewBackend starts its health-check goroutine.
func TestGenerateBackendsFromUrlsInvalidURL(t *testing.T) {
	backends, err := generateBackendsFromUrls(context.Background(), []string{"%zz"})
	if err == nil {
		t.Fatal("generateBackendsFromUrls() = nil error, want a URL parse error")
	}
	if backends != nil {
		t.Fatalf("backends = %v, want nil", backends)
	}
}

// TestGenerateBackendsFromUrlsValid is a regression test for the preallocation
// bug: the function previously assigned into a zero-length slice instead of
// appending. Port 1 on loopback is used so that a future working health check
// fails fast without external traffic; the cancellation context stops the
// health-check goroutines when the test ends.
func TestGenerateBackendsFromUrlsValid(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	urls := []string{"http://127.0.0.1:1", "http://127.0.0.1:1"}
	backends, err := generateBackendsFromUrls(ctx, urls)
	if err != nil {
		t.Fatalf("generateBackendsFromUrls() = %v, want nil", err)
	}
	if len(backends) != len(urls) {
		t.Fatalf("len(backends) = %d, want %d", len(backends), len(urls))
	}
	for i, b := range backends {
		if b == nil {
			t.Fatalf("backends[%d] = nil", i)
		}
		if b.Url == nil || b.Url.Host != "127.0.0.1:1" {
			t.Fatalf("backends[%d].Url = %v, want host 127.0.0.1:1", i, b.Url)
		}
	}
}
