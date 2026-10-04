# Pulse

A horizontally scaled Go API behind a load balancer I wrote from scratch, with a Redis-backed distributed rate limiter, a cache-aside layer, Prometheus latency histograms, and k6 load tests that find where the system breaks and why.

**Headline results (MacBook Air, Docker Desktop with 8 CPUs / 4 GB):**

- **p99 = 8 ms at 1,500 req/s** sustained, 0 errors, 0 dropped requests.
- The **load balancer is the bottleneck**: it saturates at about one CPU core while the API replicas sit under half a core each.
- Under overload, **health-check timeouts caused a self-inflicted outage** (2.5% errors) — a small-scale cascading failure.

---

## Architecture

```text
                  ┌──────────────────────┐
                  │   k6 load generator  │
                  └──────────┬───────────┘
                             │ GET /items/{id}
                  ┌──────────▼───────────┐
                  │  Go load balancer    │  round-robin (atomic counter)
                  │  :8000  (cmd/lb)     │  active + passive health checks
                  └───┬──────┬───────┬───┘
            ┌─────────┘      │       └─────────┐
   ┌────────▼──────┐ ┌───────▼───────┐ ┌───────▼───────┐       ┌──────────────┐
   │ API replica 1 │ │ API replica 2 │ │ API replica 3 │ ◄──── │ Prometheus   │
   │ :8080         │ │ :8080         │ │ :8080         │scrape │ :9090        │
   └───────┬───────┘ └───────┬───────┘ └───────┬───────┘ 5 s   └──────────────┘
           └─────────────────┼─────────────────┘
                    ┌────────▼─────────┐
                    │      Redis       │  rate-limit counters (atomic Lua)
                    │                  │  item cache (30 s TTL)
                    └──────────────────┘
```

The API replicas are **stateless**: everything they share lives in Redis, so the load balancer can send any request to any replica.

## Features

| Component | What it does |
| --- | --- |
| **API** (`cmd/api`) | `GET /items/{id}` with cache-aside over a simulated 5 ms database; graceful shutdown; server timeouts |
| **Rate limiter** (`internal/limiter`) | Fixed-window, 1,000 req/s per client, shared across all replicas via one atomic Redis Lua script |
| **Load balancer** (`cmd/lb`) | Round-robin with a lock-free atomic counter; active health checks every 2 s; passive health checks on proxy errors; tuned connection pool |
| **Metrics** | Prometheus histogram `http_request_duration_seconds` labelled by route pattern and status code (low cardinality by design) |
| **Load tests** (`loadtest/`) | k6 open-model (constant arrival rate) step tests with warm-up exclusion, plus a script that captures per-container CPU mid-test |

## Quick start

Requirements: Docker Desktop. Go 1.27+ only if you want to run the code or tests outside Docker.

```bash
docker compose up --build            # Redis, 3 API replicas, LB, Prometheus

curl -i localhost:8000/items/42      # X-Served-By rotates between replicas; X-Cache: MISS then HIT
open http://localhost:9090           # Prometheus (Status → Targets: 3 APIs UP)
```

Run a load test at a fixed rate (rebuilds first, so you always test the code you saved):

```bash
./loadtest/run.sh 1500 baseline      # writes results/baseline-1500.{k6,stats}.txt
```

Run the limiter's concurrency test (needs Redis on localhost:6379):

```bash
docker run -d -p 6379:6379 redis:7
go test -race ./...
```

## API

| Endpoint | Behaviour |
| --- | --- |
| `GET /items/{id}` | Returns `{"id":…, "name":…}`. Rate-limited per `X-Client-ID` header (default `anonymous`). Headers: `X-Cache: HIT/MISS`, `X-Served-By: <host>:<port>` |
| `GET /healthz` | `ok` — used by the load balancer. Not rate-limited |
| `GET /metrics` | Prometheus metrics. Not rate-limited |

| Status | Meaning |
| --- | --- |
| `429` | Client exceeded 1,000 requests in the current 1 s window |
| `503` (API) | Redis unreachable — the limiter **fails closed** |
| `503` (LB) | No healthy backend |
| `502` (LB) | Backend unreachable during proxying |

## Configuration

| Variable | Used by | Default |
| --- | --- | --- |
| `REDIS_ADDR` | API | `localhost:6379` (Compose sets `redis:6379`) |
| `PORT` | API | `8080` |
| `BACKENDS` | LB | `http://localhost:8080,…8081,…8082` (Compose sets `api1..3`) |
| `RATE`, `BASE_URL` | k6 | `1000`, `http://localhost:8000` |

---

## Results

**Setup:** MacBook Air, Docker Desktop limited to 8 CPUs / 4 GB. k6 runs **inside the same Docker network** (avoiding Docker Desktop's port-forwarding proxy) but shares the same CPUs as the system under test. Each run: 15 s warm-up (excluded) + 60 s measured at a constant arrival rate. LB uses the tuned transport (`MaxIdleConnsPerHost: 200`). Raw output is in [`results/`](results/).

| Target rate | Served | p99 (measured) | Dropped | Pass? | LB CPU | Each API | Redis |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1,500 req/s | 1,500 | **8.0 ms** | 0 | ✅ | 39% | ~14% | 9% |
| 2,000 req/s | 1,904 | 333 ms | 7,105 | ❌ | **96%** | ~45% | 37% |
| 2,500 req/s | 2,478 | 41 ms | 961 | ❌ | **87%** | ~34% | 23% |
| 3,000 req/s | 2,916 | 156 ms | 5,953 | ❌ | **89%** | ~38% | 34% |

*Pass = p99 < 100 ms, < 1% errors, and no dropped requests during the measured phase. CPU is from `docker stats` 30 s into the measured phase (100% = one core). These runs used 200 pre-allocated k6 VUs; some drops at 2,500 req/s are a load-generator artifact (the script now pre-allocates 1,000).*

## Findings

**1. The load balancer is the bottleneck.** In every overloaded run, the LB is the busiest process, flat at roughly one core (87–96%) while load keeps rising. The APIs stay under half a core each and Redis around a third. Throughput stops scaling where the LB stops scaling.

**2. Adding replicas did not add capacity.** With the same ramp test, 1 replica served ~1,975 req/s and 3 replicas ~2,020 req/s (≈2% difference). If the APIs were the limit, three replicas would serve roughly three times as much. Every request passes through the single LB first, so that is the shared constraint.

**3. Health checks caused a self-inflicted outage.** With one replica under overload, `/healthz` took longer than the LB's 1 s check timeout. The LB marked the backend dead 5 times and returned 503 to *every* request until the next check. 13,562 errors ÷ 5 ejections ≈ 2,700 errors each ≈ 2 s of total outage at ~1,350 req/s — the whole 2.53% error rate. The proxied requests themselves never failed (0 passive-check errors). This is a small-scale **cascading failure**: overload → health check fails → capacity removed → more overload.

**4. Graceful shutdown helps, but does not reach zero.** Stopping a replica under load (10 concurrent clients, processes run locally before containerising) produced ~0.1% `502`s with SIGTERM and ~0.3% with `kill -9`. Graceful shutdown finishes in-flight requests, but new requests arriving before the LB's next health check (≤ 2 s) are still refused.

**5. Measurement noise is large.** Two runs of identical code served 2,423 and 1,599 req/s (~35% apart) on this laptop. Single runs cannot distinguish small improvements; repeated runs and per-request CPU cost are more reliable than raw p99.

## Design decisions and trade-offs

| Decision | Why | Trade-off |
| --- | --- | --- |
| **Atomic Lua script** (`INCR` + `PEXPIRE`) for the limiter | Two replicas can't both read 999 and both allow; a crash can't leave a counter without an expiry | One Redis round trip per request |
| **Fixed window** | Simplest correct distributed limiter | Allows up to 2× the limit across a window boundary; sliding window or token bucket avoid this |
| **Limiter fails closed** (503 if Redis is down) | Protects downstream from unlimited traffic | A Redis outage becomes a full `/items` outage, which also bypasses the cache's graceful fallback |
| **Cache-aside, 30 s TTL**; falls back to the store on Redis errors | Fast repeat reads; the cache is never a hard dependency | Data can be up to 30 s stale |
| **Client identity from `X-Client-ID`** | Behind the LB, every request has the LB's IP | Clients can spoof it; production would use an API key or authenticated identity |
| **Atomic counter for round-robin** | Every request is its own goroutine; an atomic avoids both a data race and a lock on the hot path | — |
| **Histogram, not average**; exponential buckets 0.5 ms–4 s; `route` label from the matched pattern | Percentiles expose the slow tail; route patterns keep cardinality low (never label by item id) | Quantiles are interpolated within a bucket |
| **Open-model load testing** | Fixed arrival rate exposes queueing; closed-model tests hide slowdowns (coordinated omission) | Needs enough pre-allocated VUs (Little's Law: in-flight = rate × latency) |

## Limitations and next steps

- **Health-check threshold + panic mode:** eject a backend only after N consecutive failures, and never eject *all* backends (route to everyone rather than return 503) — directly fixes Finding 3.
- **Drain before shutdown:** fail `/healthz` first, wait one check interval, then shut down — should take graceful-stop errors to zero.
- **Retry idempotent requests** on another backend when the connection was refused.
- **Profile the LB** with `pprof` to see where its CPU goes; consider multiple LB instances (it is also a single point of failure).
- **Sliding-window limiter**, and pipelining the limiter and cache calls into one Redis round trip.
- **Distributed tracing** (OpenTelemetry) across LB → API → Redis.

## Repository layout

```text
cmd/api/            API service
cmd/lb/             load balancer
internal/limiter/   distributed rate limiter + concurrency test
internal/config/    env-var helper + Prometheus histogram
loadtest/           k6 script and run.sh
results/            raw load-test output
docs/CODE_GUIDE.md  directory-, file- and function-level guide to the code
```

For how each part of the code works, see **[docs/CODE_GUIDE.md](docs/CODE_GUIDE.md)**.
