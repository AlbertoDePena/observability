package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"observability/internal/handlers"
	"observability/internal/worker"
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

	r.Get("/health/live", handlers.LivenessCheck)
	r.Get("/health/ready", handlers.ReadinessCheck)

	// --- API endpoints (with per-route timeout) ---

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(10 * time.Second))

		r.Get("/process", handlers.Process)
		r.Post("/orders", handlers.CreateOrder)
		r.Get("/error", handlers.ForceError)
		r.Get("/panic", handlers.ForcePanic)
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

	go worker.RunBackgroundService(ctx, logger)

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
