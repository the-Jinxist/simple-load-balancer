package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	// The signal context is passed to the backends and used to trigger a
	// graceful HTTP server shutdown.
	backends, err := generateBackendsFromUrls(ctx, urls)
	if err != nil {
		log.Fatalf("error while starting server: %s", err)
	}

	pool := balancer.InitializePool(backends)
	balancer := balancer.NewBalancer(pool)

	server := http.Server{
		Addr:              fmt.Sprintf(":%d", 8080),
		Handler:           balancer,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Run the server in a goroutine so the main goroutine can handle signals.
	// http.ErrServerClosed is treated as a clean shutdown, not a fatal error.
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}
}

// generateBackendsFromUrls creates one Backend per URL and starts each
// backend's health checker. It returns an error if the URL list is empty or a
// backend cannot be created.
func generateBackendsFromUrls(ctx context.Context, urls []string) ([]*balancer.Backend, error) {
	if len(urls) == 0 {
		return nil, errors.New("please add at least one url")
	}

	backends := make([]*balancer.Backend, 0, len(urls))
	for _, url := range urls {
		b, err := balancer.NewBackend(ctx, url)
		if err != nil {
			return nil, err
		}

		backends = append(backends, b)
	}

	return backends, nil
}
