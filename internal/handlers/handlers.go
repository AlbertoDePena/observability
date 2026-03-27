package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	observability "observability/pkg"
)

// LivenessCheck returns 200 OK if the server is running.
func LivenessCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ReadinessCheck returns 200 OK if downstream dependencies are healthy.
func ReadinessCheck(w http.ResponseWriter, r *http.Request) {
	// TODO: check downstream dependencies (DB, cache, external APIs)
	ready := true
	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "unavailable"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Process handles the main business logic endpoint.
func Process(w http.ResponseWriter, r *http.Request) {
	err := handleBusinessLogic(r.Context())
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	w.Write([]byte("ok"))
}

// CreateOrder handles order creation with span tracing.
func CreateOrder(w http.ResponseWriter, r *http.Request) {
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
}

// ForceError intentionally returns an error for testing error handling.
func ForceError(w http.ResponseWriter, r *http.Request) {
	_, span := observability.StartSpan(r.Context(), "forced_error")
	defer span.End()

	err := errors.New("something went wrong")
	span.SetError(err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// ForcePanic intentionally panics for testing recovery middleware.
func ForcePanic(w http.ResponseWriter, r *http.Request) {
	panic("something unexpected happened")
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
