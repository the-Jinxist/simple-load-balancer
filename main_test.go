package main

import (
	"context"
	"testing"
)

// TestGenerateBackendsFromUrlsEmpty covers the only input that is safe to pass
// to generateBackendsFromUrls today: with no URLs it creates no backends. Real
// URLs are not tested in-process because NewBackend currently panics on startup
// (IMPROVEMENTS.md P0-1) and because the returned slice is known to contain nil
// entries (IMPROVEMENTS.md P0-2).
func TestGenerateBackendsFromUrlsEmpty(t *testing.T) {
	backends := generateBackendsFromUrls(context.Background(), nil)

	if len(backends) != 0 {
		t.Fatalf("len(backends) = %d, want 0", len(backends))
	}
}
