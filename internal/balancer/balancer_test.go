package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func TestGetRetriesFromContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	if got := GetRetriesFromContext(r); got != 0 {
		t.Fatalf("GetRetriesFromContext() without a value = %d, want 0", got)
	}

	r = r.WithContext(context.WithValue(r.Context(), RetryKey, 3))
	if got := GetRetriesFromContext(r); got != 3 {
		t.Fatalf("GetRetriesFromContext() = %d, want 3", got)
	}
}

func TestGetAttemptsFromContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	if got := GetAttemptsFromContext(r); got != 0 {
		t.Fatalf("GetAttemptsFromContext() without a value = %d, want 0", got)
	}

	r = r.WithContext(context.WithValue(r.Context(), AttemptKey, 2))
	if got := GetAttemptsFromContext(r); got != 2 {
		t.Fatalf("GetAttemptsFromContext() = %d, want 2", got)
	}
}

// TestLoadBalanceNoAliveBackends verifies the only error path that can be
// exercised in-process: with no healthy peers the handler responds 503.
func TestLoadBalanceNoAliveBackends(t *testing.T) {
	InitializePool([]*Backend{testBackend(false)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	LoadBalance(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Service not available") {
		t.Fatalf("body = %q, want it to contain %q", body, "Service not available")
	}
}

// TestLoadBalanceNilPool verifies the 503 response added in 2adc653 for the
// uninitialized-pool branch.
func TestLoadBalanceNilPool(t *testing.T) {
	previous := serverPool
	serverPool = nil
	t.Cleanup(func() { serverPool = previous })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	LoadBalance(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Service not available") {
		t.Fatalf("body = %q, want it to contain %q", body, "Service not available")
	}
}

// TestLoadBalanceSuccess verifies that a successful proxy response is returned
// as-is. LoadBalance now returns after ServeHTTP, so the 503 body is no longer
// appended to successful responses.
func TestLoadBalanceSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %s", upstream.URL, err)
	}

	backend := &Backend{
		Url:          target,
		Alive:        true,
		ReverseProxy: httputil.NewSingleHostReverseProxy(target),
	}
	InitializePool([]*Backend{backend})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	LoadBalance(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "upstream-ok") {
		t.Fatalf("body = %q, want it to contain the upstream response", body)
	}
	if strings.Contains(body, "Service not available") {
		t.Fatalf("body = %q, want only the upstream response", body)
	}
}
