package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

type ctxKey string

const loggerKey ctxKey = "observability_logger"

var metricsNamespace string

// Span handles the state of a single unit of work
type Span struct {
	name      string
	startTime time.Time
	logger    *slog.Logger
	mu        sync.Mutex
	err       error
	attrs     []any
	done      bool
}

// Init sets up the global JSON logger with resource attributes
func Init(service string, version string, env string, level slog.Level) *slog.Logger {
	metricsNamespace = service

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	logger := slog.New(handler).With(
		slog.String("service", service),
		slog.String("version", version),
		slog.String("env", env),
	)
	slog.SetDefault(logger)
	return logger
}

// WithLogger stores a logger in the context for downstream use via LoggerFromCtx.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// LoggerFromCtx retrieves the logger from context or returns a safe fallback
func LoggerFromCtx(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// StartSpan begins a new timed operation
func StartSpan(ctx context.Context, name string) (context.Context, *Span) {
	parent := LoggerFromCtx(ctx)
	spanID := genID(8)

	s := &Span{
		name:      name,
		startTime: time.Now(),
		logger:    parent.With(slog.String("span_name", name), slog.String("span_id", spanID)),
	}

	return WithLogger(ctx, s.logger), s
}

func (s *Span) SetError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *Span) AddAttribute(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs = append(s.attrs, slog.Any(key, value))
}

func (s *Span) AddEvent(msg string, attrs ...any) {
	s.logger.Info(msg, attrs...)
}

func (s *Span) End() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.done {
		return
	}
	s.done = true

	duration := time.Since(s.startTime).Milliseconds()
	logArgs := make([]any, len(s.attrs), len(s.attrs)+4)
	copy(logArgs, s.attrs)
	logArgs = append(logArgs, slog.Int64("duration_ms", duration))

	if s.err != nil {
		logArgs = append(logArgs,
			slog.String("error", s.err.Error()),
			Metrics(metricsNamespace, []string{"span_name"},
				MetricDef{Name: "SpanDuration", Unit: "Milliseconds"},
				MetricDef{Name: "SpanError", Unit: "Count"},
			),
		)
		s.logger.Error("span_finished", logArgs...)
	} else {
		logArgs = append(logArgs,
			Metrics(metricsNamespace, []string{"span_name"},
				MetricDef{Name: "SpanDuration", Unit: "Milliseconds"},
			),
		)
		s.logger.Info("span_finished", logArgs...)
	}
}

// Middleware for Chi integration
func Middleware(l *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			traceID := middleware.GetReqID(r.Context())
			if traceID == "" {
				traceID = genID(16)
			}

			reqLogger := l.With(slog.String("trace_id", traceID))
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			ctx := WithLogger(r.Context(), reqLogger)
			next.ServeHTTP(ww, r.WithContext(ctx))

			logArgs := []any{
				slog.Int("status", ww.Status()),
				slog.Int64("latency_ms", time.Since(start).Milliseconds()),
				slog.Int("response_bytes", ww.BytesWritten()),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("remote_ip", r.RemoteAddr),
				Metrics(metricsNamespace, []string{"method"},
					MetricDef{Name: "Latency", Unit: "Milliseconds"},
				),
			}

			if ww.Status() >= 500 {
				reqLogger.Error("http_request", logArgs...)
			} else {
				reqLogger.Info("http_request", logArgs...)
			}
		})
	}
}

// Recoverer catches panics, logs them with the request's trace context, and returns 500.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				logger := LoggerFromCtx(r.Context())
				logger.Error("panic_recovered",
					slog.String("panic", fmt.Sprintf("%v", rv)),
					slog.String("stack", string(debug.Stack())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					Metrics(metricsNamespace, []string{"method"},
						MetricDef{Name: "Panic", Unit: "Count"},
					),
				)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// MetricDef defines a single CloudWatch EMF metric entry.
type MetricDef struct {
	Name string
	Unit string
}

// Metrics builds a single _aws EMF group containing one or more metric definitions.
// All metrics share the same namespace, timestamp, and dimension set.
func Metrics(namespace string, dims []string, defs ...MetricDef) slog.Attr {
	metrics := make([]map[string]string, len(defs))
	for i, d := range defs {
		metrics[i] = map[string]string{"Name": d.Name, "Unit": d.Unit}
	}
	return slog.Group("_aws",
		slog.Int64("Timestamp", time.Now().UnixMilli()),
		slog.Any("CloudWatchMetrics", []map[string]any{{
			"Namespace":  namespace,
			"Dimensions": [][]string{dims},
			"Metrics":    metrics,
		}}),
	)
}

// Helpers
func genID(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	return hex.EncodeToString(b)
}
