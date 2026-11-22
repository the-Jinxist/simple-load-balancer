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

	"simple-load-balancer/pkg"
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
	// ctx stops the health-check loop when cancelled.
	ctx context.Context
	// client performs health-check requests.
	client *http.Client
	// healthCheckInterval is the base interval between health checks. Jitter is
	// applied on every tick.
	healthCheckInterval time.Duration
}

// NewBackend parses backendURL, builds a reverse proxy for it, and starts a
// background health-check loop.
//
// A new backend starts marked as not alive and performs an immediate health
// check; subsequent checks run at a jittered interval until ctx is cancelled.
func NewBackend(ctx context.Context, backendURL string) (*Backend, error) {
	url, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}

	backend := &Backend{
		Url:                 url,
		ctx:                 ctx,
		client:              http.DefaultClient,
		healthCheckInterval: 2 * time.Second,
	}

	proxy := httputil.NewSingleHostReverseProxy(url)

	proxy.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// ErrorHandler runs when the upstream request fails. It retries the same
	// backend up to three times with linear backoff, then writes a 502 response
	// and marks the backend dead.
	//
	// Known limitations: retries may reuse an already-consumed request body and
	// always target the same backend. See IMPROVEMENTS.md (P1-9, P1-10).
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[%s] %s\n", url.Host, err.Error())
		retries := GetRetriesFromContext(r)
		if retries < 3 {
			base := time.Duration(10+10*retries) * time.Millisecond
			<-time.After(base)
			ctx := context.WithValue(r.Context(), RetryKey, retries+1)

			if r.GetBody != nil {
				body, err := r.GetBody()
				if err != nil {
					http.Error(w, "Sorry an error occurred", http.StatusInternalServerError)
					return
				}
				r = r.WithContext(ctx)
				r.Body = body
			}

			proxy.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		backend.SetAlive(false)
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

// startPeriodicalHealthChecks performs an immediate health check, then repeats
// at a jittered interval until the backend context is cancelled.
func (b *Backend) startPeriodicalHealthChecks() {
	if err := b.performHealthCheck(); err != nil {
		log.Printf("initial health check failed for %s: %v", b.Url.Host, err)
	}

	ticker := time.NewTicker(pkg.Jitter(b.healthCheckInterval, 0.2))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := b.performHealthCheck()
			if err != nil {
				log.Printf("error while performing health check: %s", err)
			}

			ticker.Reset(pkg.Jitter(b.healthCheckInterval, 0.1))
		case <-b.ctx.Done():
			fmt.Println("Stopped ticker loop.")
			return
		}
	}
}

// performHealthCheck sends an HTTP GET to the backend and records the result in
// Alive. Any 2xx response marks the backend alive; request errors, timeouts, and
// non-2xx responses mark it dead.
func (b *Backend) performHealthCheck() error {
	timeout := 2 * time.Second
	checkCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, b.Url.String(), nil)
	if err != nil {
		b.SetAlive(false)
		return err
	}

	res, err := b.client.Do(req)
	if err != nil {
		b.SetAlive(false)
		return err
	}

	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		b.SetAlive(false)
		return fmt.Errorf("health check returned %s", res.Status)
	}

	b.SetAlive(true)
	return nil
}
