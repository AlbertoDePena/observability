package database

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestDB creates a DB backed by a real file in t.TempDir.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	_, err = db.ExecContext(ctx, `CREATE TABLE items (
		id    INTEGER PRIMARY KEY,
		name  TEXT NOT NULL,
		value REAL NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

// -------------------------------------------------------------------------
// Functional tests
// -------------------------------------------------------------------------

func TestOpenAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpenInvalidPath(t *testing.T) {
	ctx := context.Background()
	_, err := Open(ctx, "/nonexistent/dir/test.db")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestExecAndQuery(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	res, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`, "alpha", 1.5)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()
	if id != 1 {
		t.Fatalf("expected id 1, got %d", id)
	}

	var name string
	var value float64
	row := db.QueryRowContext(ctx, `SELECT name, value FROM items WHERE id = ?`, id)
	if err := row.Scan(&name, &value); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != "alpha" || value != 1.5 {
		t.Fatalf("got (%q, %f), want (alpha, 1.5)", name, value)
	}
}

func TestQueryContextMultipleRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for i := range 5 {
		_, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
			fmt.Sprintf("item-%d", i), float64(i))
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	rows, err := db.QueryContext(ctx, `SELECT id, name FROM items ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if count != 5 {
		t.Fatalf("expected 5 rows, got %d", count)
	}
}

func TestReadOnlyConnectionRejectsWrites(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, true)
	if err != nil {
		t.Fatalf("begin ro tx: %v", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`, "bad", 0)
	if err == nil {
		t.Fatal("expected error writing through read-only connection")
	}
}

func TestBeginTxWriteCommit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, false)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`, "beta", 2.0)
	if err != nil {
		tx.Rollback()
		t.Fatalf("insert in tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var name string
	row := db.QueryRowContext(ctx, `SELECT name FROM items WHERE name = ?`, "beta")
	if err := row.Scan(&name); err != nil {
		t.Fatalf("read after commit: %v", err)
	}
}

func TestBeginTxRollback(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, false)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`, "gamma", 3.0)
	if err != nil {
		tx.Rollback()
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var count int
	row := db.QueryRowContext(ctx, `SELECT count(*) FROM items`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows after rollback, got %d", count)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `CREATE TABLE children (
		id        INTEGER PRIMARY KEY,
		parent_id INTEGER NOT NULL REFERENCES items(id)
	)`)
	if err != nil {
		t.Fatalf("create children table: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO children (parent_id) VALUES (999)`)
	if err == nil {
		t.Fatal("expected FK violation error")
	}
}

func TestWALModeEnabled(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	var mode string
	row := db.QueryRowContext(ctx, `PRAGMA journal_mode`)
	if err := row.Scan(&mode); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected wal, got %s", mode)
	}
}

func TestContextCancellation(t *testing.T) {
	db := newTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`, "x", 0)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// -------------------------------------------------------------------------
// Load tests
// -------------------------------------------------------------------------

func TestLoadConcurrentReads(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Seed data.
	for i := range 100 {
		_, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
			fmt.Sprintf("item-%d", i), float64(i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	const (
		readers  = 20
		duration = 2 * time.Second
	)

	deadline := time.Now().Add(duration)
	var ops atomic.Int64
	var errs atomic.Int64
	var wg sync.WaitGroup

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				id := rand.IntN(100) + 1
				var name string
				err := db.QueryRowContext(ctx, `SELECT name FROM items WHERE id = ?`, id).Scan(&name)
				if err != nil {
					errs.Add(1)
				}
				ops.Add(1)
			}
		}()
	}
	wg.Wait()

	t.Logf("concurrent reads: %d ops in %s (%d errors)", ops.Load(), duration, errs.Load())
	if errs.Load() > 0 {
		t.Fatalf("got %d read errors", errs.Load())
	}
}

func TestLoadConcurrentWrites(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const (
		writers  = 10
		duration = 2 * time.Second
	)

	deadline := time.Now().Add(duration)
	var ops atomic.Int64
	var errs atomic.Int64
	var wg sync.WaitGroup

	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
					"load", rand.Float64())
				if err != nil {
					errs.Add(1)
				}
				ops.Add(1)
			}
		}()
	}
	wg.Wait()

	var count int
	db.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count)
	t.Logf("concurrent writes: %d ops in %s (%d errors), %d rows", ops.Load(), duration, errs.Load(), count)
	if errs.Load() > 0 {
		t.Fatalf("got %d write errors", errs.Load())
	}
}

func TestLoadMixedReadWrite(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Seed some data so readers have something to query.
	for i := range 50 {
		_, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
			fmt.Sprintf("seed-%d", i), float64(i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	const (
		readers  = 16
		writers  = 4
		duration = 3 * time.Second
	)

	deadline := time.Now().Add(duration)
	var readOps, writeOps atomic.Int64
	var readErrs, writeErrs atomic.Int64
	var wg sync.WaitGroup

	// Readers
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				rows, err := db.QueryContext(ctx,
					`SELECT id, name, value FROM items ORDER BY id DESC LIMIT 10`)
				if err != nil {
					readErrs.Add(1)
					readOps.Add(1)
					continue
				}
				for rows.Next() {
					var id int
					var name string
					var value float64
					rows.Scan(&id, &name, &value)
				}
				rows.Close()
				readOps.Add(1)
			}
		}()
	}

	// Writers
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
					"mixed", rand.Float64()*1000)
				if err != nil {
					writeErrs.Add(1)
				}
				writeOps.Add(1)
			}
		}()
	}

	wg.Wait()

	var count int
	db.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count)

	t.Logf("mixed load over %s:", duration)
	t.Logf("  reads:  %d ops (%d errors)", readOps.Load(), readErrs.Load())
	t.Logf("  writes: %d ops (%d errors)", writeOps.Load(), writeErrs.Load())
	t.Logf("  total rows: %d", count)

	if readErrs.Load() > 0 {
		t.Fatalf("got %d read errors", readErrs.Load())
	}
	if writeErrs.Load() > 0 {
		t.Fatalf("got %d write errors", writeErrs.Load())
	}
}

func TestLoadTransactionContention(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Seed data.
	for i := range 10 {
		_, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
			fmt.Sprintf("tx-%d", i), float64(i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	const (
		workers  = 8
		iters    = 50
	)

	var errs atomic.Int64
	var wg sync.WaitGroup

	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range iters {
				// Alternate between read-only and write transactions.
				readOnly := i%2 == 0
				tx, err := db.BeginTx(ctx, readOnly)
				if err != nil {
					errs.Add(1)
					continue
				}
				if readOnly {
					var count int
					tx.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count)
					tx.Commit()
				} else {
					_, err = tx.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
						fmt.Sprintf("w%d-i%d", w, i), rand.Float64())
					if err != nil {
						tx.Rollback()
						errs.Add(1)
						continue
					}
					if err := tx.Commit(); err != nil {
						errs.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()

	var count int
	db.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count)
	t.Logf("transaction contention: %d workers × %d iters, %d errors, %d rows",
		workers, iters, errs.Load(), count)
	if errs.Load() > 0 {
		t.Fatalf("got %d transaction errors", errs.Load())
	}
}

func TestLoadBulkInsertThroughput(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const batchSize = 1000

	start := time.Now()
	tx, err := db.BeginTx(ctx, false)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := range batchSize {
		_, err := tx.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
			fmt.Sprintf("bulk-%d", i), float64(i))
		if err != nil {
			tx.Rollback()
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	elapsed := time.Since(start)

	var count int
	db.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count)
	if count != batchSize {
		t.Fatalf("expected %d rows, got %d", batchSize, count)
	}

	t.Logf("bulk insert: %d rows in %s (%.0f rows/s)", batchSize, elapsed,
		float64(batchSize)/elapsed.Seconds())
}

func TestPrepareReadContext(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for i := range 5 {
		_, err := db.ExecContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`,
			fmt.Sprintf("item-%d", i), float64(i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	stmt, err := db.PrepareReadContext(ctx, `SELECT name FROM items WHERE id = ?`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()

	for i := 1; i <= 5; i++ {
		var name string
		if err := stmt.QueryRowContext(ctx, i).Scan(&name); err != nil {
			t.Fatalf("query id=%d: %v", i, err)
		}
		if name != fmt.Sprintf("item-%d", i-1) {
			t.Fatalf("got %q, want item-%d", name, i-1)
		}
	}
}

func TestPrepareWriteContext(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	stmt, err := db.PrepareWriteContext(ctx, `INSERT INTO items (name, value) VALUES (?, ?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()

	for i := range 10 {
		_, err := stmt.ExecContext(ctx, fmt.Sprintf("prep-%d", i), float64(i))
		if err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
	}

	var count int
	db.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count)
	if count != 10 {
		t.Fatalf("expected 10 rows, got %d", count)
	}
}

func TestPingContext(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestOpenCancelledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// Create the file first so Open doesn't fail on missing file.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}
