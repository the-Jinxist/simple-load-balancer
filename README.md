# simple-load-balancer

A small HTTP load balancer and reverse proxy written in Go using only the
standard library. It distributes requests across a fixed set of upstream servers
with round-robin selection, skips backends that fail a periodic health check,
and retries failed upstream requests.

> **Status: early prototype.** The code builds, vets, and its tests pass. It
> starts on `:8080`, but the hardcoded backend list is still empty, so every
> request currently returns `503 Service Unavailable`. See
> [IMPROVEMENTS.md](IMPROVEMENTS.md) for the remaining work.

## Current behavior

- Serves HTTP on `:8080` using `*balancer.Balancer`, which implements
  `http.Handler`.
- Builds one `Backend` per hardcoded URL in `main.go`. Each backend owns an
  `httputil.ReverseProxy` and a tuned `http.Transport`.
- Selects backends with an atomic cursor using a CAS loop (round-robin),
  skipping entries whose `Alive` flag is false. The cursor advances past the
  selected backend, so each alive backend is chosen once per cycle.
- Runs an HTTP health check per backend immediately at startup, then every
  roughly two seconds with jitter.
- Retries a failed upstream request up to three times against the same backend
  with linear backoff (10 ms, 20 ms, 30 ms). After the retries, it writes
  `502 Bad Gateway` and marks that backend dead.
- Returns `503 Service Unavailable` when no healthy backend is available.
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
  │  pool.NextPeer() ── CAS round-robin, skips !Alive
  ▼
Backend.ReverseProxy ──► upstream server
  │
  ├── success → return the upstream response
  └── failure → retry same backend (≤3, linear backoff)
                → 502 Bad Gateway, SetAlive(false)
```

### Health checks

```text
NewBackend
  └── goroutine
        ├── performHealthCheck() immediately
        └── every ~2 seconds (jittered)
              └── HTTP GET backend URL with a 2 s timeout
                    ├── 2xx       → SetAlive(true)
                    └── error or non-2xx → SetAlive(false)
```

Jitter comes from `pkg.Jitter`, which varies each interval by roughly ±10–20%
so backends do not check in lockstep forever.

## Project layout

```text
.
├── main.go                          # wiring: signal context, backends, pool, HTTP server
├── main_test.go                     # backend-list construction tests
├── go.mod
├── internal/
│   ├── balancer/
│   │   ├── backend.go               # Backend type, reverse proxy, health checks
│   │   ├── backend_test.go          # alive flag, health checks, URL parsing
│   │   ├── balancer.go              # Balancer handler and request-context helpers
│   │   ├── balancer_test.go         # handler and context tests
│   │   ├── pool.go                  # ServerPool and CAS round-robin selection
│   │   └── pool_test.go             # rotation, dead-peer skipping, edge cases
│   └── circuitbreaker/
│       └── breaker.go               # circuit breaker scaffold (unused)
└── pkg/
    └── jitter.go                    # duration jitter helper
```

### Package responsibilities

| Package/File | Responsibility |
| --- | --- |
| `main.go` | Build the backend list, construct the pool, create the balancer, start and shut down the server. |
| `internal/balancer/backend.go` | Represent an upstream, proxy to it, retry on failure, and track health. |
| `internal/balancer/balancer.go` | Choose a peer per request, return 503 when none are healthy, and read retry/attempt values from the request context. |
| `internal/balancer/pool.go` | Hold the backend slice and the atomic round-robin cursor. |
| `internal/circuitbreaker/breaker.go` | Intended circuit breaker; not implemented or used yet. |
| `pkg/jitter.go` | Randomize durations used by the health-check loop. |

## Requirements

- Go 1.26.5 (see `go.mod`)
- No third-party dependencies

## Running

```sh
go run .
```

The server starts, but with the empty backend list every request returns 503.
To exercise the proxy you need real upstream addresses, for example:

```go
urls := []string{
    "http://127.0.0.1:9001",
    "http://127.0.0.1:9002",
}
```

The URLs and listen address are hardcoded in `main.go`; there are no flags or
environment variables yet.

## Testing

```sh
go test ./...          # run all tests
go test ./... -race    # include the race detector
go vet ./...           # static checks
gofmt -l .             # list unformatted files
```

All packages currently pass. `internal/circuitbreaker` and `pkg` have no tests
yet.

The suite covers:

- Backend-list construction: empty input, invalid URLs, and the valid-URL
  regression for the old preallocation bug.
- CAS round-robin order, dead-peer skipping, cursor advancement, and empty/all-
  dead pools.
- Concurrent peer selection and concurrent `Alive` reads/writes (`-race`).
- Health-check success, connection failure, and non-2xx responses.
- Balancer success, nil-pool, and no-healthy-peer responses.
- Request-context retry/attempt helpers.
- Backend URL parsing.

## Known limitations

This is a summary only. The full analysis and acceptance criteria are in
[IMPROVEMENTS.md](IMPROVEMENTS.md).

1. **Empty backend list (P0-3):** the hardcoded URLs are empty and are not
   validated.
2. **Retries target the same backend (P1-10):** three attempts against the
   backend that just failed, with linear backoff; there is no failover.
3. **Retries can resend a consumed body (P1-9):** server requests have
   `GetBody == nil`, and non-idempotent methods are retried.
4. **Failover is not implemented in the handler (P2-13):** the backend was
   decoupled from the balancer, but `Balancer` still makes only one attempt.
5. **Health-check settings are fixed (P2-14):** interval and timeout are
   hardcoded, the health path is not configurable, and the check ignores the
   backend context.
6. **Cleanup remains (P2-16):** naming (`Url`, `mux`, `SelectedBacked`), logging,
   and the unused `AttemptKey` deserve a pass.
7. **Circuit breaker is unfinished (P2-17):** states are zero-value variables,
   the countdown leaks a ticker, and nothing uses the package.
8. **No configuration, logging, metrics, or CI (P3-18).**

## Related documents

- [IMPROVEMENTS.md](IMPROVEMENTS.md) — prioritized plan for the remaining work,
  with code sketches and acceptance criteria.
