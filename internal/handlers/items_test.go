package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"observability/pkg/database"

	"github.com/go-chi/chi/v5"
)

// newTestServer returns a chi router wired to a real SQLite DB and the item handler.
func newTestServer(t *testing.T) (*database.DB, http.Handler) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.ExecContext(ctx, `CREATE TABLE items (
		id    INTEGER PRIMARY KEY,
		name  TEXT NOT NULL,
		value REAL NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	h := NewItemHandler(db)
	r := chi.NewRouter()
	r.Post("/items", h.CreateItem)
	r.Get("/items", h.ListItems)
	r.Get("/items/{id}", h.GetItem)

	return db, r
}

// -------------------------------------------------------------------------
// Functional tests
// -------------------------------------------------------------------------

func TestCreateItem(t *testing.T) {
	_, handler := newTestServer(t)

	body := `{"name":"widget","value":9.99}`
	req := httptest.NewRequest(http.MethodPost, "/items", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var item Item
	json.NewDecoder(w.Body).Decode(&item)
	if item.ID != 1 || item.Name != "widget" || item.Value != 9.99 {
		t.Fatalf("unexpected item: %+v", item)
	}
}

func TestCreateItemMissingName(t *testing.T) {
	_, handler := newTestServer(t)

	body := `{"value":1.0}`
	req := httptest.NewRequest(http.MethodPost, "/items", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateItemInvalidBody(t *testing.T) {
	_, handler := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/items", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetItem(t *testing.T) {
	_, handler := newTestServer(t)

	// Create an item first.
	body := `{"name":"gadget","value":4.50}`
	req := httptest.NewRequest(http.MethodPost, "/items", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Fetch it.
	req = httptest.NewRequest(http.MethodGet, "/items/1", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var item Item
	json.NewDecoder(w.Body).Decode(&item)
	if item.Name != "gadget" {
		t.Fatalf("expected gadget, got %s", item.Name)
	}
}

func TestGetItemNotFound(t *testing.T) {
	_, handler := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/items/999", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetItemInvalidID(t *testing.T) {
	_, handler := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/items/abc", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestListItems(t *testing.T) {
	_, handler := newTestServer(t)

	// Create a few items.
	for _, name := range []string{"a", "b", "c"} {
		body := fmt.Sprintf(`{"name":"%s","value":1.0}`, name)
		req := httptest.NewRequest(http.MethodPost, "/items", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var items []Item
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
}

func TestListItemsEmpty(t *testing.T) {
	_, handler := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var items []Item
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

// -------------------------------------------------------------------------
// Load tests
// -------------------------------------------------------------------------

func TestLoadCreateAndGet(t *testing.T) {
	_, handler := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := ts.Client()
	client.Transport = &http.Transport{
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     50,
	}

	const (
		writers  = 10
		readers  = 20
		duration = 3 * time.Second
	)

	deadline := time.Now().Add(duration)
	var writeOps, readOps atomic.Int64
	var writeErrs, readErrs atomic.Int64
	var maxID atomic.Int64
	var wg sync.WaitGroup

	// Writers: POST /items
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				body := fmt.Sprintf(`{"name":"load-%d","value":%.2f}`,
					rand.IntN(100000), rand.Float64()*100)
				resp, err := client.Post(ts.URL+"/items", "application/json",
					bytes.NewBufferString(body))
				if err != nil {
					writeErrs.Add(1)
					writeOps.Add(1)
					continue
				}
				if resp.StatusCode != http.StatusCreated {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					writeErrs.Add(1)
				} else {
					var item Item
					json.NewDecoder(resp.Body).Decode(&item)
					resp.Body.Close()
					for {
						cur := maxID.Load()
						if item.ID <= cur || maxID.CompareAndSwap(cur, item.ID) {
							break
						}
					}
				}
				writeOps.Add(1)
			}
		}()
	}

	// Readers: GET /items/{id} with random IDs
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				id := maxID.Load()
				if id == 0 {
					readOps.Add(1)
					continue
				}
				target := rand.Int64N(id) + 1
				resp, err := client.Get(fmt.Sprintf("%s/items/%d", ts.URL, target))
				if err != nil {
					readErrs.Add(1)
					readOps.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					readErrs.Add(1)
				}
				readOps.Add(1)
			}
		}()
	}

	wg.Wait()

	t.Logf("HTTP load test over %s:", duration)
	t.Logf("  writes: %d ops (%d errors)", writeOps.Load(), writeErrs.Load())
	t.Logf("  reads:  %d ops (%d errors)", readOps.Load(), readErrs.Load())

	if writeErrs.Load() > 0 {
		t.Fatalf("got %d write errors", writeErrs.Load())
	}
	if readErrs.Load() > 0 {
		t.Fatalf("got %d read errors", readErrs.Load())
	}
}

func TestLoadListUnderConcurrentWrites(t *testing.T) {
	_, handler := newTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := ts.Client()
	client.Transport = &http.Transport{
		MaxIdleConnsPerHost: 30,
		MaxConnsPerHost:     30,
	}

	const (
		writers  = 5
		listers  = 10
		duration = 2 * time.Second
	)

	deadline := time.Now().Add(duration)
	var writeOps, listOps atomic.Int64
	var writeErrs, listErrs atomic.Int64
	var wg sync.WaitGroup

	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				body := fmt.Sprintf(`{"name":"list-test","value":%.2f}`, rand.Float64()*100)
				resp, err := client.Post(ts.URL+"/items", "application/json",
					bytes.NewBufferString(body))
				if err != nil {
					writeErrs.Add(1)
				} else {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != http.StatusCreated {
						writeErrs.Add(1)
					}
				}
				writeOps.Add(1)
			}
		}()
	}

	for range listers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				resp, err := client.Get(ts.URL + "/items")
				if err != nil {
					listErrs.Add(1)
				} else {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						listErrs.Add(1)
					}
				}
				listOps.Add(1)
			}
		}()
	}

	wg.Wait()

	t.Logf("list under writes over %s:", duration)
	t.Logf("  writes: %d ops (%d errors)", writeOps.Load(), writeErrs.Load())
	t.Logf("  lists:  %d ops (%d errors)", listOps.Load(), listErrs.Load())

	if writeErrs.Load() > 0 {
		t.Fatalf("got %d write errors", writeErrs.Load())
	}
	if listErrs.Load() > 0 {
		t.Fatalf("got %d list errors", listErrs.Load())
	}
}
