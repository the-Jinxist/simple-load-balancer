# Improvement plan

This document is a companion to [README.md](README.md). It records the issues
found while reviewing the current implementation, why they matter, and a
proposed order for fixing them. Issue IDs are referenced from comments in the
source and from the test suite.

No code in this document has been applied yet; it is a plan.

## Priority overview

| ID | Severity | Area | Summary |
| --- | --- | --- | --- |
| P0-1 | Critical | Lifecycle | `Backend.ctx` is never assigned, so the health-check goroutine panics at startup. |
| P0-2 | Critical | Startup | `generateBackendsFromUrls` preallocates and appends, producing leading `nil` backends. |
| P0-3 | Critical | Configuration | Backend URLs in `main.go` are empty strings. |
| P1-4 | High | HTTP handler | `LoadBalance` writes a 503 after every successful proxied response. |
| P1-5 | High | HTTP handler | `log.Fatal` inside the request path exits the server. |
| P1-6 | High | Selection | Round-robin starts at index 1 and stores unnormalized cursor values. |
| P1-7 | High | Selection | Empty pool causes a divide-by-zero panic. |
| P1-8 | High | Health checks | No check runs until one minute after startup; backends start dead. |
| P1-9 | High | Retries | Retries reuse an already-consumed request body. |
| P1-10 | High | Retries | Retries target the same failing backend. |
| P2-11 | Medium | Lifecycle | No graceful shutdown; clean shutdown is treated as a fatal error. |
| P2-12 | Medium | Robustness | No server or proxy transport timeouts. |
| P2-13 | Medium | Design | Backend error handler calls the global balancer, creating coupling and recursion. |
| P2-14 | Medium | Health checks | TCP-only checks consider an erroring HTTP server healthy. |
| P2-15 | Medium | Design | Mutable package-level global pool is hard to test and inject. |
| P2-16 | Low | Cleanup | Dead code, naming, logging, and comment inconsistencies. |
| P3-17 | Low | Testing | Expand tests and update characterization tests as fixes land. |
| P3-18 | Low | Project hygiene | Config, logging, metrics, CI, packaging. |

---

## P0 — Blocks running at all

### P0-1: `Backend.ctx` is never assigned

**Current:** `NewBackend` constructs `&Backend{Url: url}` and never stores the
`ctx` argument. `startPeriodicalHealthChecks` selects on `b.ctx.Done()`; a nil
interface panics immediately.

```text
$ go run .
panic: runtime error: invalid memory address or nil pointer dereference
    .../balancer.(*Backend).startPeriodicalHealthChecks(...)
```

**Impact:** The process crashes on the first backend it creates. Any real
configuration still fails.

**Fix:** Store the context (minimal change):

```go
backend := &Backend{
    Url: url,
    ctx: ctx,
}
```

**Better fix:** Remove the `ctx` field and pass the context to the goroutine,
which makes the dependency explicit and avoids storing request-scoped values on
a long-lived struct:

```go
func NewBackend(ctx context.Context, backendURL string) (*Backend, error) {
    // ...
    go backend.startPeriodicalHealthChecks(ctx)
    return backend, nil
}

func (b *Backend) startPeriodicalHealthChecks(ctx context.Context) {
    // ...
    case <-ctx.Done():
        return
}
```

**Acceptance:** Starting with a valid backend no longer panics; cancelling the
context stops the health-check goroutine.

---

### P0-2: `generateBackendsFromUrls` returns leading `nil` entries

**Current:**

```go
backends := make([]*balancer.Backend, len(urls))
for _, url := range urls {
    // ...
    backends = append(backends, b)
}
```

Preallocating with length `n` and then appending produces a slice of length
`2n`: the first `n` entries are `nil`, followed by the real backends. Verified
behavior for four URLs:

```text
len=8 backends=[<nil> <nil> <nil> <nil> <backend> <backend> <backend> <backend>]
```

**Impact:** `NextPeer` calls `s.servers[idx].IsAlive()` and panics on the first
nil entry. This is masked today by the earlier P0-1 panic.

**Fix:**

```go
backends := make([]*balancer.Backend, 0, len(urls))
```

Also consider returning `([]*balancer.Backend, error)` instead of calling
`log.Fatalf`, so `main` owns process exit.

**Acceptance:** `len(backends) == len(urls)` and no entry is nil, even when a
backend fails to parse.

---

### P0-3: Backend URLs are empty placeholders

**Current:** `main.go` uses four empty strings. `url.Parse("")` succeeds with an
empty URL, so construction does not fail, but the backend has no host and can
never become healthy.

**Impact:** The pool is permanently empty of healthy peers; every request gets a
503 (once startup bugs are fixed).

**Fix:** Populate real URLs and validate them at startup:

```go
urls := []string{
    "http://127.0.0.1:9001",
    "http://127.0.0.1:9002",
}

for _, raw := range urls {
    u, err := url.Parse(raw)
    if err != nil || u.Scheme == "" || u.Host == "" {
        log.Fatalf("invalid backend URL %q", raw)
    }
}
```

Prefer a configuration source (flags, environment, or a file) over editing
source. See P3-18.

**Acceptance:** Invalid or empty URLs fail fast with a clear message; valid URLs
produce usable backends.

---

## P1 — Correctness bugs

### P1-4: `LoadBalance` always writes a 503

**Current:**

```go
peer := pool.NextPeer()
if peer != nil {
    peer.ReverseProxy.ServeHTTP(w, r)
}
http.Error(w, "Service not available", http.StatusServiceUnavailable)
```

There is no `return` after `ServeHTTP`. Every successful request therefore
attempts a second `WriteHeader` and appends `Service not available` to the
upstream response.

**Impact:** Corrupted response bodies and `superfluous response.WriteHeader`
warnings. `TestLoadBalanceSuccessAppendsError` currently locks this behavior in.

**Fix:**

```go
peer := pool.NextPeer()
if peer == nil {
    http.Error(w, "Service not available", http.StatusServiceUnavailable)
    return
}
peer.ReverseProxy.ServeHTTP(w, r)
```

**Acceptance:** A successful proxied response contains only upstream bytes and
the upstream status code; the 503 path is unchanged.

---

### P1-5: `log.Fatal` in an HTTP handler

**Current:** `LoadBalance` calls `log.Fatal("Sever pool is not initialized yet")`
when the global pool is nil. `log.Fatal` calls `os.Exit(1)` from whichever
goroutine is handling the request.

**Impact:** A single request before initialization (or a future initialization
bug) kills the entire server. It also makes the branch untestable.

**Fix:** Return a 503 and log at error level:

```go
pool := GetServerPool()
if pool == nil {
    log.Print("load balancer: server pool is not initialized")
    http.Error(w, "Service not available", http.StatusServiceUnavailable)
    return
}
```

Better still, remove the global entirely (P2-15) so the handler always has a
pool.

**Acceptance:** A nil pool produces a 503 and the process keeps running. Add a
test once the branch no longer exits.

---

### P1-6: Round-robin skips index 0 and stores unnormalized values

**Current:**

```go
func (s *ServerPool) NextIndex() int {
    return int(s.current.Add(1) % uint64(len(s.servers)))
}
```

The cursor is incremented before the modulo, so the first call returns `1`.
Observed sequence for three backends: `1, 2, 0, ...`.

When a fallback scan skips dead peers, `NextPeer` stores the raw loop index:

```go
s.current.Store(uint64(i)) // i may be >= len(s.servers)
```

For a four-backend pool where only index 0 is alive, the cursor becomes `4`
instead of `0`, so the next call computes `5 % 4 == 1` and rescans the dead
peers. The cursor no longer tracks the selected backend.

**Impact:** Uneven first-request distribution and incorrect cursor state after
failover. `Add` followed by `Store` is also not atomic as a sequence, so
concurrent requests can clobber each other's cursor updates.

**Fix:** Normalize the cursor and avoid storing raw indexes:

```go
func (s *ServerPool) NextIndex() int {
    n := uint64(len(s.servers))
    return int((s.current.Add(1) - 1) % n)
}

func (s *ServerPool) NextPeer() *Backend {
    n := len(s.servers)
    if n == 0 {
        return nil
    }

    start := s.NextIndex()
    for i := 0; i < n; i++ {
        idx := (start + i) % n
        if s.servers[idx].IsAlive() {
            return s.servers[idx]
        }
    }
    return nil
}
```

This keeps the cursor monotonic and lets the scan wrap correctly. If the cursor
must jump to the selected backend, use a CAS loop rather than `Add` + `Store`;
be aware that pinning the cursor can cause contention and does not improve
balance.

**Acceptance:** First selection is index 0; a full cycle visits every alive
backend exactly once; the cursor stays within `[0, n)`; `-race` is clean under
concurrent selection.

`TestServerPoolNextIndexSequence`, `TestServerPoolNextPeerRoundRobin`, and
`TestServerPoolNextPeerStoresRawIndex` must be updated to the new expectations.

---

### P1-7: Empty pool divide-by-zero

**Current:** `NextIndex` evaluates `% uint64(len(s.servers))`; a zero-length
pool panics with integer divide by zero. `NextPeer` does not guard it.

**Impact:** The process crashes if configuration produces no backends.

**Fix:** Guard in `NextPeer` (and, defensively, in `NextIndex` or by making it
return an error/sentinel):

```go
if len(s.servers) == 0 {
    return nil
}
```

**Acceptance:** `NextPeer` on an empty pool returns nil and `LoadBalance`
responds 503. Replace `TestServerPoolNextPeerEmptyPoolPanics` with a nil-return
assertion.

---

### P1-8: No backend is usable for the first minute

**Current:** `Alive` defaults to `false`, and the ticker's first tick is after
`1 * time.Minute`. Nothing checks health at construction time.

**Impact:** For the first minute after startup, `NextPeer` finds no alive
backend and every request returns 503.

**Fix:** Run a check immediately, then start the loop:

```go
func (b *Backend) startPeriodicalHealthChecks(ctx context.Context) {
    if err := b.performHealthCheck(ctx); err != nil {
        log.Printf("initial health check failed for %s: %v", b.Url.Host, err)
    }

    ticker := time.NewTicker(b.checkInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            if err := b.performHealthCheck(ctx); err != nil {
                log.Printf("health check failed for %s: %v", b.Url.Host, err)
            }
        case <-ctx.Done():
            return
        }
    }
}
```

Make the interval configurable and consider adding jitter so checks do not
synchronize across backends.

**Acceptance:** A healthy backend can receive traffic immediately after
`NewBackend` returns (or is registered), and a canceled context stops the loop.

---

### P1-9: Retries reuse a consumed request body

**Current:** On proxy error, `ErrorHandler` re-calls `proxy.ServeHTTP(w,
r.WithContext(ctx))` with the same request. The body may already have been read
and forwarded (or partially read), so a retried `POST`/`PUT` can send an empty
or truncated body.

**Impact:** Silent request corruption on non-idempotent methods and on large
bodies.

**Fix:** Choose one of:

1. Do not retry requests with a body. Restrict retries to `GET`, `HEAD`,
   `OPTIONS`, and `TRACE`.
2. If retries are required, reconstitute the body from `r.GetBody`:

   ```go
   if r.GetBody != nil {
       body, err := r.GetBody()
       if err != nil {
           // fail the request
       }
       r = r.WithContext(ctx)
       r.Body = body
   }
   ```

3. Better: limit retries to failures that occur before any bytes are written to
   the client, and buffer or discard the response when attempting a retry.

**Acceptance:** Retried requests carry the original body, or body-bearing
requests are not retried; behavior is documented in the README.

---

### P1-10: Retries target the same failing backend

**Current:** `ErrorHandler` sleeps 10 ms and retries the backend that just
failed, up to three times, before marking it dead.

**Impact:** Connection-refused and timeout errors rarely succeed 10 ms later, so
the retries add ~30 ms latency per failing request and delay eviction.

**Fix:** Fail over to a different peer instead. The cleanest shape is a
handler-level selection loop (see P2-13):

```go
for attempt := 0; attempt < maxAttempts; attempt++ {
    peer := pool.NextPeer()
    if peer == nil {
        break
    }
    // proxy once; on error, mark dead and continue
}
http.Error(w, "Service not available", http.StatusServiceUnavailable)
```

Keep the per-backend proxy simple: on error, mark the backend dead and let the
caller decide whether to retry elsewhere.

**Acceptance:** A dead backend is skipped after one failed attempt; retries go to
a different peer; no sleep-based retry of the same peer remains.

---

## P2 — Robustness and design

### P2-11: No graceful shutdown

**Current:** `signal.NotifyContext` is created in `main`, but the server is
never shut down. `ListenAndServe` blocks until the process exits, and its
`http.ErrServerClosed` return is passed to `log.Fatal`.

**Impact:** In-flight requests are dropped on `SIGTERM`, and a clean shutdown
exits with a non-zero status.

**Fix:**

```go
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
```

**Acceptance:** `SIGTERM` stops accepting new connections, waits for in-flight
requests (bounded by a timeout), and exits zero.

---

### P2-12: Missing server and transport timeouts

**Current:** `http.Server` sets only `Addr` and `Handler`. The default
`httputil.ReverseProxy` transport has no `ResponseHeaderTimeout` and uses default
dial/TLS settings.

**Impact:** Slowloris-style clients can hold connections open indefinitely; a
slow upstream can tie up handlers indefinitely.

**Fix:**

```go
server := &http.Server{
    Addr:              ":8080",
    Handler:           http.HandlerFunc(balancer.LoadBalance),
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       30 * time.Second,
    WriteTimeout:      30 * time.Second,
    IdleTimeout:       120 * time.Second,
}
```

Give the reverse proxy a tuned transport:

```go
proxy.Transport = &http.Transport{
    Proxy:                 http.ProxyFromEnvironment,
    DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
    MaxIdleConns:          100,
    MaxIdleConnsPerHost:   10,
    IdleConnTimeout:       90 * time.Second,
    ResponseHeaderTimeout: 10 * time.Second,
    ExpectContinueTimeout: 1 * time.Second,
}
```

**Acceptance:** Timeouts are configured and documented; a stalled upstream
releases the handler within the configured bound.

---

### P2-13: Backend error handler couples to the global balancer

**Current:** `NewBackend`'s `ErrorHandler` calls `LoadBalance(w, r)`. This makes
a backend depend on package-global pool state and creates recursion:
`LoadBalance → proxy → ErrorHandler → LoadBalance → …` (bounded only by each
backend being marked dead).

**Impact:** Hard to reason about, hard to test, and capable of deep recursion
when many backends fail. Retry semantics are split across two layers.

**Fix:** Move peer selection and failover into a handler that owns the pool:

```go
type Handler struct {
    pool        *ServerPool
    maxAttempts int
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    for attempt := 0; attempt < h.maxAttempts; attempt++ {
        peer := h.pool.NextPeer()
        if peer == nil {
            break
        }
        if peer.TryServe(w, r) { // reports whether it wrote a response
            return
        }
        peer.SetAlive(false)
    }
    http.Error(w, "Service not available", http.StatusServiceUnavailable)
}
```

Note: retrying after a partial write is unsafe. Either:

- attempt only one proxy try per request and rely on health checks, or
- buffer the response (for example with `httputil.ReverseProxy`'s
  `ModifyResponse` plus a buffered writer) and only retry on connection-level
  failures before headers are sent.

Document the chosen guarantee. Keep `Backend` unaware of the balancer.

**Acceptance:** `internal/balancer/backend.go` has no reference to
`LoadBalance` or the global pool; failover is testable with a fake pool.

---

### P2-14: Health checks are TCP-only and fixed

**Current:** `performHealthCheck` dials `b.Url.Host` with a 2 s timeout. It does
not send HTTP, does not check status codes, and has no configurable path,
interval, or timeout.

**Impact:** A process that accepts TCP connections but returns 500 is considered
healthy. A URL without an explicit port produces a dial error.

**Fix:** Use an HTTP health request with a context timeout:

```go
func (b *Backend) performHealthCheck(ctx context.Context) error {
    checkCtx, cancel := context.WithTimeout(ctx, b.checkTimeout)
    defer cancel()

    req, err := http.NewRequestWithContext(checkCtx, http.MethodGet,
        b.Url.ResolveReference(&url.URL{Path: b.healthPath}).String(), nil)
    if err != nil {
        b.SetAlive(false)
        return err
    }

    resp, err := b.client.Do(req)
    if err != nil {
        b.SetAlive(false)
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode < 200 || resp.StatusCode > 299 {
        b.SetAlive(false)
        return fmt.Errorf("health check returned %s", resp.Status)
    }

    b.SetAlive(true)
    return nil
}
```

Make `healthPath`, `checkInterval`, and `checkTimeout` configuration fields.

**Acceptance:** An upstream returning a non-2xx status is marked dead; a healthy
upstream is marked alive; timeouts are bounded.

---

### P2-15: Mutable package-level global pool

**Current:** `var serverPool *ServerPool` is set by `InitializePool` and read by
`GetServerPool`. `LoadBalance` is a package-level function tied to that global.

**Impact:** Hidden dependency, untestable handler without touching global state,
and a nil/fatal path at request time. It also prevents running two balancers in
one process (for example in tests).

**Fix:** Construct the pool and inject it into a handler:

```go
pool := balancer.NewServerPool(backends)
handler := &balancer.Handler{Pool: pool}
server := &http.Server{Addr: ":8080", Handler: handler}
```

If the global must stay for compatibility, wrap initialization in `sync.Once`,
validate that `len(servers) > 0`, and avoid mutating it after startup.

**Acceptance:** No package-level mutable state is required to serve requests;
tests construct their own pool and handler.

---

### P2-16: Dead code, naming, and logging cleanup

Small, low-risk cleanups:

- Remove `AttemptKey` and `GetAttemptsFromContext`, or actually record attempts.
- Move `RetryKey` next to the retry logic, or consolidate retry constants.
- Use `URL` instead of `Url` for the exported field (Go initialism convention).
- Rename `mux` to `mu`, or use `atomic.Bool` if the mutex is not needed.
- Replace `fmt.Println("Stopped ticker loop.")` with `log.Println`, or remove it.
- Drop the trailing `\n` from `log.Printf("[%s] %s\n", ...)` (Printf adds one).
- Use `":8080"` and `8080` consistently instead of `fmt.Sprintf(":%d", 8080)`.
- Fix the typo `Sever pool` and align the `generateBackendsFromUrls` comment
  with its actual `log.Fatalf` behavior.
- Add a `main` test once startup can be exercised without panicking.

**Acceptance:** `go vet` remains clean; no unused identifiers; log output has no
double newlines.

---

## P3 — Testing and project hygiene

### P3-17: Expand tests and update characterization tests

The current suite intentionally documents the behavior that exists today. When
fixes land, update these tests in the same change:

| Test | Current expectation | After the fix |
| --- | --- | --- |
| `TestServerPoolNextIndexSequence` | First index is `1` | First index is `0` |
| `TestServerPoolNextPeerStoresRawIndex` | Cursor becomes raw index `4` | Cursor is normalized, or not pinned |
| `TestServerPoolNextPeerEmptyPoolPanics` | Empty pool panics | Empty pool returns `nil` |
| `TestLoadBalanceSuccessAppendsError` | 503 body appended after success | Only upstream body is returned |
| `TestNewBackendInvalidURL` | Only invalid URLs are covered | Add valid-URL construction and context cancellation |
| `TestGenerateBackendsFromUrlsEmpty` | Only empty input is covered | Assert one backend per URL, no nils |

Additional coverage to add:

- A table-driven `NextPeer` test with mixed alive/dead backends and wraparound.
- A full integration test: `httptest` upstreams + handler, verifying routing and
  failover.
- Fuzz `ServerPool.NextPeer` with random alive flags and pool sizes.
- Test that `SIGTERM` performs a graceful shutdown.
- Test the retry body behavior (P1-9).
- Run `go test ./... -race` in CI.

### P3-18: Project hygiene

- Externalize configuration (flags, environment, or a config file).
- Add request logging with request IDs and upstream/latency fields.
- Add metrics (requests, failures, healthy backends) and a readiness endpoint.
- Add a `Makefile` with `build`, `test`, `test-race`, `vet`, and `fmt` targets.
- Add a `Dockerfile` and a minimal CI workflow.
- Document the retry/failover guarantees and the health-check contract.
- Consider a license file if the project will be shared.

---

## Suggested implementation order

1. **Make it run:** P0-1, P0-2, P0-3. Add a valid-URL startup test path.
2. **Fix the request path:** P1-4, P1-5, P1-6, P1-7. Update the pool and handler
   characterization tests.
3. **Make health and retries sane:** P1-8, P1-9, P1-10.
4. **Operational robustness:** P2-11, P2-12.
5. **Refactor for testability:** P2-13, P2-15, then P2-14 and P2-16.
6. **Tooling:** P3-17 and P3-18.

## Definition of done

- `go run .` serves traffic with a valid backend list and does not panic.
- `go test ./... -race`, `go vet ./...`, and `go test ./...` all pass.
- `gofmt -l .` reports no files.
- No `log.Fatal` in a request path; no nil-pool or empty-pool panic.
- Shutdown is graceful and bounded.
- Health checks are HTTP-based and configurable.
- Retry and failover semantics are documented and covered by tests.
- README is updated to match the fixed behavior; this document is either marked
  done or trimmed to the remaining work.
