# simple-load-balancer

A small HTTP load balancer and reverse proxy written in Go using only the
standard library. It is intended to distribute requests across a fixed set of
upstream servers with round-robin selection, skip backends that fail a periodic
health check, and retry failed upstream requests.

> **Status: early prototype.** The code builds and its tests pass, but
> `go run .` currently panics on startup and the backend list is still empty.
> This README documents the code **as it exists today**. The proposed fixes are
> in [IMPROVEMENTS.md](IMPROVEMENTS.md).

## Current behavior

- Serves HTTP on `:8080` with `balancer.LoadBalance` as the handler.
- Creates one `Backend` per hardcoded URL, each with its own
  `httputil.ReverseProxy`.
- Selects backends with a shared atomic cursor (round-robin), skipping backends
  whose `Alive` flag is false.
- Runs a TCP health check per backend every minute in a background goroutine.
- Retries a failed upstream request up to three times (10 ms apart) against the
  same backend, then marks that backend dead and asks the pool for another peer.
- Returns `503 Service Unavailable` when no healthy backend is available.

The four backend URLs in `main.go` are empty placeholders, and the listen
address is hardcoded. Configuration is not externalized yet.

## How it works

```text
client
  │
  ▼
http.Server (:8080)
  │
  ▼
balancer.LoadBalance
  │  pool.NextPeer() ── round-robin, skips !Alive
  ▼
Backend.ReverseProxy ──► upstream server
  │
  └── on error: retry same peer (≤3), then SetAlive(false)
                and call LoadBalance again
```

Health checks run independently per backend:

```text
NewBackend
  └── goroutine: every 1 minute
        └── net.DialTimeout("tcp", backend.Url.Host, 2s)
              ├── success → SetAlive(true)
              └── failure → SetAlive(false)
```

A backend starts with `Alive == false` and is only checked on the first ticker
tick, so it cannot receive traffic during the first minute after startup.

## Project layout

```text
.
├── main.go                          # wiring: signal context, backends, HTTP server
├── main_test.go                     # test for empty backend-list construction
├── go.mod
└── internal/
    └── balancer/
        ├── backend.go               # Backend type, reverse proxy, health checks
        ├── backend_test.go          # alive flag, health check, URL parsing
        ├── balancer.go              # HTTP handler and request-context helpers
        ├── balancer_test.go         # handler behavior, context helpers
        ├── pool.go                  # ServerPool and round-robin selection
        └── pool_test.go             # rotation, dead-peer skipping, edge cases
```

### Package responsibilities

| File | Responsibility |
| --- | --- |
| `main.go` | Build the backend list, initialize the global pool, start the server. |
| `internal/balancer/backend.go` | Represent an upstream, proxy to it, retry on failure, and track health. |
| `internal/balancer/balancer.go` | Choose a peer per request, return 503 when none are healthy, and read retry/attempt values from the request context. |
| `internal/balancer/pool.go` | Hold the backend slice and the round-robin cursor. |

## Requirements

- Go 1.26.5 (see `go.mod`)
- No third-party dependencies

## Running

```sh
go run .
```

Before this can serve traffic you must replace the empty URL placeholders in
`main.go` with real upstream addresses that include a scheme and host, for
example:

```go
urls := []string{
    "http://127.0.0.1:9001",
    "http://127.0.0.1:9002",
}
```

Caveat: even with valid URLs, the current code panics in the health-check
goroutine because `Backend.ctx` is never assigned (see
[IMPROVEMENTS.md](IMPROVEMENTS.md), P0-1).

## Testing

```sh
go test ./...          # run all tests
go test ./... -race    # include the race detector
go vet ./...           # static checks
gofmt -l .             # list unformatted files
```

The suite covers the parts of the current implementation that can be exercised
in-process:

- Round-robin cursor sequence and dead-peer skipping.
- Empty-pool and all-dead-pool behavior.
- Concurrent peer selection and concurrent `Alive` reads/writes (`-race`).
- TCP health check success and failure.
- Request-context retry/attempt helpers.
- `LoadBalance` success and no-healthy-peer responses.
- Invalid backend URL handling and empty backend-list construction.

Some tests are **characterization tests** that assert current quirks so a fix
is a visible, deliberate change. They are called out in
[IMPROVEMENTS.md](IMPROVEMENTS.md#p3-17-expand-tests-and-update-characterization-tests)
and include:

- `TestServerPoolNextIndexSequence` (first index is `1`, not `0`).
- `TestServerPoolNextPeerStoresRawIndex` (cursor stores an unnormalized index).
- `TestServerPoolNextPeerEmptyPoolPanics` (empty pool divides by zero).
- `TestLoadBalanceSuccessAppendsError` (successful responses also get the 503
  body appended).

`NewBackend` cannot be tested with a valid URL today because it panics in a
background goroutine (P0-1), and `LoadBalance` with a nil pool exits the process
via `log.Fatal` (P1-5). Those branches are documented but not exercised.

## Known limitations

This is a summary only — the full analysis, fixes, and acceptance criteria are
in [IMPROVEMENTS.md](IMPROVEMENTS.md).

1. **Startup panic:** `Backend.ctx` is never set, so the health-check goroutine
   dereferences a nil context.
2. **Nil backends:** `generateBackendsFromUrls` preallocates and appends, so the
   returned slice has `len(urls)` leading `nil` entries.
3. **Empty URLs:** the hardcoded backend list contains only empty strings.
4. **Double response:** `LoadBalance` writes a 503 after every successful proxy
   because it does not return after `ServeHTTP`.
5. **Round-robin cursor:** the first selected index is `1`, and fallback scans
   store a raw index that can exceed the slice length.
6. **No graceful shutdown or server timeouts.**
7. **No config, no request logging, no metrics, no CI.**

## Related documents

- [IMPROVEMENTS.md](IMPROVEMENTS.md) — prioritized improvement plan derived from
  a code review, with code sketches and acceptance criteria.
