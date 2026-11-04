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

// LoadBalance selects the next alive backend from the global pool and forwards
// the request to it. If no backend is available it responds with HTTP 503.
//
// Known issues: the handler writes a 503 even after a successful proxy because
// it does not return after ServeHTTP, and it calls log.Fatal when the pool is
// uninitialized. See IMPROVEMENTS.md (P1-4, P1-5).
func LoadBalance(w http.ResponseWriter, r *http.Request) {
	pool := GetServerPool()
	if pool == nil {
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}

	peer := pool.NextPeer()
	if peer != nil {
		peer.ReverseProxy.ServeHTTP(w, r)
		return
	}

	// NOTE: this line also runs after a successful ServeHTTP above.
	// See IMPROVEMENTS.md (P1-4).
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
