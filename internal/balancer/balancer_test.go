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

// TestBalancerNoAliveBackends verifies that the handler responds 503 when the
// pool has no healthy peers.
func TestBalancerNoAliveBackends(t *testing.T) {
	handler := NewBalancer(InitializePool([]*Backend{testBackend(false)}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Service not available") {
		t.Fatalf("body = %q, want it to contain %q", body, "Service not available")
	}
}

// TestBalancerNilPool verifies the 503 response when no pool was supplied.
func TestBalancerNilPool(t *testing.T) {
	handler := NewBalancer(nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Service not available") {
		t.Fatalf("body = %q, want it to contain %q", body, "Service not available")
	}
}

// TestBalancerSuccess verifies that a successful proxy response is returned
// as-is, without appending the 503 body.
func TestBalancerSuccess(t *testing.T) {
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
	handler := NewBalancer(InitializePool([]*Backend{backend}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	handler.ServeHTTP(rec, req)

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
