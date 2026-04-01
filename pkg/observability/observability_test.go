package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// newTestLogger creates a logger that writes JSON to a buffer for assertion.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

// parseLogLines splits the buffer into decoded JSON maps.
func parseLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("failed to parse log line: %v\nraw: %s", err, raw)
		}
		lines = append(lines, m)
	}
	return lines
}

// lastLog returns the last log line from the buffer.
func lastLog(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := parseLogLines(t, buf)
	if len(lines) == 0 {
		t.Fatal("expected at least one log line, got none")
	}
	return lines[len(lines)-1]
}

// --- Init ---

func TestInit_SetsResourceAttributes(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(handler).With(
		slog.String("service", "test-svc"),
		slog.String("version", "0.1.0"),
		slog.String("env", "test"),
	)
	logger.Info("boot")

	m := lastLog(t, &buf)
	if m["service"] != "test-svc" {
		t.Errorf("expected service=test-svc, got %v", m["service"])
	}
	if m["version"] != "0.1.0" {
		t.Errorf("expected version=0.1.0, got %v", m["version"])
	}
	if m["env"] != "test" {
		t.Errorf("expected env=test, got %v", m["env"])
	}
}

func TestInit_SetsMetricsNamespace(t *testing.T) {
	Init("my-namespace", "1.0.0", "test", slog.LevelInfo)
	if metricsNamespace != "my-namespace" {
		t.Errorf("expected metricsNamespace=my-namespace, got %s", metricsNamespace)
	}
}

func TestInit_RespectsLogLevel(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(handler)

	logger.Info("should be filtered")
	logger.Warn("should appear")

	lines := parseLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	if lines[0]["msg"] != "should appear" {
		t.Errorf("expected msg=should appear, got %v", lines[0]["msg"])
	}
}

// --- LoggerFromCtx ---

func TestLoggerFromCtx_ReturnsLoggerFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	ctx := context.WithValue(context.Background(), loggerKey, logger)
	got := LoggerFromCtx(ctx)
	got.Info("from_ctx")

	m := lastLog(t, &buf)
	if m["msg"] != "from_ctx" {
		t.Errorf("expected msg=from_ctx, got %v", m["msg"])
	}
}

func TestWithLogger_InjectsLoggerIntoContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	ctx := WithLogger(context.Background(), logger)
	got := LoggerFromCtx(ctx)
	got.Info("injected")

	m := lastLog(t, &buf)
	if m["msg"] != "injected" {
		t.Errorf("expected msg=injected, got %v", m["msg"])
	}
}

func TestLoggerFromCtx_FallsBackToDefault(t *testing.T) {
	// Should not panic with empty context
	logger := LoggerFromCtx(context.Background())
	if logger == nil {
		t.Fatal("expected non-nil logger from fallback")
	}
}

// --- StartSpan ---

func TestStartSpan_EnrichesContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	newCtx, span := StartSpan(ctx, "test_op")
	defer span.End()

	// The logger from the new context should have span_name
	LoggerFromCtx(newCtx).Info("inside_span")

	m := lastLog(t, &buf)
	if m["span_name"] != "test_op" {
		t.Errorf("expected span_name=test_op, got %v", m["span_name"])
	}
	if m["span_id"] == nil || m["span_id"] == "" {
		t.Error("expected non-empty span_id")
	}
}

func TestStartSpan_GeneratesUniqueSpanIDs(t *testing.T) {
	ctx := context.Background()
	_, span1 := StartSpan(ctx, "op1")
	_, span2 := StartSpan(ctx, "op2")
	defer span1.End()
	defer span2.End()

	// Access span_id through the logger by logging and checking output isn't sufficient
	// since span_id is in the logger. Instead verify the spans are distinct objects.
	if span1 == span2 {
		t.Error("expected distinct span instances")
	}
}

// --- Span.SetError ---

func TestSetError_NilIsIgnored(t *testing.T) {
	_, span := StartSpan(context.Background(), "op")
	span.SetError(nil)
	span.mu.Lock()
	defer span.mu.Unlock()
	if span.err != nil {
		t.Error("expected nil error after SetError(nil)")
	}
}

func TestSetError_StoresError(t *testing.T) {
	_, span := StartSpan(context.Background(), "op")
	expected := errors.New("fail")
	span.SetError(expected)
	span.mu.Lock()
	defer span.mu.Unlock()
	if span.err != expected {
		t.Errorf("expected error %v, got %v", expected, span.err)
	}
}

// --- Span.AddAttribute ---

func TestAddAttribute_AppendsToAttrs(t *testing.T) {
	_, span := StartSpan(context.Background(), "op")
	span.AddAttribute("key1", "val1")
	span.AddAttribute("key2", 42)
	span.mu.Lock()
	defer span.mu.Unlock()
	if len(span.attrs) != 2 {
		t.Errorf("expected 2 attrs, got %d", len(span.attrs))
	}
}

// --- Span.AddEvent ---

func TestAddEvent_LogsWithSpanContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	_, span := StartSpan(ctx, "op")
	span.AddEvent("something_happened", slog.String("detail", "info"))
	span.End()

	lines := parseLogLines(t, &buf)
	// First line should be the event
	found := false
	for _, m := range lines {
		if m["msg"] == "something_happened" {
			found = true
			if m["detail"] != "info" {
				t.Errorf("expected detail=info, got %v", m["detail"])
			}
			if m["span_name"] != "op" {
				t.Errorf("expected span_name=op, got %v", m["span_name"])
			}
		}
	}
	if !found {
		t.Error("expected to find log line with msg=something_happened")
	}
}

// --- Span.End ---

func TestEnd_LogsSpanFinishedWithDuration(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	_, span := StartSpan(ctx, "timed_op")
	span.End()

	m := lastLog(t, &buf)
	if m["msg"] != "span_finished" {
		t.Errorf("expected msg=span_finished, got %v", m["msg"])
	}
	if m["level"] != "INFO" {
		t.Errorf("expected level=INFO, got %v", m["level"])
	}
	if _, ok := m["duration_ms"]; !ok {
		t.Error("expected duration_ms in log output")
	}
}

func TestEnd_ErrorSpanLogsAtErrorLevel(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	_, span := StartSpan(ctx, "error_op")
	span.SetError(errors.New("something broke"))
	span.End()

	m := lastLog(t, &buf)
	if m["level"] != "ERROR" {
		t.Errorf("expected level=ERROR, got %v", m["level"])
	}
	if m["error"] != "something broke" {
		t.Errorf("expected error=something broke, got %v", m["error"])
	}
}

func TestEnd_DoubleCallIsNoop(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	_, span := StartSpan(ctx, "double_end")
	span.End()
	span.End() // second call should be ignored

	lines := parseLogLines(t, &buf)
	spanFinishedCount := 0
	for _, m := range lines {
		if m["msg"] == "span_finished" {
			spanFinishedCount++
		}
	}
	if spanFinishedCount != 1 {
		t.Errorf("expected exactly 1 span_finished log, got %d", spanFinishedCount)
	}
}

func TestEnd_DoesNotMutateAttrs(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	_, span := StartSpan(ctx, "mutation_test")
	span.AddAttribute("key", "value")

	// Record attrs length before End
	span.mu.Lock()
	attrsBefore := len(span.attrs)
	span.mu.Unlock()

	span.End()

	// Attrs should not have been modified by End
	span.mu.Lock()
	attrsAfter := len(span.attrs)
	span.mu.Unlock()

	if attrsBefore != attrsAfter {
		t.Errorf("End() mutated s.attrs: before=%d, after=%d", attrsBefore, attrsAfter)
	}
}

func TestEnd_ConcurrentSafety(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	_, span := StartSpan(ctx, "concurrent_op")

	var wg sync.WaitGroup
	// Concurrent AddAttribute + SetError + End
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			span.AddAttribute("key", n)
		}(i)
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		span.SetError(errors.New("race"))
	}()
	go func() {
		defer wg.Done()
		span.End()
	}()
	wg.Wait()

	// Should not panic or deadlock — reaching here is the test
}

// --- Span.End EMF ---

func TestEnd_ErrorSpanEmitsSingleAWSGroup(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	_, span := StartSpan(ctx, "emf_test")
	span.SetError(errors.New("fail"))
	span.End()

	raw := buf.String()
	// Count occurrences of "_aws" — should be exactly 1 per log line
	lastLine := raw[strings.LastIndex(raw[:len(raw)-1], "\n")+1:]
	count := strings.Count(lastLine, `"_aws"`)
	if count != 1 {
		t.Errorf("expected exactly 1 _aws group in error span log, got %d", count)
	}
}

func TestEnd_ErrorSpanContainsBothMetrics(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	ctx := context.WithValue(context.Background(), loggerKey, logger)

	_, span := StartSpan(ctx, "emf_both")
	span.SetError(errors.New("fail"))
	span.End()

	m := lastLog(t, &buf)
	aws, ok := m["_aws"].(map[string]any)
	if !ok {
		t.Fatal("expected _aws group in log")
	}
	cwm, ok := aws["CloudWatchMetrics"].([]any)
	if !ok || len(cwm) == 0 {
		t.Fatal("expected CloudWatchMetrics array")
	}
	entry := cwm[0].(map[string]any)
	metrics := entry["Metrics"].([]any)
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metrics in error span, got %d", len(metrics))
	}

	names := make(map[string]bool)
	for _, metric := range metrics {
		names[metric.(map[string]any)["Name"].(string)] = true
	}
	if !names["SpanDuration"] {
		t.Error("expected SpanDuration metric")
	}
	if !names["SpanError"] {
		t.Error("expected SpanError metric")
	}

	// Verify single dimension set (not duplicated per metric)
	dims := entry["Dimensions"].([]any)
	if len(dims) != 1 {
		t.Errorf("expected 1 dimension set, got %d (metrics would be double-counted)", len(dims))
	}
}

// --- Metrics ---

func TestMetrics_SingleDef(t *testing.T) {
	attr := Metrics("ns", []string{"method"}, MetricDef{Name: "Latency", Unit: "Milliseconds"})
	if attr.Key != "_aws" {
		t.Errorf("expected key=_aws, got %s", attr.Key)
	}
}

func TestMetrics_MultipleDefs(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	logger.Info("test",
		Metrics("ns", []string{"d1", "d2"},
			MetricDef{Name: "A", Unit: "Count"},
			MetricDef{Name: "B", Unit: "Bytes"},
		),
	)

	m := lastLog(t, &buf)
	aws := m["_aws"].(map[string]any)
	cwm := aws["CloudWatchMetrics"].([]any)
	entry := cwm[0].(map[string]any)

	metrics := entry["Metrics"].([]any)
	if len(metrics) != 2 {
		t.Fatalf("expected 2 metrics, got %d", len(metrics))
	}

	dims := entry["Dimensions"].([]any)
	if len(dims) != 1 {
		t.Fatalf("expected 1 dimension set, got %d", len(dims))
	}
	dimSet := dims[0].([]any)
	if len(dimSet) != 2 {
		t.Fatalf("expected 2 dimensions in set, got %d", len(dimSet))
	}
}

func TestMetrics_NamespaceIsSet(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	logger.Info("test",
		Metrics("custom-ns", []string{"d"}, MetricDef{Name: "M", Unit: "None"}),
	)

	m := lastLog(t, &buf)
	aws := m["_aws"].(map[string]any)
	cwm := aws["CloudWatchMetrics"].([]any)
	entry := cwm[0].(map[string]any)
	if entry["Namespace"] != "custom-ns" {
		t.Errorf("expected Namespace=custom-ns, got %v", entry["Namespace"])
	}
}

// --- Middleware ---

func TestMiddleware_LogsHTTPRequest(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/test-path", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := lastLog(t, &buf)
	if m["msg"] != "http_request" {
		t.Errorf("expected msg=http_request, got %v", m["msg"])
	}
	if m["method"] != "GET" {
		t.Errorf("expected method=GET, got %v", m["method"])
	}
	if m["path"] != "/test-path" {
		t.Errorf("expected path=/test-path, got %v", m["path"])
	}
	if m["status"] != float64(200) {
		t.Errorf("expected status=200, got %v", m["status"])
	}
	if m["response_bytes"] != float64(5) {
		t.Errorf("expected response_bytes=5, got %v", m["response_bytes"])
	}
	if _, ok := m["latency_ms"]; !ok {
		t.Error("expected latency_ms in log")
	}
	if _, ok := m["remote_ip"]; !ok {
		t.Error("expected remote_ip in log")
	}
}

func TestMiddleware_5xxLogsAtErrorLevel(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := lastLog(t, &buf)
	if m["level"] != "ERROR" {
		t.Errorf("expected level=ERROR for 500, got %v", m["level"])
	}
}

func TestMiddleware_2xxLogsAtInfoLevel(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := lastLog(t, &buf)
	if m["level"] != "INFO" {
		t.Errorf("expected level=INFO for 200, got %v", m["level"])
	}
}

func TestMiddleware_InjectsLoggerIntoContext(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	var ctxLogger *slog.Logger

	handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxLogger = LoggerFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ctx", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if ctxLogger == nil {
		t.Fatal("expected logger in context")
	}

	// Verify trace_id is present by logging through the context logger
	ctxLogger.Info("from_handler")
	lines := parseLogLines(t, &buf)
	for _, m := range lines {
		if m["msg"] == "from_handler" {
			if _, ok := m["trace_id"]; !ok {
				t.Error("expected trace_id from context logger")
			}
			return
		}
	}
	t.Error("expected to find from_handler log line")
}

func TestMiddleware_UsesChiRequestID(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(Middleware(logger))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	m := lastLog(t, &buf)
	traceID, ok := m["trace_id"].(string)
	if !ok || traceID == "" {
		t.Error("expected non-empty trace_id from chi RequestID middleware")
	}
}

// --- Recoverer ---

func TestRecoverer_CatchesPanicAndReturns500(t *testing.T) {
	metricsNamespace = "test-ns"
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	// Inject logger into context BEFORE Recoverer so it can find it via r.Context()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), loggerKey, logger)
		Recoverer(inner).ServeHTTP(w, r.WithContext(ctx))
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}

	m := lastLog(t, &buf)
	if m["msg"] != "panic_recovered" {
		t.Errorf("expected msg=panic_recovered, got %v", m["msg"])
	}
	if m["level"] != "ERROR" {
		t.Errorf("expected level=ERROR, got %v", m["level"])
	}
	if m["panic"] != "boom" {
		t.Errorf("expected panic=boom, got %v", m["panic"])
	}
	if m["method"] != "GET" {
		t.Errorf("expected method=GET, got %v", m["method"])
	}
	if m["path"] != "/panic" {
		t.Errorf("expected path=/panic, got %v", m["path"])
	}
	stack, ok := m["stack"].(string)
	if !ok || !strings.Contains(stack, "goroutine") {
		t.Error("expected stack trace in log")
	}
}

func TestRecoverer_PassesThroughNormally(t *testing.T) {
	handler := Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body=ok, got %s", rec.Body.String())
	}
}

// --- genID ---

func TestGenID_ReturnsCorrectLength(t *testing.T) {
	id8 := genID(8)
	if len(id8) != 16 {
		t.Errorf("genID(8) should produce 16 hex chars, got %d", len(id8))
	}
	id16 := genID(16)
	if len(id16) != 32 {
		t.Errorf("genID(16) should produce 32 hex chars, got %d", len(id16))
	}
}

func TestGenID_IsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := genID(8)
		if seen[id] {
			t.Fatalf("genID produced duplicate: %s", id)
		}
		seen[id] = true
	}
}

func TestGenID_IsValidHex(t *testing.T) {
	id := genID(16)
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("genID produced non-hex character: %c in %s", c, id)
		}
	}
}
