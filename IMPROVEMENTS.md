# Improvement plan

This document is a companion to [README.md](README.md). It records what is
still wrong or unfinished in the current implementation, why it matters, and a
proposed order for fixing it. Issue IDs are referenced from comments in the
source and from the test suite.

**Last reviewed:** 2026-11-21, against the working tree based on commit
`7f9be2a` plus the uncommitted `Balancer`/pool refactor and the new
`internal/circuitbreaker` package.

Fixed items have been moved to [Resolved](#resolved) so the rest of the document
focuses on what is left.

## Status legend

- **Fixed** — the behavior described no longer occurs.
- **Partial** — part of the problem or part of the suggested change landed.
- **Open** — no effective change landed, or the attempted change is broken.
- **Blocked** — the fix depends on another issue being resolved first.

## Current verification

Checked on 2026-11-21:

| Check | Result |
| --- | --- |
| `gofmt -l .` | Clean |
| `go build ./...` | Passes |
| `go vet ./...` | Fails: `internal/balancer/balancer_test.go` references removed `LoadBalance` and `serverPool` |
| `go test ./...` | Fails: root package passes; `internal/balancer` does not compile; `internal/circuitbreaker` has no tests |
| `go run .` | Starts without panicking, but `GET /` returns `503 Service Unavailable` because all backend URLs are empty |

## Priority overview

| ID | Severity | Area | Summary | Status |
| --- | --- | --- | --- | --- |
| P0-3 | Critical | Configuration | Backend URLs in `main.go` are still empty and are not validated. | Open |
| P1-6 | High | Selection | Fallback selection repeats alive peers and updates the cursor non-atomically. | Partial |
| P1-8 | High | Health checks | The initial check exists, but the health check itself is broken. | Partial / blocked |
| P1-9 | High | Retries | Retries still reuse a consumed body for server requests and non-idempotent methods. | Partial |
| P1-10 | High | Retries | Retries still target the same failing backend. | Open |
| P1-11 | High | Error handling | After retries are exhausted, the proxy writes no response; clients can receive an empty 200. | Open |
| P2-13 | Medium | Design | Backend is decoupled from the balancer, but failover and error responses were not moved into `Balancer`. | Partial |
| P2-14 | High | Health checks | The HTTP health-check attempt has an inverted error check and a nil-request panic path. | Open |
| P2-16 | Low | Cleanup | Many comments and names are stale after the refactor. | Partial |
| P2-17 | Medium | Circuit breaker | `internal/circuitbreaker` is incomplete, unsynchronized, and not wired in. | Open |
| P3-17 | High | Testing | The balancer tests do not compile against the new API. | Open |
| P3-18 | Low | Project hygiene | Config, logging, metrics, CI, and packaging are still missing. | Open |

---

## Remaining work

### P0-3: Backend URLs are empty placeholders

**Status:** Open.

`main.go` still contains four empty strings:

```go
urls := []string{
    "", "", "", "",
}
```

`url.Parse("")` succeeds, so construction does not fail, but the resulting
backends have no host. `NewBackend` starts a health-check goroutine for each;
the current health-check bug then marks every backend dead. The server starts
but every request returns 503.

**Impact:** The service cannot serve any traffic, even though it appears to
start successfully.

**Fix:** Populate real backend URLs and validate each one before constructing a
backend:

```go
for _, raw := range urls {
    u, err := url.Parse(raw)
    if err != nil || u.Scheme == "" || u.Host == "" {
        return nil, fmt.Errorf("invalid backend URL %q", raw)
    }
}
```

Prefer flags, environment variables, or a config file over editing source. See
P3-18.

**Acceptance:** Invalid or empty URLs fail fast with a clear error; a valid list
produces usable backends and at least one request reaches an upstream.

---

### P1-6: Fallback selection repeats peers and updates the cursor non-atomically

**Status:** Partial. The first index is now `0`, and a fallback scan stores the
normalized index instead of the raw loop index. Two problems remain.

**Repeated peers:** When a fallback scan finds an alive peer, `NextPeer` stores
that peer's index. On the next call, `NextIndex` computes
`(Add(1) - 1) % n`, which returns the same index again. For a pool of
`[dead, alive, dead, alive]`, the current sequence is:

```text
b1, b1, b3, b3, b1, b1, ...
```

`TestServerPoolNextPeerSkipsDead` currently documents this behavior.

**Non-atomic cursor update:** `Add` followed by `Store` is two operations.
Concurrent requests can clobber each other's cursor updates even though each
operation is atomic on its own.

**Fix:** Keep the scan local and avoid writing the cursor from inside it:

```go
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

If the cursor must jump to a selected backend, use a CAS loop and document the
trade-off.

**Acceptance:** One full cycle visits every alive backend exactly once; the
cursor stays in `[0, n)`; `go test -race` is clean under concurrent selection;
the duplicate-selection expectation in `pool_test.go` is removed.

---

### P1-8: The initial health check exists but is broken

**Status:** Partial / blocked by P2-14.

`NewBackend` starts a backend with `Alive == false`, and
`startPeriodicalHealthChecks` now runs one check immediately before starting the
ticker. That resolves the original "no check until the first tick" problem in
structure, but P2-14 means the check always marks the backend dead. Every
backend is therefore unhealthy from startup.

The interval is also still hardcoded to one minute with no jitter.

**Fix:** Fix P2-14 first. Then make the interval and timeout configurable and
add jitter so checks do not synchronize across backends.

**Acceptance:** A healthy backend becomes alive immediately after startup and
stays alive across ticks; a canceled context stops the loop; checks are
configurable.

---

### P1-9: Retries still reuse a consumed request body

**Status:** Partial.

The error handler calls `r.GetBody()` and replaces `r.Body` when `GetBody` is
non-nil. That helps client-side requests and tests, but requests received by an
`http.Server` normally have `GetBody == nil`, so production retries can still
send an empty or truncated body.

Retries are also still allowed for non-idempotent methods (`POST`, `PUT`,
`PATCH`, `DELETE`), and there is no guard against retrying after response bytes
have been written.

**Fix:** Pick one documented behavior:

1. Retry only idempotent methods (`GET`, `HEAD`, `OPTIONS`, `TRACE`).
2. Buffer or reconstitute the body before retrying, and only retry before any
   response bytes reach the client.
3. Do not retry in the proxy at all; fail over at the handler level before
   writing a response.

**Acceptance:** Retried requests carry the original body, or body-bearing and
non-idempotent requests are not retried. The guarantee is documented in the
README.

---

### P1-10: Retries target the same failing backend

**Status:** Open.

`ErrorHandler` sleeps 10 ms and retries the same backend up to three times
before marking it dead. Connection-refused and timeout errors rarely recover in
10 ms, so these retries add roughly 30 ms of latency and delay eviction.

**Fix:** Attempt each backend once and fail over to another peer. The natural
place for that loop is `Balancer.ServeHTTP` (see P2-13 and P1-11):

```go
for attempt := 0; attempt < maxAttempts; attempt++ {
    peer := pool.NextPeer()
    if peer == nil {
        break
    }
    // Try the peer once. On error, mark it dead and continue.
}
http.Error(w, "Service not available", http.StatusServiceUnavailable)
```

**Acceptance:** A failed backend is skipped after one attempt; retries go to a
different peer; there is no sleep-based retry of the same backend.

---

### P1-11: Failed proxy requests can return an empty 200

**Status:** Open (new finding).

After the retry limit is reached, `ErrorHandler` does only this:

```go
backend.SetAlive(false)
```

It does not write a status code or body, and the reverse proxy then returns.
Because no response has been written, net/http can send `200 OK` with an empty
body for a request that never reached an upstream.

**Impact:** Clients see a successful status for a failed request. Load balancers
and retry logic upstream will treat the failure as success.

**Fix:** Write an explicit error response before returning:

```go
http.Error(w, "Bad Gateway", http.StatusBadGateway)
```

Better: move failure handling into `Balancer`, which can choose another peer
before writing anything and then write a single 502/503 if all peers fail.

**Acceptance:** A request routed to an unreachable upstream never returns 200;
it returns a documented 502/503 (or succeeds through failover). Add a test with
an unreachable upstream.

---

### P2-13: Failover and error responses are not owned by `Balancer`

**Status:** Partial.

The refactor removed the recursive `LoadBalance` call from the proxy error
handler, so `Backend` no longer depends on the global balancer. That part is
fixed.

However, failover was not moved anywhere. `Balancer.ServeHTTP` selects one peer,
calls its reverse proxy, and returns. If that proxy fails, the error handler
marks the backend dead and writes nothing (P1-11). Retry policy is still split
between the proxy and no handler-level logic.

**Fix:** Give `Balancer` one attempt per peer and make the backend proxy report
failure without writing a partial response. Buffer the response if retrying
after a partial write is required; otherwise document that retries only cover
connection-level failures before headers are sent.

**Acceptance:** `backend.go` contains no load-balancing or failover decisions;
`Balancer` owns peer selection, attempt count, and the final 502/503; a fake
pool can exercise the failover path in tests.

---

### P2-14: The HTTP health check is broken

**Status:** Open (broken attempt).

The health check was changed from a TCP dial to an HTTP request, but the error
condition is inverted:

```go
req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, b.Url.String(), nil)
if err == nil {
    b.SetAlive(false)
    return err
}

res, err := b.client.Do(req)
```

Consequences:

- For every valid URL, `NewRequestWithContext` succeeds, so the backend is
  marked dead and `performHealthCheck` returns without sending a request.
- If request construction fails, `req` is nil and `client.Do(nil)` is called,
  which panics.
- The check uses `context.Background()` instead of `b.ctx`, so shutting down a
  backend does not cancel an in-flight check.
- The timeout is hardcoded to 2 seconds, there is no configurable health path,
  and `http.DefaultClient` is shared.
- The function's comment still describes a TCP dial.

**Fix:**

```go
func (b *Backend) performHealthCheck(ctx context.Context) error {
    checkCtx, cancel := context.WithTimeout(ctx, b.checkTimeout)
    defer cancel()

    req, err := http.NewRequestWithContext(
        checkCtx, http.MethodGet, b.healthURL(), nil,
    )
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

Pass the backend context into the check, add configurable `healthPath`,
`checkTimeout`, and `checkInterval`, and add tests for success, non-2xx, and
connection failure.

**Acceptance:** A 2xx response marks the backend alive; a non-2xx response,
timeout, or connection error marks it dead; no request is skipped or sent with
a nil request; canceling the backend context cancels the check.

---

### P2-16: Comments and names are stale after the refactor

**Status:** Partial. The `Sever pool` typo is fixed; the rest remains.

Stale comments to fix:

- `Backend.ctx` still says it is never assigned; `NewBackend` now assigns it.
- `NewBackend` says a backend starts not alive and is first checked after one
  tick; an immediate check now runs.
- `startPeriodicalHealthChecks` says it performs a TCP check on the first tick.
- `performHealthCheck` says it attempts a TCP connection.
- `balancer.go` still has a `LoadBalance` comment above `Balancer.ServeHTTP`
  and mentions resolved `P1-4`/`P1-5` issues.
- `Balancer` and `NewBalancer` have no doc comments.
- `InitializePool` says it replaces the global pool; it now constructs and
  returns a pool.
- `NextIndex` says the first index is `1` and that it panics when empty.
- `NextPeer` says the raw loop index is stored; it now stores the normalized
  index.
- `ErrorHandler` says it asks the pool for another peer; that call was removed.
- `main.go` says the server does not shut down gracefully; it now does.

Other cleanup:

- Remove `AttemptKey` and `GetAttemptsFromContext`, or actually record attempts.
- Rename `Url` to `URL`, `mux` to `mu`, and `InitializePool` to something like
  `NewServerPool`.
- Replace `fmt.Println("Stopped ticker loop.")` with `log.Println` or remove it.
- Drop the trailing `\n` from `log.Printf("[%s] %s\n", ...)`.
- Replace `fmt.Sprintf(":%d", 8080)` with `":8080"` or a config value.
- Avoid shadowing the package in `main.go`:
  `balancer := balancer.NewBalancer(pool)`.

**Acceptance:** `go vet` is clean, comments match behavior, names follow Go
conventions, and log output has no duplicate newlines.

---

### P2-17: The circuit breaker is incomplete and unwired

**Status:** Open (new finding).

`internal/circuitbreaker/breaker.go` was added but is not used by the balancer.
It also has correctness issues:

```go
var (
    Open       BreakerState
    Closed     BreakerState
    HalfOpened BreakerState
)
```

- All three states are zero-value strings (`""`), so they are indistinguishable
  and mutable. Use a typed `const` with distinct values and a `String` method.
- `startHalfOpenCountdown` uses `<-time.Tick(...)`, which leaks a ticker. Use
  `time.NewTimer` or `time.After`.
- `state` is read and written without a mutex or atomic, so concurrent use will
  race.
- There is no `Execute`, `Allow`, or `Record` method; `exec` and
  `BreakerCtxExec` are unused.
- The package has no tests, and nothing constructs a breaker.

**Fix:** Decide whether the breaker is part of the current scope. If yes:

1. Define distinct states as constants.
2. Protect state with a mutex or atomics.
3. Replace `time.Tick` with a timer and make the countdown cancelable.
4. Expose an `Execute(ctx, input)` method that runs `exec` and records
   success/failure.
5. Wire one breaker per backend or per backend group.
6. Add tests for closed → open → half-open → closed transitions.

If it is not in scope, remove the package until it is.

**Acceptance:** The package is either fully implemented, tested, and used, or
deleted. No unsynchronized state and no leaked tickers.

---

### P3-17: Tests do not compile against the current API

**Status:** Open.

`internal/balancer/balancer_test.go` still targets the removed global API:

```text
undefined: LoadBalance
undefined: serverPool
```

Required updates:

- `InitializePool` now returns `*ServerPool`; build a handler with
  `NewBalancer(pool)`.
- Replace `TestLoadBalanceNoAliveBackends` and `TestLoadBalanceSuccess` with
  tests that call `Balancer.ServeHTTP`.
- Replace `TestLoadBalanceNilPool` with `NewBalancer(nil)` and a 503 assertion.
- Keep the health-check characterization test only until P2-14 is fixed, then
  replace it with real success/non-2xx/error tests.
- Add tests for the error path from P1-11 and the failover path from P2-13.
- Add circuit-breaker tests if P2-17 is kept.
- Run `go test ./... -race` and `go vet ./...` in CI.

**Acceptance:** `go test ./... -race`, `go vet ./...`, and `gofmt -l .` pass.
Every fixed issue has a regression test, and characterization tests are
updated in the same change as the fix.

---

### P3-18: Project hygiene

**Status:** Open.

- Externalize configuration: backend URLs and listen address.
- Add request logging with request ID, selected backend, status, and latency.
- Add metrics and a readiness endpoint that reflects backend health.
- Add a `Makefile` with `build`, `test`, `test-race`, `vet`, and `fmt` targets.
- Add a `Dockerfile` and a minimal CI workflow.
- Document retry/failover guarantees and the health-check contract.
- Update `README.md` whenever behavior changes; it currently lags behind the
  code.
- Consider a license file if the project will be shared.

---

## Resolved

The following items no longer need work. They are kept here so old references
still resolve.

| ID | Resolution |
| --- | --- |
| P0-1 | `NewBackend` now assigns `ctx`; `go run .` no longer panics in the health-check goroutine. |
| P0-2 | `generateBackendsFromUrls` appends into a zero-length slice; `TestGenerateBackendsFromUrlsValid` covers the regression. |
| P1-4 | `Balancer.ServeHTTP` returns after a successful proxy; the 503 body is no longer appended. |
| P1-5 | A nil pool returns 503; `log.Fatal` is gone from the request path. |
| P1-7 | `NextIndex` guards an empty pool and `NextPeer` returns nil. |
| P2-11 | `ListenAndServe` runs in a goroutine and `Shutdown` runs on signal. |
| P2-12 | Server read/write/idle timeouts and reverse-proxy transport timeouts are configured. |
| P2-15 | The package-level global pool and `GetServerPool` were removed; the pool is injected into `Balancer`. |

---

## Suggested implementation order

1. **Restore test signal:** P3-17. Rewrite `balancer_test.go` for the
   `Balancer` API so `go test` and `go vet` run again.
2. **Make the service actually work:** P2-14 + P1-8 (health checks) and P0-3
   (real, validated backend configuration).
3. **Fix the failure path:** P1-11, P1-9, P1-10, and P2-13. Define one retry
   and failover policy and make sure failures never return an empty 200.
4. **Fix selection fairness:** P1-6. Remove the duplicate selection and the
   non-atomic cursor update.
5. **Finish or remove the breaker:** P2-17, then P2-16 cleanup.
6. **Project hygiene:** P3-18.

## Definition of done

- A valid backend list is required and validated at startup.
- `go run .` serves traffic without panics; at least one upstream receives a
  request.
- `go test ./... -race`, `go vet ./...`, and `gofmt -l .` pass.
- No request to a failed upstream returns an empty 200.
- Health checks send real HTTP requests, honor context cancellation, and are
  configurable.
- Retry and failover semantics are documented and covered by tests.
- Selection visits each alive backend once per cycle under `-race`.
- The circuit breaker is either implemented, tested, and wired in, or removed.
- `README.md` matches the implemented behavior.
