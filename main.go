package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"simple-load-balancer/internal/balancer"
)

// main wires configuration, backends, and the HTTP server together.
//
// The backend URLs and the listen address are currently hardcoded. At least one
// valid backend URL is required for the process to serve traffic.
func main() {
	// stop restores the default signal behavior once the program exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// NOTE: these empty strings are placeholders. url.Parse accepts them, but
	// the resulting backends have no host and can never pass a health check.
	// See IMPROVEMENTS.md (P0-3).
	urls := []string{
		"", "", "", "",
	}

	// NOTE: the signal context is only passed to the backends. The HTTP server
	// itself does not shut down gracefully yet. See IMPROVEMENTS.md (P2-11).
	backends := generateBackendsFromUrls(ctx, urls)
	balancer.InitializePool(backends)

	server := http.Server{
		Addr:    fmt.Sprintf(":%d", 8080),
		Handler: http.HandlerFunc(balancer.LoadBalance),
	}

	// NOTE: ListenAndServe returns http.ErrServerClosed on a clean shutdown, so
	// treating every error as fatal also exits non-zero in that case. The server
	// also has no read/write/idle timeouts yet. See IMPROVEMENTS.md (P2-11,
	// P2-12).
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// generateBackendsFromUrls creates one Backend per URL and starts each
// backend's health checker. It exits the process (via log.Fatalf) if a backend
// cannot be created.
//
// NOTE: the slice is preallocated with len(urls) entries and then appended to,
// so the result contains len(urls) leading nil entries followed by the real
// backends. See IMPROVEMENTS.md (P0-2).
func generateBackendsFromUrls(ctx context.Context, urls []string) []*balancer.Backend {
	backends := make([]*balancer.Backend, len(urls))
	for _, url := range urls {
		b, err := balancer.NewBackend(ctx, url)
		if err != nil {
			log.Fatalf("Failure while setting up backend: %s", err)
		}

		backends = append(backends, b)
	}

	return backends
}
