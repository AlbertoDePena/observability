# Observability Module - Technical Documentation

## Overview

The `observability` module is a lightweight, structured logging and tracing library for Go HTTP services. It produces JSON-formatted logs to stdout using the standard library's `log/slog` package and embeds [AWS CloudWatch Embedded Metric Format (EMF)](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch_Embedded_Metric_Format.html) metadata directly into log lines, enabling CloudWatch to automatically extract custom metrics without a separate metrics SDK.

The module integrates with [chi/v5](https://github.com/go-chi/chi) for HTTP middleware but the core logging and span APIs are framework-agnostic.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                      main.go                            │
│  Init() ──► chi Router ──► Middleware ──► Handlers      │
│                                  │                      │
│                          ┌───────┴────────┐             │
│                          │  context.Context│             │
│                          │  (carries logger│             │
│                          │   + trace_id)   │             │
│                          └───────┬────────┘             │
│                                  │                      │
│                          StartSpan / LoggerFromCtx      │
└─────────────────────────────────────────────────────────┘
                           │
                           ▼ stdout (JSON)
                ┌─────────────────────────┐
                │  CloudWatch Logs Agent   │
                │  (parses EMF metadata)   │
                └────────────┬────────────┘
                             ▼
                   CloudWatch Metrics
```

## Module Structure

```
observability/
├── go.mod              # Module definition (observability), depends on chi/v5
├── go.sum
├── main.go             # Example HTTP server demonstrating usage
└── pkg/
    └── observability.go  # Core library: logger, spans, middleware, metrics
```

## Core Components

### 1. Logger Initialization (`Init`)

```go
func Init(service string, version string, env string) *slog.Logger
```

Creates a JSON-structured logger writing to stdout. Every log line includes three resource attributes (`service`, `version`, `env`) for filtering and correlation in log aggregation systems. The logger is also set as the process-wide default via `slog.SetDefault()`.

**Parameters:**
| Name | Description |
|------|-------------|
| `service` | Service identifier (e.g., `"my-api"`) |
| `version` | Application version (e.g., `"1.0.0"`) |
| `env` | Deployment environment (e.g., `"production"`, `"development"`) |

### 2. Context-Propagated Logging (`LoggerFromCtx`)

```go
func LoggerFromCtx(ctx context.Context) *slog.Logger
```

Retrieves the current logger from `context.Context`. The logger stored in context is progressively enriched as it passes through middleware (gains `trace_id`) and spans (gains `span_name`, `span_id`). If no logger is found in context, it falls back to `slog.Default()`.

This design ensures that any function with access to the request context can emit structured logs that are automatically correlated to the originating HTTP request.

### 3. Spans (`StartSpan` / `Span`)

```go
func StartSpan(ctx context.Context, name string) (context.Context, *Span)
```

Spans represent timed units of work. They provide lightweight tracing without requiring a full distributed tracing backend.

**Span lifecycle:**

1. `StartSpan(ctx, "operation_name")` - creates a span, generates a random `span_id`, records the start time, and returns a new context with the span's enriched logger.
2. During the span's lifetime, callers can:
   - `span.AddAttribute(key, value)` - attach business-relevant metadata (e.g., `user_id`).
   - `span.AddEvent(msg)` - emit an intermediate log entry under the span's logger.
   - `span.SetError(err)` - mark the span as failed.
3. `span.End()` - emits a `span_finished` log entry containing:
   - All accumulated attributes
   - `duration_ms` - wall-clock elapsed time in milliseconds
   - A `SpanDuration` CloudWatch EMF metric dimensioned by `span_name`
   - If an error was set: the error message and a `SpanError` CloudWatch EMF metric

**Concurrency safety:** `Span` fields are protected by a `sync.Mutex`, making it safe to call `SetError` and `AddAttribute` from multiple goroutines.

### 4. HTTP Middleware

```go
func Middleware(l *slog.Logger) func(http.Handler) http.Handler
```

A chi-compatible middleware that wraps each HTTP request with observability:

1. **Trace ID propagation** - reads the request ID set by chi's `middleware.RequestID`. If none exists (e.g., for non-chi contexts), generates a random 32-character hex trace ID.
2. **Logger injection** - creates a request-scoped logger enriched with the `trace_id` and stores it in context. All downstream `LoggerFromCtx` calls will inherit this trace ID.
3. **Response capture** - wraps the `http.ResponseWriter` using chi's `WrapResponseWriter` to capture the status code.
4. **Request log** - after the handler completes, emits an `http_request` log entry with:
   - `status` - HTTP response status code
   - `latency_ms` - request duration in milliseconds
   - `method` - HTTP method
   - A `Latency` CloudWatch EMF metric dimensioned by `method`

### 5. CloudWatch Embedded Metric Format (`Metric`)

```go
func Metric(namespace, name, unit string, dims []string) slog.Attr
```

Generates an `slog.Attr` containing an `_aws` group that conforms to the [CloudWatch EMF specification](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch_Embedded_Metric_Format_Specification.html). When these log lines are ingested by CloudWatch Logs, the agent automatically extracts and publishes the embedded metrics.

**Metrics emitted by the module:**

| Metric Name | Namespace | Unit | Dimensions | Emitted By |
|---|---|---|---|---|
| `Latency` | `MyAPI` | Milliseconds | `method` | HTTP Middleware |
| `SpanDuration` | `MyAPI` | Milliseconds | `span_name` | `Span.End()` |
| `SpanError` | `MyAPI` | Count | `span_name` | `Span.End()` (on error) |

### 6. ID Generation (`genID`)

```go
func genID(length int) string
```

Generates a cryptographically random hex string of `length * 2` characters (since each byte becomes two hex characters). Used for `trace_id` (16 bytes = 32 hex chars) and `span_id` (8 bytes = 16 hex chars).

## Data Flow

### HTTP Request

```
Incoming Request
    │
    ▼
chi.middleware.RequestID  ──► sets X-Request-Id header + context value
    │
    ▼
observability.Middleware  ──► reads request ID as trace_id
    │                         creates logger with trace_id
    │                         injects logger into context
    │                         wraps ResponseWriter
    │
    ▼
Handler (e.g., /process)
    │
    ├── StartSpan("payment_flow")
    │       │
    │       ├── AddAttribute("user_id", "user_123")
    │       ├── AddEvent("contacting_gateway")
    │       ├── SetError(err)  // if failure
    │       └── End()  ──► logs "span_finished" with duration + EMF metrics
    │
    ▼
Middleware post-handler    ──► logs "http_request" with status, latency, EMF metrics
```

### Background Task

```
context.Background()
    │
    ▼
StartSpan("daily_cleanup") ──► creates logger with span_name + span_id
    │                           (no trace_id — not an HTTP context)
    │
    ├── LoggerFromCtx(ctx).Info(...)  ──► logs with span context
    │
    └── End()  ──► logs "span_finished" with duration + EMF metrics
```

## Example Log Output

A single HTTP request to `/process` produces log lines similar to:

```json
{"time":"...","level":"INFO","msg":"contacting_gateway","service":"my-api","version":"1.0.0","env":"development","trace_id":"a1b2c3...","span_name":"payment_flow","span_id":"d4e5f6..."}

{"time":"...","level":"INFO","msg":"span_finished","service":"my-api","version":"1.0.0","env":"development","trace_id":"a1b2c3...","span_name":"payment_flow","span_id":"d4e5f6...","user_id":"user_123","duration_ms":102,"_aws":{"Timestamp":1711468800000,"CloudWatchMetrics":[{"Namespace":"MyAPI","Dimensions":[["span_name"]],"Metrics":[{"Name":"SpanDuration","Unit":"Milliseconds"}]}]}}

{"time":"...","level":"INFO","msg":"http_request","service":"my-api","version":"1.0.0","env":"development","trace_id":"a1b2c3...","status":200,"latency_ms":103,"method":"GET","_aws":{"Timestamp":1711468800000,"CloudWatchMetrics":[{"Namespace":"MyAPI","Dimensions":[["method"]],"Metrics":[{"Name":"Latency","Unit":"Milliseconds"}]}]}}
```

## Dependencies

| Dependency | Version | Purpose |
|---|---|---|
| `github.com/go-chi/chi/v5` | v5.2.1 | HTTP router, request ID middleware, response writer wrapper |
| `log/slog` (stdlib) | Go 1.21+ | Structured JSON logging |
| `crypto/rand` (stdlib) | — | Cryptographically secure ID generation |
