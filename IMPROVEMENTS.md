# Improvement plan

This document is a companion to [README.md](README.md). It records the issues
found while reviewing the current implementation, why they matter, and a
proposed order for fixing them. Issue IDs are referenced from comments in the
source and from the test suite.

**Last reviewed:** 2026-11-04, against commit `2adc653` ("fix: solved some P0
and P1 issues"). Each item now carries a status. The "Current (at review)"
paragraphs still describe the state at the time of the original review; the
status notes record what changed in `2adc653`.

**Status legend**

- **Fixed** — the behavior described in the item no longer occurs.
- **Partial** — part of the problem or part of the suggested change landed.
- **Open** — no effective change landed.

## Priority overview

| ID | Severity | Area | Summary | Status |
| --- | --- | --- | --- | --- |
| P0-1 | Critical | Lifecycle | `Backend.ctx` is never assigned, so the health-check goroutine panics at startup. | Open |
| P0-2 | Critical | Startup | `generateBackendsFromUrls` preallocates and appends, producing leading `nil` backends. | Open (error return added) |
| P0-3 | Critical | Configuration | Backend URLs in `main.go` are empty strings. | Open (empty-list guard only) |
| P1-4 | High | HTTP handler | `LoadBalance` writes a 503 after every successful proxied response. | Open |
| P1-5 | High | HTTP handler | `log.Fatal` inside the request path exits the server. | **Fixed** |
| P1-6 | High | Selection | Round-robin still starts at index 1; fallback cursor is now normalized. | **Partial** |
| P1-7 | High | Selection | Empty pool causes a divide-by-zero panic. | **Fixed** |
| P1-8 | High | Health checks | First check is delayed one minute; new backends are now optimistically alive. | **Partial** |
| P1-9 | High | Retries | Retries reuse a consumed body when `GetBody` is nil; a reset was added otherwise. | **Partial** |
| P1-10 | High | Retries | Retries target the same failing backend. | Open |
| P2-11 | Medium | Lifecycle | No graceful shutdown; clean shutdown is treated as a fatal error. | Open (attempt is dead code) |
| P2-12 | Medium | Robustness | No server or proxy transport timeouts. | **Fixed** |
| P2-13 | Medium | Design | Backend error handler calls the global balancer, creating coupling and recursion. | Open |
| P2-14 | Medium | Health checks | TCP-only checks consider an erroring HTTP server healthy. | Open |
| P2-15 | Medium | Design | Mutable package-level global pool is hard to test and inject. | Open |
| P2-16 | Low | Cleanup | Dead code, naming, logging, and comment inconsistencies. | **Partial** (typo only) |
| P3-17 | Low | Testing | Expand tests and update characterization tests as fixes land. | Open (tests now fail) |
| P3-18 | Low | Project hygiene | Config, logging, metrics, CI, packaging. | Open |

### Verification snapshot

Checked on 2026-11-04 at commit `2adc653`:

| Check | Result |
| --- | --- |
| `go build ./...` | Passes |
| `go vet ./...` | Fails: `main_test.go` still calls `generateBackendsFromUrls` with one return value |
| `go test ./...` | Fails: the root package does not compile, and two `balancer` tests assert stale behavior |
| `go run .` | Still panics in `startPeriodicalHealthChecks` because `Backend.ctx` is nil (P0-1) |

The commit fixed P1-5, P1-7, and P2-12 and partially addressed P1-6, P1-8, P1-9,
and P2-16. It did not update the tests or the source comments to match, so the
repository is currently in a failing state even though several fixes are real.
P0-1 still blocks startup, so the "Make it run" work remains the top priority.

---

## P0 — Blocks running at all

### P0-1: `Backend.ctx` is never assigned

**Status:** Open in `2adc653`. `NewBackend` still never assigns `ctx`, and
`go run .` still panics in `startPeriodicalHealthChecks`.

**Current (at review):** `NewBackend` constructs `&Backend{Url: url}` and never stores the
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

**Status:** Open in `2adc653`. The function now returns `([]*Backend, error)`
and rejects an empty URL list, but the allocation is still
`make([]*Backend, len(urls))` followed by `append`, so the leading nil entries
remain.

**Current (at review):**

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

**Status:** Open in `2adc653`. The URL list is still four empty strings, and
`NewBackend` still accepts them. The new empty-list check does not validate
individual URLs or require a scheme and host. Because `Alive` now defaults to
true, these host-less backends are selected and fail at proxy time instead of
being skipped by a health check.

**Current (at review):** `main.go` uses four empty strings. `url.Parse("")` succeeds with an
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

**Status:** Open in `2adc653`. `LoadBalance` still has no `return` after
`ServeHTTP`; `TestLoadBalanceSuccessAppendsError` still passes and locks the bug
in.

**Current (at review):**

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

**Status:** **Fixed** in `2adc653`. `LoadBalance` now responds with 503 and
returns when the pool is nil; `log.Fatal` is gone from the handler. The
suggested error-level log line was not added, and the nil-pool branch still has
no test.

**Current (at review):** `LoadBalance` calls `log.Fatal("Sever pool is not initialized yet")`
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

### P1-6: Round-robin still skips index 0; cursor normalization fixed

**Status:** **Partially fixed** in `2adc653`. The fallback scan now stores the
normalized index (`idx`) instead of the raw loop index, so the cursor no longer
leaves `[0, n)`. The off-by-one first selection remains (`NextIndex` still adds
before taking the modulo), and `Add` + `Store` is still not atomic as a
sequence. `TestServerPoolNextPeerStoresRawIndex` now fails because it still
expects the old raw value.

**Current (at review):** The cursor is incremented before the modulo, so the first call
returns `1`. Observed sequence for three backends: `1, 2, 0, ...`.

```go
func (s *ServerPool) NextIndex() int {
    return int(s.current.Add(1) % uint64(len(s.servers)))
}
```

Originally, when a fallback scan skipped dead peers, `NextPeer` stored the raw
loop index:

```go
s.current.Store(uint64(i)) // i may be >= len(s.servers)
```

For a four-backend pool where only index 0 was alive, the cursor became `4`
instead of `0`, so the next call computed `5 % 4 == 1` and rescanned the dead
peers. **This part is fixed:** the code now stores `uint64(idx)`.

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

`TestServerPoolNextPeerStoresRawIndex` must be updated now (the cursor is
normalized). `TestServerPoolNextIndexSequence` and
`TestServerPoolNextPeerRoundRobin` still pass and should be updated when the
off-by-one is fixed.

---

### P1-7: Empty pool divide-by-zero

**Status:** **Fixed** in `2adc653`. `NextIndex` now returns `0` when the pool is
empty, so `NextPeer` returns nil instead of panicking. The guard lives in
`NextIndex` rather than `NextPeer`; it is effective, but a sentinel index is
less explicit than an empty-pool check in `NextPeer`. The test that expects a
panic (`TestServerPoolNextPeerEmptyPoolPanics`) now fails and needs updating.

**Current (at review):** `NextIndex` evaluates `% uint64(len(s.servers))`; a zero-length
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

**Status:** **Partially addressed** in `2adc653`. `NewBackend` now sets
`Alive: true`, so a new backend can receive traffic immediately and the
"unusable for the first minute" symptom is gone. The trade-off is that a
backend that is actually down stays in rotation until the first tick or its
first proxy error. No health check runs at construction time, and the interval
is still hardcoded to one minute.

**Current (at review):** `Alive` defaults to `false`, and the ticker's first tick is after
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

**Status:** **Partially fixed** in `2adc653`. The retry path now calls
`r.GetBody()` and replaces `r.Body` when `GetBody` is non-nil. That helps for
client-side requests and tests, but incoming server requests normally have
`GetBody == nil`, so production retries still reuse the consumed body. Retries
are also still allowed for non-idempotent methods, and the behavior is not
documented in the README.

**Current (at review):** On proxy error, `ErrorHandler` re-calls `proxy.ServeHTTP(w,
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

**Status:** Open in `2adc653`. The retry loop still targets the same backend
three times; only the body handling changed (P1-9).

**Current (at review):** `ErrorHandler` sleeps 10 ms and retries the backend that just
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

**Status:** Open in `2adc653`, despite an attempted fix. A `select` on
`ctx.Done()` that calls `server.Shutdown` was added, but it runs *after* the
blocking `ListenAndServe`, so it is unreachable while the server is running.
`log.Fatal(err)` also still exits before the select when `ListenAndServe`
returns `http.ErrServerClosed`. The server therefore still does not shut down on
`SIGTERM`. Restructure it as in the fix below: run `ListenAndServe` in a
goroutine and select on both the error channel and `ctx.Done()`.

**Current (at review):** `signal.NotifyContext` is created in `main`, but the server is
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

**Status:** **Fixed** in `2adc653`. The server now sets `ReadHeaderTimeout`,
`ReadTimeout`, `WriteTimeout`, and `IdleTimeout`, and `NewBackend` installs a
tuned `http.Transport` with dial, idle, response-header, and expect-continue
timeouts. The values are hardcoded rather than configurable, but the original
issue is addressed.

**Current (at review):** `http.Server` sets only `Addr` and `Handler`. The default
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

**Status:** Open in `2adc653`. `backend.go` still calls `LoadBalance` from the
proxy error handler.

**Current (at review):** `NewBackend`'s `ErrorHandler` calls `LoadBalance(w, r)`. This makes
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

**Status:** Open in `2adc653`. `performHealthCheck` still performs a TCP dial
only; the new transport timeouts apply to proxied requests, not health checks.

**Current (at review):** `performHealthCheck` dials `b.Url.Host` with a 2 s timeout. It does
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

**Status:** Open in `2adc653`. The package-level `serverPool`, `InitializePool`,
and `GetServerPool` global remain.

**Current (at review):** `var serverPool *ServerPool` is set by `InitializePool` and read by
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

**Status:** **Partially fixed** in `2adc653`. The `Sever pool` typo is gone
along with the `log.Fatal` call. The other cleanups remain: `AttemptKey` /
`GetAttemptsFromContext`, `Url`, `mux`, `fmt.Println`, the trailing `\n`, and
`fmt.Sprintf(":%d", 8080)`. The signature change also made two doc comments
stale: `generateBackendsFromUrls` no longer calls `log.Fatalf`, and `NewBackend`
no longer creates a backend that starts not alive. `go vet` currently fails
because `main_test.go` still assumes the old one-value signature.

Small, low-risk cleanups:

- Remove `AttemptKey` and `GetAttemptsFromContext`, or actually record attempts.
- Move `RetryKey` next to the retry logic, or consolidate retry constants.
- Use `URL` instead of `Url` for the exported field (Go initialism convention).
- Rename `mux` to `mu`, or use `atomic.Bool` if the mutex is not needed.
- Replace `fmt.Println("Stopped ticker loop.")` with `log.Println`, or remove it.
- Drop the trailing `\n` from `log.Printf("[%s] %s\n", ...)` (Printf adds one).
- Use `":8080"` and `8080` consistently instead of `fmt.Sprintf(":%d", 8080)`.
- Align the `generateBackendsFromUrls` doc comment with its current error-return
  behavior (it still mentions `log.Fatalf`). The `Sever pool` typo is already
  fixed.
- Add a `main` test once startup can be exercised without panicking.

**Acceptance:** `go vet` remains clean; no unused identifiers; log output has no
double newlines.

---

## P3 — Testing and project hygiene

### P3-17: Expand tests and update characterization tests

**Status:** Open and regressed in `2adc653`. The source changed but the tests
were not updated, so the repository currently fails `go vet` and `go test`:

| Test | State after `2adc653` | Action |
| --- | --- | --- |
| `TestServerPoolNextIndexSequence` | Passes; still expects first index `1` | Update when the P1-6 off-by-one is fixed |
| `TestServerPoolNextPeerStoresRawIndex` | **Fails**: expects cursor `4`, now `0` | Replace with a normalized-cursor assertion now (P1-6 partial fix) |
| `TestServerPoolNextPeerEmptyPoolPanics` | **Fails**: no panic now | Replace with a nil-return assertion now (P1-7 fixed) |
| `TestLoadBalanceSuccessAppendsError` | Passes; still documents the appended 503 | Update when P1-4 is fixed |
| `TestNewBackendInvalidURL` | Passes; valid URLs still panic | Add valid-URL and context-cancellation coverage after P0-1 |
| `TestGenerateBackendsFromUrlsEmpty` | **Build failure**: calls the old one-value signature; the function now also returns an error for empty input | Update the call and assert the new error now |

Additional coverage to add:

- A table-driven `NextPeer` test with mixed alive/dead backends and wraparound.
- A full integration test: `httptest` upstreams + handler, verifying routing and
  failover.
- Fuzz `ServerPool.NextPeer` with random alive flags and pool sizes.
- Test that `SIGTERM` performs a graceful shutdown.
- Test the retry body behavior (P1-9).
- Run `go test ./... -race` in CI.

### P3-18: Project hygiene

**Status:** Open in `2adc653`. Configuration is still hardcoded; no request
logging, metrics, Makefile, Dockerfile, or CI were added.

- Externalize configuration (flags, environment, or a config file).
- Add request logging with request IDs and upstream/latency fields.
- Add metrics (requests, failures, healthy backends) and a readiness endpoint.
- Add a `Makefile` with `build`, `test`, `test-race`, `vet`, and `fmt` targets.
- Add a `Dockerfile` and a minimal CI workflow.
- Document the retry/failover guarantees and the health-check contract.
- Consider a license file if the project will be shared.

---

## Suggested implementation order

1. **Make it run:** P0-1, P0-2, P0-3. **No progress** — all three remain open and
   `go run .` still panics.
2. **Fix the request path:** P1-4, P1-5, P1-6, P1-7. **P1-5 and P1-7 fixed;
   P1-6 partial; P1-4 open.** The P1-6/P1-7 characterization tests are now stale
   and failing.
3. **Make health and retries sane:** P1-8, P1-9, P1-10. **P1-8 and P1-9 partial;
   P1-10 open.**
4. **Operational robustness:** P2-11, P2-12. **P2-12 fixed; P2-11 attempted but
   still open (the new shutdown block is unreachable).**
5. **Refactor for testability:** P2-13, P2-15, then P2-14 and P2-16. **No
   progress**, apart from the typo cleanup in P2-16.
6. **Tooling:** P3-17 and P3-18. **P3-17 is now urgent because the test suite does
   not compile; P3-18 has no progress.**

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
