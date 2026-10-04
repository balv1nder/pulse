# Pulse — Code Guide

A reference for how the code works, from the folder structure down to individual functions. The [README](../README.md) covers what the project is and what was measured; this guide covers **how it works**.

**Contents**

1. [The big picture](#1-the-big-picture)
2. [Life of a request](#2-life-of-a-request)
3. [Directory level](#3-directory-level)
4. [File and function level](#4-file-and-function-level)
   - [cmd/api/main.go](#cmdapimaingo--the-api-service)
   - [cmd/lb/main.go](#cmdlbmaingo--the-load-balancer)
   - [internal/limiter/limiter.go](#internallimiterlimitergo--the-distributed-rate-limiter)
   - [internal/limiter/limiter_test.go](#internallimiterlimiter_testgo--concurrency-test)
   - [internal/config/config.go](#internalconfigconfiggo--shared-helpers)
   - [Infrastructure files](#infrastructure-files)
   - [Load-test files](#load-test-files)
5. [Concurrency: who runs where](#5-concurrency-who-runs-where)
6. [Go concepts used, and where](#6-go-concepts-used-and-where)
7. [Known issues and gotchas](#7-known-issues-and-gotchas)
8. [Common tasks](#8-common-tasks)

---

## 1. The big picture

Two Go programs and three off-the-shelf services:

| Process | Source | Port | Role |
| --- | --- | --- | --- |
| Load balancer | `cmd/lb` | 8000 (published) | Single entry point; spreads requests over the API replicas; removes dead ones |
| API × 3 | `cmd/api` | 8080 (internal) | Serves items; enforces the rate limit; exposes metrics |
| Redis | `redis:7` image | 6379 (internal) | Shared state: rate-limit counters and the item cache |
| Prometheus | `prom/prometheus` image | 9090 (published) | Scrapes `/metrics` from each API every 5 s |
| k6 | `grafana/k6` image (on demand) | — | Generates load during tests |

**Key idea:** the APIs keep no state in memory that matters. Anything that must be shared between replicas (counters, cache) is in Redis. That is what makes it safe to add or remove replicas.

---

## 2. Life of a request

`curl localhost:8000/items/42 -H "X-Client-ID: alice"`, step by step:

| # | Where | What happens |
| --- | --- | --- |
| 1 | LB `http.Server` | Accepts the TCP connection; starts a **new goroutine** for this request |
| 2 | `pool.ServeHTTP` | Calls `pick()` to choose a backend |
| 3 | `pool.pick` | Atomically increments the counter, takes `counter % 3`, skips backends whose `alive` flag is false |
| 4 | `httputil.ReverseProxy` | Copies the request to `http://apiN:8080/items/42`, reusing a pooled connection from the tuned `http.Transport` |
| 5 | API `http.Server` | Accepts; new goroutine; enforces Read/Write timeouts |
| 6 | `logging` middleware | Starts a timer; wraps the response writer in `statusRecorder` |
| 7 | `ServeMux` | Matches `GET /items/{id}`; sets `r.Pattern` and the `id` path value |
| 8 | `rateLimit` middleware | Reads `X-Client-ID` → `limiter.Allow` → one Lua script call to Redis. Over limit → **429**; Redis error → **503** |
| 9 | `getItem` handler | Redis `GET item:42` (200 ms timeout). Hit → use cached bytes. Miss → `slowLookup` (5 ms) → marshal → `SET` with 30 s TTL |
| 10 | `getItem` | Sets `X-Cache`, `X-Served-By`, `Content-Type`; writes the JSON body |
| 11 | back in `logging` | Records the duration in the Prometheus histogram with labels `route="GET /items/{id}"`, `code="200"`; logs one line |
| 12 | LB → client | Reverse proxy streams the response back |

In the background, independently of requests:

- The **LB health checker** calls `GET /healthz` on each backend every 2 s.
- **Prometheus** calls `GET /metrics` on each API every 5 s.
- Each **API** logs Redis connection-pool stats every 5 s.

---

## 3. Directory level

```text
pulse/
├── cmd/                     one folder per executable (each is `package main`)
│   ├── api/main.go          the API service
│   └── lb/main.go           the load balancer
├── internal/                packages importable only from inside this module
│   ├── config/config.go     env-var helper + the shared Prometheus histogram
│   └── limiter/
│       ├── limiter.go       distributed fixed-window rate limiter
│       └── limiter_test.go  concurrency test (needs Redis on localhost)
├── loadtest/
│   ├── k6.js                constant-arrival-rate step test
│   └── run.sh               rebuild → test → CPU snapshot → summary
├── results/                 raw output of each load-test run
├── docs/CODE_GUIDE.md       this file
├── Dockerfile               one multi-stage image for both binaries
├── docker-compose.yml       the whole system on one private network
├── prometheus.yml           scrape targets
├── go.mod / go.sum          module path and pinned dependencies
├── .dockerignore            keeps .git and loadtest out of the build context
└── .gitignore
```

**Conventions:**

- `cmd/<name>` is the standard Go layout for programs; `go build ./cmd/api` builds one of them.
- `internal/` is enforced by the Go compiler: other modules can't import it.
- Folder name = package name: `internal/limiter` contains `package limiter`, imported as `github.com/balv1nder/pulse/internal/limiter`.

---

## 4. File and function level

### `cmd/api/main.go` — the API service

**Package-level state**

| Name | Type | Purpose |
| --- | --- | --- |
| `hostname` | `string` | Container hostname, read once at startup. Used in `X-Served-By` |
| `port` | `string` | Listening port, set in `main`. Used in `X-Served-By` |

**Types**

#### `type Item struct { ID, Name string }`

The JSON response body. Struct tags (`json:"id"`) set the JSON keys. Only exported (capitalised) fields are encoded.

#### `type statusRecorder struct { http.ResponseWriter; code int }`

Wraps a `ResponseWriter` to remember the status code, which `ResponseWriter` itself doesn't expose.

- **Embedding** `http.ResponseWriter` means all its methods (`Header`, `Write`) are promoted automatically; only `WriteHeader` is overridden.
- `code` starts at `200` because a handler that only calls `Write` never calls `WriteHeader` — Go sends 200 implicitly.

#### `func (r *statusRecorder) WriteHeader(code int)`

Saves `code`, then calls the embedded `WriteHeader`. Pointer receiver so the saved code is visible to the middleware afterwards.

**Functions**

#### `func writeJSON(w http.ResponseWriter, status int, v any)`

Helper: sets `Content-Type: application/json`, writes `status`, encodes `v`. Order matters — headers must be set before `WriteHeader`, and the body after. *Currently unused* (`getItem` writes pre-encoded bytes directly); kept as a utility.

#### `func getItem(rdb *redis.Client) http.HandlerFunc`

**Handler factory.** Takes its dependency (`rdb`) and returns the actual handler as a closure. This is dependency injection without a framework: handler signatures are fixed, so dependencies come in through the enclosing function.

The returned handler implements **cache-aside**:

1. Reads `id` with `r.PathValue("id")` (Go 1.22+ routing).
2. Creates a 200 ms timeout derived from `r.Context()` — if the client disconnects, Redis calls are cancelled too. `defer cancel()` releases the timer.
3. `rdb.Get(ctx, "item:"+id).Bytes()` then a `switch` on the error:

| Case | Meaning | Action |
| --- | --- | --- |
| `err == nil` | Cache hit | Use cached bytes; `X-Cache: HIT` |
| `errors.Is(err, redis.Nil)` | Cache miss (`redis.Nil` is a **sentinel error** for "key not found") | `slowLookup` → `json.Marshal` → `rdb.Set(..., 30s)`; `X-Cache: MISS` |
| anything else | Redis broken | Serve from `slowLookup` anyway (graceful degradation); log the original Redis error (saved in `redisErr` before `err` is reused) |

4. Sets `X-Served-By` and `Content-Type`, writes the bytes.

*Note:* the error from `rdb.Set` is ignored — a failed cache write is not worth failing the request.

#### `func slowLookup(id string) Item`

Stand-in for a database query: sleeps 5 ms and returns `Item{ID: id, Name: "Item-" + id}`. `time.Sleep` uses no CPU, so under load many lookups can be "in progress" at once (Little's Law).

#### `func logging(next http.Handler) http.Handler`

**Middleware** — a function that wraps a handler and returns a new handler. This one wraps the whole mux, so it sees every request.

1. Records `start`.
2. Wraps `w` in a `statusRecorder`.
3. Calls `next.ServeHTTP(rec, r)`.
4. Observes the duration in `config.ReqDuration` with labels `r.Pattern` (e.g. `GET /items/{id}`) and the status code.
5. Logs `METHOD PATH CODE DURATION`.

Why `r.Pattern` and not `r.URL.Path`: the path contains the item id, and labelling by id would create one time series per item (**cardinality explosion**). `r.Pattern` is filled in by the mux during step 3; for unmatched routes (404) it is empty.

#### `func healthz(w http.ResponseWriter, r *http.Request)`

Writes `ok\n`. Deliberately cheap and **not** behind `rateLimit`, otherwise a busy client could get health checks rate-limited and the LB would eject healthy replicas.

#### `func rateLimit(l *limiter.Limiter, next http.Handler) http.Handler`

Middleware that enforces the limiter before calling `next`.

1. Client identity = `X-Client-ID` header, or `"anonymous"`. (The IP can't be used: behind the LB every request comes from the LB's IP.)
2. `l.Allow(r.Context(), client)`.
3. Error → log + **503** (*fail closed*: if Redis is down, nothing passes). Not allowed → **429**. Otherwise → `next.ServeHTTP`.

The **policy** (fail open vs closed) lives here, in the service, not in the limiter package — the library reports errors, the caller decides.

#### `func main()`

Startup sequence, in order:

| Step | Code | Why |
| --- | --- | --- |
| 1 | `redis.NewClient(...)` | One client for the whole process; it holds a **connection pool** (default 10 connections per CPU). Never create one per request |
| 2 | `rdb.Ping` with a 2 s timeout → `log.Fatalf` on failure | Fail fast with the real error if Redis is unreachable |
| 3 | Background goroutine logging `rdb.PoolStats()` every 5 s | Shows whether requests wait for Redis connections (`timeouts`, `misses` growing; `idle` = 0) |
| 4 | `http.NewServeMux()` + routes | `/healthz` and `/metrics` plain; `/items/{id}` wrapped as `rateLimit(lim, getItem(rdb))` |
| 5 | `limiter.New(rdb, 1000, time.Second)` | 1,000 requests per client per second |
| 6 | `&http.Server{... ReadTimeout 5s, WriteTimeout 10s}` with `Handler: logging(mux)` | The default server has **no** timeouts — slow clients could hold connections forever |
| 7 | `ListenAndServe` in a goroutine | Frees `main` to wait for a signal. `http.ErrServerClosed` is the *expected* return after `Shutdown`, so it isn't fatal |
| 8 | `signal.NotifyContext(..., SIGINT, SIGTERM)` then `<-ctx.Done()` | Blocks until Ctrl+C or `docker stop` |
| 9 | `srv.Shutdown(ctx with 10 s timeout)` | **Graceful shutdown:** stop accepting connections, let in-flight requests finish, give up after 10 s |

Handler chain for `/items/{id}`:

```text
http.Server → logging → ServeMux → rateLimit → getItem
```

---

### `cmd/lb/main.go` — the load balancer

**Types**

#### `type backend struct { url *url.URL; proxy *httputil.ReverseProxy; alive atomic.Bool }`

One API replica.

- `proxy` — Go's standard reverse proxy, preconfigured for this backend's URL; it is itself an `http.Handler`.
- `alive` — written by the health-check goroutine and the proxy error handler, read by every request goroutine, so it **must** be atomic (a plain `bool` would be a data race).
- Stored as `*backend` (pointer) so every goroutine sees and updates the same instance.

#### `type pool struct { backends []*backend; next atomic.Uint64 }`

All backends plus the round-robin counter. `pool` implements `http.Handler` (it has `ServeHTTP`), so it can be passed straight to `http.Server` — Go interfaces are satisfied implicitly, with no `implements` keyword.

**Methods**

#### `func (p *pool) pick() *backend`

Round-robin with skipping:

1. `n` = number of backends.
2. Up to `n` times: `p.next.Add(1) % n` → candidate; return it if `alive`.
3. All dead → return `nil`.

`Add(1)` is a single atomic CPU instruction: thousands of concurrent request goroutines get distinct numbers with no lock. A plain `p.i++` would lose increments (data race; `go run -race` reports it).

#### `func (p *pool) ServeHTTP(w http.ResponseWriter, r *http.Request)`

Entry point for every request: `pick()`; if `nil` → **503** `Service Unavailable!`; else `b.proxy.ServeHTTP(w, r)`.

#### `func (p *pool) healthCheck(every time.Duration)`

**Active health checking**, run as a background goroutine.

- Own `http.Client` with a 1 s timeout (the default client has none; a hung backend would freeze the checker).
- Every `every` (2 s), for each backend: `GET <url>/healthz`; healthy = no error **and** status 200.
- Always closes `resp.Body` (otherwise connections leak); `resp` is `nil` when `err != nil`, hence the check.
- Logs only on state *changes* (`alive=false` / `alive=true`).
- Checks run sequentially, so one hung backend delays the others by up to 1 s.

**Functions**

#### `func main()`

1. Reads `BACKENDS` (comma-separated URLs) via `config.Getenv`.
2. For each URL: `url.Parse` (fatal on error) → `backend` with `httputil.NewSingleHostReverseProxy(u)`.
3. Replaces the proxy's `Transport` with a tuned one:

   | Field | Value | Default | Effect |
   | --- | --- | --- | --- |
   | `MaxIdleConns` | 1000 | 100 | Total idle connections kept |
   | `MaxIdleConnsPerHost` | 200 | **2** | Idle connections kept *per backend*. With the default 2, most connections are closed after each request and re-opened on the next (TCP handshake each time; sockets pile up in TIME_WAIT) |
   | `IdleConnTimeout` | 90 s | 90 s | How long an idle connection is kept |

4. Marks every backend alive at startup.
5. Sets `ErrorHandler` (**passive health check**): a closure capturing this `b`; on any proxy error it logs, marks `b` dead, and returns **502**. Since Go 1.22 each loop iteration has its own variables, so each closure captures its own backend.
6. Starts `go p.healthCheck(2 * time.Second)`.
7. Runs an `http.Server` on `:8000` with Read 5 s / Write 10 s timeouts. (No graceful shutdown on the LB.)

---

### `internal/limiter/limiter.go` — the distributed rate limiter

**Algorithm: fixed window.** Time is cut into windows of length `window`. Each client gets one counter per window; requests beyond `limit` in a window are rejected.

```text
key = "rl:<client>:<window number>"      window number = now_ms / window_ms
```

A new window number means a new key, so counts reset automatically; old keys expire.

#### `var script = redis.NewScript(...)`

```lua
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return n
```

- `INCR` increments and returns the new count **atomically** — two replicas can never both read 999.
- The first request in a window (`n == 1`) sets the expiry. Doing it only once avoids extending the expiry on every request.
- Running both commands **inside one Lua script** makes them atomic together; as two separate commands, a crash in between could leave a counter that never expires.
- `script.Run` first tries `EVALSHA` (sends only the script's hash); if Redis hasn't cached the script yet, it falls back to sending the full text once.

#### `type Limiter struct { rdb *redis.Client; limit int64; window time.Duration }`

Fields are lowercase (unexported); callers interact only via `New` and `Allow`.

#### `func New(rdb *redis.Client, limit int64, window time.Duration) *Limiter`

Constructor by convention (Go has no constructors). Returns a pointer.

#### `func (l *Limiter) Allow(ctx context.Context, client string) (bool, error)`

1. Window number = `time.Now().UnixMilli() / l.window.Milliseconds()`.
2. Key = `fmt.Sprintf("rl:%s:%d", client, windowNumber)`.
3. Runs the script with the key and the window in ms → `n`.
4. Returns `n <= limit`. On a Redis error returns `(false, err)` and **does not decide policy** — that's the caller's job.

**Known property:** fixed windows allow bursts at boundaries — up to `limit` at the end of one window and `limit` again at the start of the next (2× in a short span).

---

### `internal/limiter/limiter_test.go` — concurrency test

#### `func TestAllowIsExactUnderConcurrency(t *testing.T)`

Proves the limit is exact when many callers race (a stand-in for many replicas).

- Limit 1,000 per **10 s** window (long enough that the window can't roll over mid-test).
- Unique client name per run (`test-<nanoseconds>`), so leftovers from earlier runs don't interfere.
- 50 goroutines × 30 calls = 1,500 attempts; `sync.WaitGroup` waits for all; `atomic.Int64` counts allowed calls safely.
- Asserts exactly 1,000 allowed.
- Requires Redis on `localhost:6379`. Run with `go test -race ./...`.

---

### `internal/config/config.go` — shared helpers

#### `func Getenv(key, fallback string) string`

Returns the env var, or `fallback` if it is empty (and logs that the default was used). Because it never returns `""` when a fallback is given, checks like `if rawBackends == ""` in the LB can never trigger.

#### `var ReqDuration` (`*prometheus.HistogramVec`)

The request-latency histogram, registered automatically by `promauto`.

- Name: `http_request_duration_seconds` (Prometheus convention: base units, `_seconds`).
- Buckets: `ExponentialBuckets(0.0005, 2, 14)` → 0.5 ms, 1, 2, 4, 8, 16, 32, 64, 128, 256, 512 ms, ~1.02 s, ~2.05 s, ~4.1 s (+ `+Inf`).
- Labels: `route`, `code`.

Prometheus exposes it as `_bucket{le="…"}` (cumulative counts), `_sum` and `_count`. p99 across all replicas:

```text
histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{route="GET /items/{id}"}[30s])))
```

`histogram_quantile` interpolates linearly inside a bucket, so a quantile is only as precise as the bucket it falls in.

---

### Infrastructure files

#### `Dockerfile`

Multi-stage build, shared by both programs:

| Stage | What | Why |
| --- | --- | --- |
| `build` (`golang:1.27`) | Copy `go.mod`/`go.sum` → `go mod download` → copy source → `CGO_ENABLED=0 go build ./cmd/${CMD}` | Dependencies are a separate cached layer (re-downloaded only when `go.mod` changes). `CGO_ENABLED=0` gives a fully static binary |
| final (`distroless/static`) | Copy only `/out/app` | Tiny image, no shell or package manager → smaller attack surface |

`ARG CMD` selects which program to build (`api` or `lb`).

#### `docker-compose.yml`

- `x-api: &api` defines the API service once; `api1/api2/api3: *api` reuse it (YAML anchors).
- Containers reach each other by **service name** (`redis:6379`, `http://api1:8080`). Inside a container, `localhost` means that container.
- Redis has a `healthcheck`; APIs use `depends_on: condition: service_healthy`, because plain `depends_on` only waits for the container to *start*, and the API exits if Redis isn't answering.
- Only the LB (8000) and Prometheus (9090) publish ports to the host.
- `BACKENDS=${BACKENDS:-...}` lets you override the backend list from the shell (used for the 1-replica experiment).

#### `prometheus.yml`

Scrape interval 5 s; one job `api` with targets `api1:8080`, `api2:8080`, `api3:8080`. Prometheus **pulls** `/metrics` from each. The LB is not scraped (it has no `/metrics`).

---

### Load-test files

#### `loadtest/k6.js`

- **Executor:** `constant-arrival-rate` — an **open model**: sends `RATE` requests per second regardless of how fast responses come back, so queueing shows up as latency (a closed model would slow down with the server and hide it — *coordinated omission*).
- **Scenarios:** `warmup` (15 s) then `measure` (60 s, `startTime: 15s`); thresholds apply only to `{scenario:measure}`, so cold-start latency is excluded.
- **VUs:** `preAllocatedVUs: 1000`, `maxVUs: 3000`. Needed VUs ≈ rate × latency (Little's Law); allocating more mid-test is slow and causes `dropped_iterations`.
- **Thresholds:** p99 < 100 ms, errors < 1%, zero dropped (all for `measure`), and `http_reqs > 1000` so a broken script can't "pass" by sending nothing.
- **Request:** random id 0–999; `X-Client-ID: client-<VU % 50>` → 50 clients × 1,000 req/s = 50,000 req/s of limiter headroom, so the limiter never caps the test.
- `summaryTrendStats` adds `p(99)` to the summary (not shown by default).

#### `loadtest/run.sh <rate> <label>`

1. Refuses to run unless `k6.js` is the step test.
2. `docker compose up -d --build` (output → `results/build.log`) — always tests the code currently saved.
3. Starts k6 in the background on the `pulse_default` network (bypasses Docker Desktop's port forwarding).
4. After 45 s (30 s into the measured phase) snapshots `docker stats`.
5. Waits for k6 (which exits non-zero when a threshold fails — expected) and prints the summary lines.
6. Output: `results/<label>-<rate>.k6.txt` and `.stats.txt`.

---

## 5. Concurrency: who runs where

| Goroutine(s) | Program | Created by | Shared state touched |
| --- | --- | --- | --- |
| One per incoming request | LB | `http.Server` | `pool.next` (atomic), `backend.alive` (atomic read) |
| Health checker (1) | LB | `go p.healthCheck(...)` | `backend.alive` (atomic write) |
| Proxy error handler | LB | runs inside request goroutines | `backend.alive` (atomic write) |
| One per incoming request | API | `http.Server` | Redis client pool (internally synchronised), Prometheus histogram (internally synchronised) |
| Server goroutine (1) | API | `go func() { srv.ListenAndServe() }()` | — |
| Pool-stats logger (1) | API | `go func() { for range time.Tick ... }()` | reads `rdb.PoolStats()` |
| `main` | both | — | API: waits on `<-ctx.Done()`; LB: blocks in `ListenAndServe` |

Rule followed throughout: **any variable written by one goroutine and read by another is atomic or owned by a thread-safe library.** `go test -race` / `go run -race` check this.

---

## 6. Go concepts used, and where

| Concept | Where | One-line meaning |
| --- | --- | --- |
| Handler signature `func(w, r)` | every handler | Read from `r`, write to `w` |
| `http.Handler` interface | `pool`, middleware | Anything with `ServeHTTP` can serve HTTP |
| Middleware `func(http.Handler) http.Handler` | `logging`, `rateLimit` | Wrap a handler to add behaviour before/after |
| `http.HandlerFunc` adapter | middleware, `getItem` | Turns a plain function into an `http.Handler` |
| Closures | `getItem(rdb)`, `ErrorHandler`, middleware | Functions that capture variables from their surroundings |
| Struct embedding | `statusRecorder` | Promote an inner type's methods; override selectively |
| Implicit interfaces | `pool` as `http.Handler` | No `implements`; having the methods is enough |
| Pointer receivers | `(*pool)`, `(*Limiter)`, `(*statusRecorder)` | Methods that share/modify one instance |
| Zero values | `atomic.Bool{}`, `pool{}` | Types are usable without initialisation |
| `context.Context` | Redis calls, shutdown, signals | Deadlines and cancellation passed down call chains |
| Sentinel errors, `errors.Is` | `redis.Nil`, `http.ErrServerClosed` | Distinguish expected conditions from real failures |
| Goroutines | servers, health checker, logger | Cheap concurrent functions (`go f()`) |
| `sync/atomic` | `pool.next`, `backend.alive`, test counter | Lock-free safe shared variables |
| `sync.WaitGroup` | limiter test | Wait for a group of goroutines |
| Channels (receive) | `<-ctx.Done()` | Block until signalled |
| `defer` | `cancel()`, `wg.Done()`, `stop()` | Run cleanup when the function returns |
| Struct tags | `Item` | Control JSON field names |
| `internal/` packages | `config`, `limiter` | Compiler-enforced privacy to this module |

---

## 7. Known issues and gotchas

| Issue | Where | Effect | Fix idea |
| --- | --- | --- | --- |
| One failed check ejects a backend | `healthCheck`, `ErrorHandler` | Under overload `/healthz` times out → backend ejected → 503s (Finding 3 in README) | Require N consecutive failures; reset on success |
| All backends dead → 503 | `pick` | Slow-but-alive replicas get no traffic | "Panic mode": if none are alive, use any backend |
| Health check shares the API's CPU and server | `healthz` | Overload makes the API look dead | Longer check timeout; separate liveness port |
| Limiter fails closed | `rateLimit` | Redis outage = full `/items` outage; the cache fallback in `getItem` can never run | Fail open with local in-memory limits, or accept and document |
| Fixed-window bursts | `limiter.Allow` | Up to 2× limit across a window boundary | Sliding window (sorted set) or token bucket |
| Two Redis round trips per request | `rateLimit` + `getItem` | Extra latency and Redis load | Pipeline both in one round trip |
| `X-Client-ID` is trusted | `rateLimit` | Clients can pick any identity | Authenticated identity / API keys |
| Log line per request | `logging` | Measurable CPU + Docker log volume at thousands of req/s | Sample logs, or log only errors/slow requests |
| LB has no graceful shutdown | `cmd/lb` `main` | Stopping the LB drops in-flight requests | Same `NotifyContext` + `Shutdown` pattern as the API |
| No drain on API shutdown | `cmd/api` `main` | ~0.1% 502s when stopping a replica under load | Fail `/healthz` first, wait > 2 s, then `Shutdown` |
| Custom `Transport` built from scratch | `cmd/lb` `main` | Loses `DefaultTransport`'s dial timeout and other defaults | `t := http.DefaultTransport.(*http.Transport).Clone()`, then change the three fields |
| `BACKENDS == ""` check is dead code | `cmd/lb` `main` | `Getenv` with a fallback never returns `""` | Remove the default, or the check |
| `writeJSON` unused | `cmd/api` | — | Remove or use for error responses |
| `prometheus/client_golang` listed as `// indirect` | `go.mod` | Cosmetic | `go mod tidy` |
| Limiter test needs a live Redis | `limiter_test.go` | Fails without Redis | `t.Skip` when `Ping` fails, or use miniredis |
| Containers run old code until rebuilt | Docker | Testing stale code | `docker compose up -d --build` (done automatically by `run.sh`) |

---

## 8. Common tasks

| Task | Command / change |
| --- | --- |
| Start everything | `docker compose up --build` |
| Apply a code change | `docker compose up -d --build` |
| Watch logs | `docker compose logs -f lb` (or `api1`, …) |
| Stop one replica | `docker compose stop api2` / `docker compose start api2` |
| Run with one replica | `BACKENDS=http://api1:8080 docker compose up -d --force-recreate lb` |
| Load test at a rate | `./loadtest/run.sh 1500 mylabel` |
| Unit tests | `docker run -d -p 6379:6379 redis:7 && go test -race ./...` |
| Change the rate limit | `limiter.New(rdb, <limit>, <window>)` in `cmd/api/main.go` |
| Change cache TTL | `30*time.Second` in `getItem` |
| Change health-check interval | `go p.healthCheck(2 * time.Second)` in `cmd/lb/main.go` |
| Add a route | `mux.Handle("GET /path", handler)` in `cmd/api/main.go`; wrap with `rateLimit(...)` if it should be limited |
| Inspect Redis | `docker compose exec redis redis-cli` → `KEYS rl:*`, `PTTL item:42` |
| p99 in Prometheus | query shown under [`ReqDuration`](#var-reqduration-prometheushistogramvec) |
