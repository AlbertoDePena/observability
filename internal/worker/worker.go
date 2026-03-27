package worker

import (
	"context"
	"log/slog"
	"time"

	observability "observability/pkg"
)

// RunBackgroundService is a long-running background service that runs alongside
// the HTTP server. It respects context cancellation for clean shutdown.
func RunBackgroundService(ctx context.Context, logger *slog.Logger) {
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
