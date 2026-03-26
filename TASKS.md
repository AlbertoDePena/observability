# Completed Tasks

## Bug Fixes

### Fix `Span.End()` append mutation bug
`append(s.attrs, ...)` could write through to the backing array of `s.attrs` when it had spare capacity, silently corrupting span attributes. Fixed by copying `s.attrs` into a fresh slice before appending.
- File: `pkg/observability.go` — `Span.End()`

### Guard `Span.End()` against double calls
Nothing prevented `End()` from being called twice, which would emit a duplicate `span_finished` log with an incorrect duration. Added a `done` bool field to `Span`, checked and set under the existing mutex.
- File: `pkg/observability.go` — `Span` struct, `Span.End()`

### Fix CloudWatch EMF duplicate `_aws` groups
When a span had an error, `End()` appended two separate `Metric()` calls, each producing its own `_aws` JSON group. Since JSON objects can only have one key of a given name, CloudWatch would silently drop `SpanDuration` and only see `SpanError`. Replaced `Metric()` with `Metrics()` which accepts multiple `MetricDef` entries and emits a single `_aws` group containing all metrics.
- File: `pkg/observability.go` — `Metrics()`, `MetricDef`, `Span.End()`

### Fix EMF Dimensions duplication causing double-counted metrics
`Metrics()` was creating one dimension set per `MetricDef`, so error spans with 2 metrics produced `"Dimensions": [["span_name"], ["span_name"]]`. Per the EMF spec, CloudWatch publishes every metric for each dimension set, meaning every metric was emitted twice — doubling data points and skewing aggregations. Fixed by making dimensions a single shared parameter: `Metrics(namespace, dims, ...defs)`. Now produces `"Dimensions": [["span_name"]]` regardless of how many metrics are in the group.
- File: `pkg/observability.go` — `Metrics()`, `MetricDef`

## Prod-Readiness

### Add request path to HTTP middleware log
The `http_request` log entry included `method` and `status` but was missing the URL path, making it impossible to filter or debug by endpoint. Added `slog.String("path", r.URL.Path)`.
- File: `pkg/observability.go` — `Middleware()`

### Add response bytes to HTTP middleware log
Response size was not tracked despite `WrapResponseWriter` already capturing it. Added `slog.Int("response_bytes", ww.BytesWritten())` for cost analysis and anomaly detection.
- File: `pkg/observability.go` — `Middleware()`

### Add remote IP to HTTP middleware log
`RealIP` middleware resolves the client IP from proxy headers, but the resolved IP was never logged. Added `slog.String("remote_ip", r.RemoteAddr)` for rate-limit debugging and abuse investigation.
- File: `pkg/observability.go` — `Middleware()`

### Log 5xx responses at ERROR level
All HTTP responses were logged at INFO regardless of status code. A 500 now logs at ERROR, making it straightforward to set up CloudWatch alarms on error-level log entries.
- File: `pkg/observability.go` — `Middleware()`

### Handle `ListenAndServe` error and add graceful shutdown
`http.ListenAndServe` error was silently discarded. Replaced with `http.Server` + `signal.NotifyContext` listening for SIGINT/SIGTERM. On signal, `srv.Shutdown()` drains in-flight requests with a 10-second timeout.
- File: `main.go`

### Add HTTP server timeouts
`http.Server` had no `ReadTimeout`, `WriteTimeout`, or `IdleTimeout`, allowing slow clients to hold connections open indefinitely. Set `ReadTimeout: 5s`, `WriteTimeout: 10s`, `IdleTimeout: 120s`.
- File: `main.go`

### Make log level configurable in `Init()`
The `slog.HandlerOptions` was `nil`, hardcoding INFO level. Added a `slog.Level` parameter so callers can tune verbosity per environment.
- File: `pkg/observability.go` — `Init()`

### Extract hardcoded CloudWatch namespace
The string `"MyAPI"` was hardcoded in 3 separate `Metric()` call sites. Now derived from the `service` parameter passed to `Init()` and stored as package-level state, preventing silent divergence.
- File: `pkg/observability.go` — `Init()`, `Span.End()`, `Middleware()`

## Middleware

### Add custom Recoverer middleware
Chi's built-in `Recoverer` uses its own logger and output format. Built a custom version that uses the observability module, so panic recovery flows through the same structured JSON logs with full trace context, stack trace, and a `Panic` CloudWatch EMF metric.
- File: `pkg/observability.go` — `Recoverer()`

### Add RealIP middleware
Added `middleware.RealIP` to the middleware stack to extract the real client IP from `X-Forwarded-For` / `X-Real-IP` headers when behind a load balancer.
- File: `main.go`

## Cleanup

### Add context-based cancellation to background worker
The background goroutine used `context.Background()` and `time.Sleep` with no shutdown path. Now uses the signal-cancellable context with `select` on `ctx.Done()`, so the worker exits cleanly on shutdown.
- File: `main.go`

## Infrastructure

### Fix invalid imports and module structure
- Moved `observability.go` from root into `pkg/` subdirectory to resolve two-packages-in-one-directory error
- Fixed `go.mod` from `chi v1.5.5` to `chi/v5 v5.2.1`
- Fixed chi middleware import from v1 path to v5 path
- Removed invalid `"://github.com/middleware"` and placeholder `"yourproject/observability"` imports

### Add test endpoints and HTTP file
Added `/health`, `/orders`, `/slow`, `/error`, and `/panic` endpoints for exercising all observability features. Created `requests.http` for use with VS Code REST Client or IntelliJ.
- File: `main.go`, `requests.http`

### Add technical documentation
Created `TECH_DOC.md` detailing module architecture, all public APIs, data flow diagrams, EMF metric definitions, and example log output.
- File: `TECH_DOC.md`

## Production Readiness

### Replace global WriteTimeout with per-route timeouts
`WriteTimeout: 10s` on `http.Server` would kill long-running HTTP responses. Removed the global `WriteTimeout` constraint by using chi's `middleware.Timeout` on short-lived API route groups only. The server-level `WriteTimeout` is set to 30s as a safety net.
- File: `main.go`

### Add `ReadHeaderTimeout` to server config
Added `ReadHeaderTimeout: 2s` to guard against slowloris attacks where clients send headers very slowly to exhaust server connections.
- File: `main.go`

### Add request body size limit
Added `middleware.RequestSize(1 << 20)` (1MB) to the middleware stack to prevent oversized request bodies from consuming memory.
- File: `main.go`

### Split health check into liveness and readiness probes
Replaced single `/health` endpoint with `/health/live` (always 200, proves the process is running) and `/health/ready` (checks downstream dependencies — stubbed with a TODO for real dependency checks). ALB/ECS should route traffic only when readiness passes.
- File: `main.go`

### Add `WithLogger` helper to observability package
Added `WithLogger(ctx, logger)` to inject a logger into context for background goroutines that don't originate from an HTTP request. This avoids exporting the raw context key.
- File: `pkg/observability.go` — `WithLogger()`

## Testing

### Add unit tests for observability package
Created 31 tests covering all public APIs and critical internal behavior in `pkg/observability_test.go`.
- File: `pkg/observability_test.go`

**Init (3 tests):**
- Resource attributes (service, version, env) are set on the logger
- `metricsNamespace` is set from the service parameter
- Log level filtering is respected

**WithLogger (1 test):**
- Injects logger into context, retrievable via `LoggerFromCtx`

**LoggerFromCtx (2 tests):**
- Returns the logger stored in context
- Falls back to `slog.Default()` when context has no logger

**StartSpan (2 tests):**
- Enriches context with `span_name` and `span_id`
- Generates unique span IDs across calls

**Span.SetError (2 tests):**
- `nil` error is ignored (no state change)
- Non-nil error is stored

**Span.AddAttribute (1 test):**
- Attributes accumulate correctly

**Span.AddEvent (1 test):**
- Events are logged with span context (span_name, span_id)

**Span.End (7 tests):**
- Logs `span_finished` with `duration_ms` at INFO level
- Error spans log at ERROR level with error message
- Double `End()` call is a no-op (only one `span_finished` emitted)
- `End()` does not mutate `s.attrs` (append copy safety)
- Concurrent `AddAttribute` + `SetError` + `End` does not race or deadlock
- Error spans emit a single `_aws` EMF group (not duplicates)
- Error spans contain both `SpanDuration` and `SpanError` metrics

**Metrics (3 tests):**
- Single metric definition produces valid `_aws` group
- Multiple metric definitions are combined correctly
- Namespace is set in the CloudWatchMetrics entry

**Middleware (5 tests):**
- Logs `http_request` with status, latency, response_bytes, method, path, remote_ip
- 5xx responses log at ERROR level
- 2xx responses log at INFO level
- Injects logger with `trace_id` into request context
- Integrates with chi's `RequestID` middleware

**Recoverer (2 tests):**
- Catches panic, returns 500, logs `panic_recovered` with panic value, stack trace, method, path
- Passes through normally when no panic occurs

**genID (3 tests):**
- Returns correct hex string length (bytes * 2)
- Produces unique values across 100 calls
- Output is valid lowercase hex
