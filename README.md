# simple-load-balancer

A small HTTP load balancer and reverse proxy written in Go using only the
standard library. It is intended to distribute requests across a fixed set of
upstream servers with round-robin selection, skip backends that fail a periodic
health check, and retry failed upstream requests.

> **Status: early prototype and currently not serving traffic.** The code
> builds, but `go test ./...` fails because the balancer tests still target the
> old API. `go run .` starts, then returns `503 Service Unavailable` for every
> request because the backend list is empty and the health check is broken.
> See [IMPROVEMENTS.md](IMPROVEMENTS.md) for the remaining work.

## Current behavior

- Serves HTTP on `:8080` using `*balancer.Balancer`, which implements
  `http.Handler`.
- Builds one `Backend` per hardcoded URL in `main.go`. Each backend owns an
  `httputil.ReverseProxy` and a tuned `http.Transport`.
- Selects backends with a shared atomic cursor (round-robin), skipping entries
  whose `Alive` flag is false.
- Retries a failed upstream request up to three times (10 ms apart) against the
  same backend. When the retries are exhausted it marks that backend dead.
- Returns `503 Service Unavailable` when no healthy backend is available.
- Runs an HTTP health check per backend immediately, then every minute.
- Contains an `internal/circuitbreaker` package that is not wired into the
  balancer yet.

The four backend URLs in `main.go` are empty placeholders, and the listen
address is hardcoded. Configuration is not externalized.

## How it works

```text
client
  │
  ▼
http.Server (:8080)
  │
  ▼
balancer.Balancer.ServeHTTP
  │  pool.NextPeer() ── round-robin, skips !Alive
  ▼
Backend.ReverseProxy ──► upstream server
  │
  ├── success → return the upstream response
  └── failure → retry same backend (≤3), then SetAlive(false)
```

Current failure-path caveat: after the retries are exhausted, the proxy error
handler marks the backend dead and returns without writing a response. Clients
can therefore receive an empty `200 OK` for a request that never reached an
upstream (tracked as P1-11).

### Health checks

```text
NewBackend
  └── goroutine
        ├── performHealthCheck() immediately
        └── performHealthCheck() every 1 minute
              └── HTTP GET backend.Url.String() with a 2 s timeout
```

Caveat: the current health check has an inverted error check
(`if err == nil { SetAlive(false); return }`), so it marks every backend dead
without sending the request. That is why the initial check does not make the
service usable yet (tracked as P2-14).

## Project layout

```text
.
├── main.go                          # wiring: signal context, backends, pool, HTTP server
├── main_test.go                     # backend-list construction tests
├── go.mod
└── internal/
    ├── balancer/
    │   ├── backend.go               # Backend type, reverse proxy, health checks
    │   ├── backend_test.go          # alive flag, current health-check behavior, URL parsing
    │   ├── balancer.go              # Balancer handler and request-context helpers
    │   ├── balancer_test.go         # handler and context tests (currently stale)
    │   ├── pool.go                  # ServerPool and round-robin selection
    │   └── pool_test.go             # rotation, dead-peer skipping, edge cases
    └── circuitbreaker/
        └── breaker.go               # circuit breaker scaffold (unused)
```

### Package responsibilities

| Package/File | Responsibility |
| --- | --- |
| `main.go` | Build the backend list, construct the pool, create the balancer, start and shut down the server. |
| `internal/balancer/backend.go` | Represent an upstream, proxy to it, retry on failure, and track health. |
| `internal/balancer/balancer.go` | Choose a peer per request, return 503 when none are healthy, and read retry/attempt values from the request context. |
| `internal/balancer/pool.go` | Hold the backend slice and the round-robin cursor. |
| `internal/circuitbreaker/breaker.go` | Intended circuit breaker; not implemented or used yet. |

## Requirements

- Go 1.26.5 (see `go.mod`)
- No third-party dependencies

## Running

```sh
go run .
```

The server starts, but without valid backend URLs and a working health check
every request returns 503. To exercise the proxy you need real upstream
addresses, for example:

```go
urls := []string{
    "http://127.0.0.1:9001",
    "http://127.0.0.1:9002",
}
```

Even with valid URLs, the current health check marks every backend dead
(P2-14), so requests still return 503 until that is fixed. The URLs and listen
address are hardcoded in `main.go`; there are no flags or environment variables
yet.

## Testing

```sh
go test ./...          # run all tests
go test ./... -race    # include the race detector
go vet ./...           # static checks
gofmt -l .             # list unformatted files
```

**Current state:** the root package and its tests pass, but
`internal/balancer` does not compile because `balancer_test.go` still calls the
removed `LoadBalance` function and the removed `serverPool` global. Until that
test file is rewritten for `NewBalancer(pool)` and `Balancer.ServeHTTP`,
`go test ./...` and `go vet ./...` fail. The `circuitbreaker` package has no
tests.

The tests that do exist cover:

- Backend-list construction: empty input, invalid URLs, and the valid-URL
  regression for the old preallocation bug.
- Round-robin cursor order and dead-peer scanning.
- Empty-pool and all-dead-pool behavior.
- Concurrent peer selection and concurrent `Alive` reads/writes (`-race`).
- Current health-check behavior, which is a characterization test for the
  broken implementation.
- Request-context retry/attempt helpers.
- Backend URL parsing.

## Known limitations

This is a summary only. The full analysis, fixes, and acceptance criteria are
in [IMPROVEMENTS.md](IMPROVEMENTS.md).

1. **Empty backend list (P0-3):** the hardcoded URLs are empty and are not
   validated.
2. **Health check is broken (P2-14, blocks P1-8):** it marks every backend dead
   without sending a request, and can panic if request construction fails.
3. **Failed requests can return an empty 200 (P1-11):** after retries, the
   error handler writes no response.
4. **Retries target the same backend (P1-10):** three 10 ms retries against a
   backend that just failed, with no failover.
5. **Retries can resend a consumed body (P1-9):** server requests have
   `GetBody == nil`, and non-idempotent methods are retried.
6. **Selection can repeat peers (P1-6):** a fallback scan stores the selected
   index, which the next call selects again; `Add` + `Store` is also not atomic.
7. **Failover is not implemented in the handler (P2-13):** the backend was
   decoupled from the balancer, but `Balancer` still makes only one attempt.
8. **Tests do not compile (P3-17):** `balancer_test.go` targets the old API.
9. **Circuit breaker is unfinished (P2-17):** states are zero-value variables,
   the countdown leaks a ticker, and nothing uses the package.
10. **No configuration, logging, metrics, or CI (P3-18).**

## Related documents

- [IMPROVEMENTS.md](IMPROVEMENTS.md) — prioritized plan focused on the remaining
  work, with code sketches and acceptance criteria.
