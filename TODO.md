# TODO

## Bugs

- [x] Fix `Span.End()` append mutation bug — copy `s.attrs` before appending to prevent backing array corruption.
- [x] Guard `Span.End()` against double calls — added `done` bool field, checked under mutex.

## Prod-Readiness

- [x] Add request path to HTTP middleware log — `r.URL.Path` included in `http_request` log entry.
- [x] Handle `ListenAndServe` error and add graceful shutdown — `http.Server` + `signal.NotifyContext` for SIGINT/SIGTERM with 10s drain timeout.
- [x] Make log level configurable in `Init()` — accepts `slog.Level` parameter.
- [x] Extract hardcoded CloudWatch namespace — derived from `service` parameter passed to `Init()`.

## Cleanup

- [x] Add context-based cancellation to background worker — uses shutdown context with `select` on `ctx.Done()`.
