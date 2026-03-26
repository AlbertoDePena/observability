package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	observability "observability/pkg"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	logger := observability.Init("my-api", "1.0.0", "development", slog.LevelDebug)
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(observability.Middleware(logger))
	r.Use(observability.Recoverer)
	r.Use(middleware.RequestSize(1 << 20)) // 1MB body limit

	// --- Health checks ---

	r.Get("/health/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	r.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		// TODO: check downstream dependencies (DB, cache, external APIs)
		ready := true
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "unavailable"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// --- API endpoints (with per-route timeout) ---

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(10 * time.Second))

		r.Get("/process", func(w http.ResponseWriter, r *http.Request) {
			err := handleBusinessLogic(r.Context())
			if err != nil {
				http.Error(w, "server error", 500)
				return
			}
			w.Write([]byte("ok"))
		})

		r.Post("/orders", func(w http.ResponseWriter, r *http.Request) {
			ctx, span := observability.StartSpan(r.Context(), "create_order")
			defer span.End()

			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				span.SetError(err)
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}

			span.AddAttribute("item", body["item"])
			span.AddEvent("validating_order")
			time.Sleep(80 * time.Millisecond)

			observability.LoggerFromCtx(ctx).Info("order created", slog.Any("order", body))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "ord_abc123",
				"status": "created",
				"item":   body["item"],
			})
		})

		r.Get("/error", func(w http.ResponseWriter, r *http.Request) {
			_, span := observability.StartSpan(r.Context(), "forced_error")
			defer span.End()

			err := errors.New("something went wrong")
			span.SetError(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		})

		r.Get("/panic", func(w http.ResponseWriter, r *http.Request) {
			panic("something unexpected happened")
		})
	})

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           r,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Background worker with cancellable context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runBackgroundService(ctx, logger)

	// Start server in a goroutine so we can listen for shutdown signals
	go func() {
		logger.Info("server starting", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	// Block until shutdown signal
	<-ctx.Done()
	logger.Info("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", slog.String("error", err.Error()))
		os.Exit(1)
	}

	logger.Info("server stopped")
}

func handleBusinessLogic(ctx context.Context) error {
	ctx, span := observability.StartSpan(ctx, "payment_flow")
	defer span.End()

	span.AddAttribute("user_id", "user_123")
	span.AddEvent("contacting_gateway")

	// Simulate work
	time.Sleep(100 * time.Millisecond)

	// Simulate error
	if time.Now().Unix()%2 == 0 {
		err := errors.New("gateway timeout")
		span.SetError(err)
		return err
	}

	return nil
}

// runBackgroundService is a long-running background service that runs alongside
// the HTTP server. It respects context cancellation for clean shutdown.
func runBackgroundService(ctx context.Context, logger *slog.Logger) {
	ctx = observability.WithLogger(ctx, logger)
	logger.Info("background service started")

	for {
		func() {
			ctx, span := observability.StartSpan(ctx, "daily_cleanup")
			defer span.End()

			observability.LoggerFromCtx(ctx).Info("cleaning records...")
			time.Sleep(50 * time.Millisecond)
		}()

		select {
		case <-ctx.Done():
			logger.Info("background service stopped")
			return
		case <-time.After(30 * time.Second):
		}
	}
}
