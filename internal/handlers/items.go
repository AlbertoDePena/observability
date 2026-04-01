package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"observability/pkg/database"

	"github.com/go-chi/chi/v5"
)

type Item struct {
	ID    int64   `json:"id"`
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

type ItemHandler struct {
	db *database.DB
}

func NewItemHandler(db *database.DB) *ItemHandler {
	return &ItemHandler{db: db}
}

// CreateItem inserts a new item and returns it with the generated ID.
func (h *ItemHandler) CreateItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string  `json:"name"`
		Value float64 `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	res, err := h.db.ExecContext(r.Context(),
		`INSERT INTO items (name, value) VALUES (?, ?)`, req.Name, req.Value)
	if err != nil {
		http.Error(w, "insert failed", http.StatusInternalServerError)
		return
	}

	id, _ := res.LastInsertId()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(Item{ID: id, Name: req.Name, Value: req.Value})
}

// GetItem returns a single item by ID.
func (h *ItemHandler) GetItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	var item Item
	err = h.db.QueryRowContext(r.Context(),
		`SELECT id, name, value FROM items WHERE id = ?`, id).
		Scan(&item.ID, &item.Name, &item.Value)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(item)
}

// ListItems returns all items.
func (h *ItemHandler) ListItems(w http.ResponseWriter, r *http.Request) {
	const maxItems = 1000
	rows, err := h.db.QueryContext(r.Context(),
		`SELECT id, name, value FROM items ORDER BY id LIMIT ?`, maxItems)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	items := make([]Item, 0)
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.Name, &item.Value); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "rows error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}
