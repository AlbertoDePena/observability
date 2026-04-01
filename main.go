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
	"observability/pkg/database"
	observability "observability/pkg/observability"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	logger := observability.Init("my-api", "1.0.0", "development", slog.LevelDebug)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, "app.db")
	if err != nil {
		logger.Error("database open failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	// Create tables if they don't exist.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS items (
		id    INTEGER PRIMARY KEY,
		name  TEXT NOT NULL,
		value REAL NOT NULL
	)`); err != nil {
		db.Close()
		logger.Error("migrate failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	itemHandler := handlers.NewItemHandler(db)
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

		r.Post("/items", itemHandler.CreateItem)
		r.Get("/items", itemHandler.ListItems)
		r.Get("/items/{id}", itemHandler.GetItem)
	})

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           r,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go worker.RunBackgroundService(ctx, logger)

	// Start server in a goroutine so we can listen for shutdown signals.
	srvErr := make(chan error, 1)
	go func() {
		logger.Info("server starting", slog.String("addr", srv.Addr))
		srvErr <- srv.ListenAndServe()
	}()

	// Block until shutdown signal or server error.
	select {
	case <-ctx.Done():
		logger.Info("shutting down...")
	case err := <-srvErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", slog.String("error", err.Error()))
		}
	}

	// 1. Drain in-flight HTTP requests.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", slog.String("error", err.Error()))
	}

	// 2. Close the database after all requests have finished.
	if err := db.Close(); err != nil {
		logger.Error("database close error", slog.String("error", err.Error()))
	}

	logger.Info("server stopped")
}
