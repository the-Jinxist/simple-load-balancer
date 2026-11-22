package balancer

import (
	"net/http"
)

// ContextValue is the type used for keys that the balancer stores in request
// contexts.
type ContextValue string

const (
	// RetryKey tracks how many times the current request has been retried after
	// an upstream error. It is read by GetRetriesFromContext.
	RetryKey ContextValue = "RETRY"
	// AttemptKey is reserved for tracking peer-selection attempts. It is defined
	// but not currently used anywhere. See IMPROVEMENTS.md (P2-16).
	AttemptKey ContextValue = "ATTEMPT"
)

// Balancer is an HTTP handler that selects a backend from a ServerPool and
// proxies the request to it.
type Balancer struct {
	pool *ServerPool
}

// NewBalancer returns a Balancer backed by pool. A nil pool causes every
// request to receive a 503 response.
func NewBalancer(pool *ServerPool) *Balancer {
	return &Balancer{
		pool: pool,
	}
}

// ServeHTTP selects the next alive backend and forwards the request to it. If
// the pool is nil or no backend is alive, it responds with HTTP 503.
func (b *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pool := b.pool
	if pool == nil {
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}

	peer := pool.NextPeer()
	if peer != nil {
		peer.ReverseProxy.ServeHTTP(w, r)
		return
	}

	// No peer was selected.
	http.Error(w, "Service not available", http.StatusServiceUnavailable)
}

// GetRetriesFromContext returns how many times the request has been retried, or
// zero when the request carries no retry value.
func GetRetriesFromContext(r *http.Request) int {
	if retry, ok := r.Context().Value(RetryKey).(int); ok {
		return retry
	}
	return 0
}

// GetAttemptsFromContext returns the peer-selection attempt count stored on the
// request, or zero when no count is present. The value is never written today,
// so this function always returns zero. See IMPROVEMENTS.md (P2-16).
func GetAttemptsFromContext(r *http.Request) int {
	if retry, ok := r.Context().Value(AttemptKey).(int); ok {
		return retry
	}
	return 0
}
