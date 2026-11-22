# Improvement plan

This document is a companion to [README.md](README.md). It records what is
still wrong or unfinished in the current implementation, why it matters, and a
proposed order for fixing it. Issue IDs are referenced from comments in the
source and from the test suite.

**Last reviewed:** 2026-11-22, against the working tree based on commit
`d7cfd03` plus the documentation/comment cleanup.

Fixed items have been moved to [Resolved](#resolved) so the rest of the document
focuses on what is left.

## Status legend

- **Fixed** — the behavior described no longer occurs.
- **Partial** — part of the problem or part of the suggested change landed.
- **Open** — no effective change landed, or the attempted change is broken.
- **Blocked** — the fix depends on another issue being resolved first.

## Current verification

Checked on 2026-11-22:

| Check | Result |
| --- | --- |
| `gofmt -l .` | Clean |
| `go build ./...` | Passes |
| `go vet ./...` | Passes |
| `go test ./...` | Passes |
| `go test ./... -race` | Passes |
| `go run .` | Starts and serves, but `GET /` returns `503` because all backend URLs are empty |
| Package tests | `internal/circuitbreaker` and `pkg` have no tests |

## Priority overview

| ID | Severity | Area | Summary | Status |
| --- | --- | --- | --- | --- |
| P0-3 | Critical | Configuration | Backend URLs in `main.go` are still empty and are not validated. | Open |
| P1-9 | High | Retries | Retries still reuse a consumed body for server requests and non-idempotent methods. | Partial |
| P1-10 | High | Retries | Retries still target the same failing backend. | Open |
| P2-13 | Medium | Design | Backend is decoupled from the balancer, but failover is not owned by `Balancer`. | Partial |
| P2-14 | Medium | Health checks | HTTP checks work, but interval, timeout, path, and context handling are fixed. | Partial |
| P2-16 | Low | Cleanup | Code-level naming and logging cleanup remains after the comment pass. | Partial |
| P2-17 | Medium | Circuit breaker | `internal/circuitbreaker` is incomplete, unsynchronized, and not wired in. | Open |
| P3-17 | Low | Testing | Stale tests are fixed; coverage is still thin. | Partial |
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
backends have no host. The health check sends a request with an empty URL,
which fails, so every backend is marked dead and every request returns 503.

**Impact:** The service cannot serve traffic, even though it starts cleanly.

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

`ErrorHandler` retries the same backend up to three times with linear backoff
(10 ms, 20 ms, 30 ms) before marking it dead and writing 502. Connection-refused
and timeout errors rarely recover in that window, so the retries add latency and
delay eviction.

**Fix:** Attempt each backend once and fail over to another peer. The natural
place for that loop is `Balancer.ServeHTTP` (see P2-13):

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
different peer; there is no fixed sleep-retry loop against the same backend.

---

### P2-13: Failover is not owned by `Balancer`

**Status:** Partial.

The refactor removed the recursive `LoadBalance` call from the proxy error
handler, so `Backend` no longer depends on a global balancer. That coupling is
fixed.

However, failover was not moved anywhere. `Balancer.ServeHTTP` selects one peer,
calls its reverse proxy, and returns. If that proxy fails, the error handler
writes 502 and marks the backend dead; the request is not retried against a
different peer. Retry policy is still split between the proxy and no
handler-level logic.

**Fix:** Give `Balancer` one attempt per peer and make the backend proxy report
failure without writing a partial response. Buffer the response if retrying
after a partial write is required; otherwise document that retries only cover
connection-level failures before headers are sent.

**Acceptance:** `backend.go` contains no load-balancing or failover decisions;
`Balancer` owns peer selection, attempt count, and the final 502/503; a fake
pool can exercise the failover path in tests.

---

### P2-14: Health-check settings are fixed

**Status:** Partial. The check now sends a real HTTP request and the inverted
error condition is fixed.

Current behavior:

- `performHealthCheck` sends `GET` to `b.Url.String()` with a 2-second timeout.
- Any 2xx response marks the backend alive.
- Request errors, timeouts, and non-2xx responses mark it dead.
- `startPeriodicalHealthChecks` runs an immediate check, then repeats using
  `pkg.Jitter` around `healthCheckInterval`.

Remaining issues:

- `checkInterval` is hardcoded to 2 seconds in `NewBackend`.
- The timeout is hardcoded to 2 seconds.
- There is no configurable health path; the check always hits the backend root.
- The check uses `context.Background()` instead of the backend context, so
  cancellation does not abort an in-flight request.
- The shared `http.DefaultClient` is used rather than a client with explicit
  health-check settings.

**Fix:** Add `healthPath`, `checkTimeout`, and a configurable interval to the
backend. Derive the check context from `b.ctx`:

```go
checkCtx, cancel := context.WithTimeout(b.ctx, b.checkTimeout)
defer cancel()
```

Use a dedicated client if health checks need different transport settings.

**Acceptance:** A 2xx response marks the backend alive; a non-2xx response,
timeout, or connection error marks it dead; canceling the backend context
cancels an in-flight check; interval, timeout, and path are configurable.

---

### P2-16: Code-level naming and logging cleanup

**Status:** Partial. The stale comments have been rewritten; the remaining
items are code-level cleanups:

- Remove `AttemptKey` and `GetAttemptsFromContext`, or actually record attempts.
- Rename `Url` to `URL` and `mux` to `mu`.
- Rename `InitializePool` to something like `NewServerPool`.
- Rename or remove `SelectedBacked` (misspelled and unused by the balancer).
- Replace `fmt.Println("Stopped ticker loop.")` with `log.Println` or remove it.
- Drop the trailing `\n` from `log.Printf("[%s] %s\n", ...)`.
- Replace `fmt.Sprintf(":%d", 8080)` with `":8080"` or a config value.
- Avoid shadowing the package in `main.go`:
  `balancer := balancer.NewBalancer(pool)`.
- Reconsider the generic package name `pkg`.

**Acceptance:** `go vet` stays clean, names follow Go conventions, no unused
identifiers remain, and log output has no duplicate newlines.

---

### P2-17: The circuit breaker is incomplete and unwired

**Status:** Open.

`internal/circuitbreaker/breaker.go` is a scaffold. It is not used by the
balancer and has correctness issues:

- `Open`, `Closed`, and `HalfOpened` are zero-value `var`s, so all three are
  indistinguishable empty strings.
- `startHalfOpenCountdown` uses `<-time.Tick(...)`, which leaks a ticker.
- `state` is read and written without a mutex or atomic.
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

### P3-17: Test coverage is still thin

**Status:** Partial. The stale tests now compile and pass, including `-race`.

Remaining coverage:

- `internal/circuitbreaker` and `pkg` have no tests.
- Add a failover test once P2-13 moves retries into `Balancer`.
- Add a test that a failed proxy request returns 502 and marks the backend dead.
- Add a context-cancellation test for health checks once P2-14 derives the check
  context from the backend context.
- Run `go test ./... -race` and `go vet ./...` in CI.

**Acceptance:** Every package has tests; fixed issues have regression tests; the
race detector and vet run in CI.

---

### P3-18: Project hygiene

**Status:** Open.

- Externalize configuration: backend URLs and listen address.
- Add request logging with request ID, selected backend, status, and latency.
- Add metrics and a readiness endpoint that reflects backend health.
- Add a `Makefile` with `build`, `test`, `test-race`, `vet`, and `fmt` targets.
- Add a `Dockerfile` and a minimal CI workflow.
- Document retry/failover guarantees and the health-check contract.
- Update `README.md` whenever behavior changes.
- Consider a license file if the project will be shared.

---

## Resolved

The following items no longer need work. They are kept here so old references
still resolve.

| ID | Resolution |
| --- | --- |
| P0-1 | `NewBackend` assigns `ctx`; startup no longer panics. |
| P0-2 | `generateBackendsFromUrls` appends into a zero-length slice; a valid-URL regression test covers it. |
| P1-4 | `Balancer.ServeHTTP` returns after a successful proxy; the 503 body is not appended. |
| P1-5 | A nil pool returns 503; `log.Fatal` is gone from the request path. |
| P1-6 | `NextPeer` uses a CAS loop, scans one full cycle, and advances past the selected backend; no duplicate selection or `Add`/`Store` race remains. |
| P1-7 | An empty pool returns nil instead of dividing by zero. |
| P1-8 | An immediate startup health check runs, followed by jittered intervals. |
| P1-11 | After retries, the proxy writes `502 Bad Gateway` and marks the backend dead instead of returning an empty 200. |
| P2-11 | `ListenAndServe` runs in a goroutine and `Shutdown` runs on signal. |
| P2-12 | Server and reverse-proxy transport timeouts are configured. |
| P2-15 | The package-level global pool was removed; the pool is injected into `Balancer`. |

---

## Suggested implementation order

1. **Make the service useful:** P0-3 and the config portion of P3-18. Add
   validated, externalized backend URLs.
2. **Fix the failure path:** P1-10, P1-9, and P2-13. Move retries/failover into
   `Balancer` and define the body and idempotency rules.
3. **Finish health checks:** P2-14. Make interval, timeout, path, and context
   configurable.
4. **Finish or remove the breaker:** P2-17.
5. **Code cleanup:** P2-16.
6. **Testing and hygiene:** P3-17 and P3-18.

## Definition of done

- A valid backend list is required, validated, and configurable.
- `go run .` serves traffic without panics; at least one upstream receives a
  request.
- `go test ./... -race`, `go vet ./...`, and `gofmt -l .` pass.
- Retry and failover behavior is documented and covered by tests.
- Health checks honor context cancellation and are configurable.
- The circuit breaker is either implemented, tested, and wired in, or removed.
- `README.md` matches the implemented behavior.
