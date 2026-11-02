package balancer

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// Backend represents a single upstream server together with the reverse proxy
// that forwards requests to it.
type Backend struct {
	// Url is the parsed address of the upstream server.
	Url *url.URL
	// Alive reports whether the most recent health check succeeded. It is
	// guarded by mux because it is written by the health-check goroutine and
	// read by request handlers.
	Alive bool
	// mux guards Alive.
	mux sync.RWMutex
	// ReverseProxy forwards requests to the upstream server. It is safe for
	// concurrent use.
	ReverseProxy *httputil.ReverseProxy
	// ctx would stop the health-check loop when cancelled, but it is currently
	// never assigned by NewBackend. See IMPROVEMENTS.md (P0-1).
	ctx context.Context
}

// NewBackend parses backendURL, builds a reverse proxy for it, and starts a
// background health-check loop.
//
// The health-check goroutine is meant to stop when ctx is cancelled. A newly
// created backend starts marked as not alive; the first check runs after one
// full ticker interval. See IMPROVEMENTS.md (P0-1, P1-8) for known issues with
// context handling and the startup delay.
func NewBackend(ctx context.Context, backendURL string) (*Backend, error) {
	url, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}

	backend := &Backend{
		Url: url,
	}

	proxy := httputil.NewSingleHostReverseProxy(url)
	// ErrorHandler runs when the upstream request fails. It retries the same
	// backend up to three times with a short delay, then marks the backend as
	// dead and asks the pool to choose another peer.
	//
	// Known issues: retries reuse an already-consumed request body and always
	// target the same backend. See IMPROVEMENTS.md (P1-9, P1-10).
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[%s] %s\n", url.Host, err.Error())
		retries := GetRetriesFromContext(r)
		if retries < 3 {
			<-time.After(10 * time.Millisecond)
			ctx := context.WithValue(r.Context(), RetryKey, retries+1)
			proxy.ServeHTTP(w, r.WithContext(ctx))

			return
		}

		backend.SetAlive(false)

		LoadBalance(w, r)
	}

	backend.ReverseProxy = proxy

	go backend.startPeriodicalHealthChecks()
	return backend, nil
}

// SetAlive updates whether this backend is considered healthy. It is safe for
// concurrent use.
func (b *Backend) SetAlive(alive bool) {
	b.mux.Lock()
	b.Alive = alive
	b.mux.Unlock()
}

// IsAlive reports whether this backend is currently considered healthy. It is
// safe for concurrent use.
func (b *Backend) IsAlive() (alive bool) {
	b.mux.RLock()
	alive = b.Alive
	b.mux.RUnlock()
	return
}

// startPeriodicalHealthChecks runs a TCP health check every minute until the
// backend's context is cancelled. Because the first check only happens on the
// first tick, a newly created backend stays marked as not alive for the first
// minute. See IMPROVEMENTS.md (P1-8).
func (b *Backend) startPeriodicalHealthChecks() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := b.performHealthCheck()
			if err != nil {
				log.Printf("error while performing health check: %s", err)
			}
		case <-b.ctx.Done():
			fmt.Println("Stopped ticker loop.")
			return
		}
	}
}

// performHealthCheck attempts a TCP connection to the backend's host and records
// the result in Alive. It does not send an HTTP request, so a process that
// accepts connections but returns errors is still considered healthy. See
// IMPROVEMENTS.md (P2-14).
func (b *Backend) performHealthCheck() error {
	timeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", b.Url.Host, timeout)
	if err != nil {
		log.Println("Site unreachable, error: ", err)
		b.SetAlive(false)
		return err
	}
	_ = conn.Close() // close it, we dont need to maintain this connection
	b.SetAlive(true)
	return nil
}
