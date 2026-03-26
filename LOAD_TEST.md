# Load Test Report

**Date:** 2026-03-26
**Tool:** Apache Bench (`ab`)
**Environment:** Local (Darwin), Go 1.24.1

## Unit Tests

28/28 tests pass with `-race` flag enabled — no data races detected.

```
go test -race -count=1 ./pkg/
PASS ok observability/pkg 1.384s
```

## Load Test Results

| Endpoint | Reqs | Concurrency | RPS | Avg Latency | Failed | Notes |
|---|---|---|---|---|---|---|
| `GET /health/live` | 1,000 | 50 | **10,075** | 0.1ms | 0 | |
| `GET /health/ready` | 1,000 | 50 | **10,122** | 0.1ms | 0 | |
| `GET /process` | 1,000 | 50 | **409** | 2.4ms | 0 | 401 non-2xx expected (simulated 50% errors) |
| `POST /orders` | 1,000 | 50 | **469** | 2.1ms | 0 | Includes 80ms `time.Sleep` |
| `GET /error` | 1,000 | 50 | **9,760** | 0.1ms | 0 | All 500s as designed |
| `GET /panic` | 500 | 50 | **7,937** | 0.1ms | 0 | Recoverer catches all panics, returns 500 |

### High-Concurrency Burst (200 concurrent)

| Endpoint | Reqs | Concurrency | RPS | Avg Latency | Failed |
|---|---|---|---|---|---|
| `GET /health/live` | 5,000 | 200 | **7,152** | 0.1ms | 0 |
| `POST /orders` | 5,000 | 200 | **1,051** | 0.9ms | 0 |

## Key Findings

- **Zero failed requests** across all endpoints under load.
- **Panic recovery works reliably** at high concurrency — no leaked panics.
- **Middleware, spans, and EMF metrics** all function correctly under concurrent pressure.
- Lower RPS on `/process` and `/orders` is expected due to intentional `time.Sleep` calls simulating work.
