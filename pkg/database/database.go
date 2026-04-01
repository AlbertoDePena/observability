package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// -------------------------------------------------------------------------
// URI builders
// -------------------------------------------------------------------------

// rwURI builds a SQLite URI for a writable connection.
// Pragmas are set here rather than via EXEC so they are applied before the
// connection is returned to database/sql's pool.
func rwURI(path string) string {
	return fmt.Sprintf(
		"file:%s"+
			// WAL allows concurrent readers alongside the single writer.
			"?_pragma=journal_mode(WAL)"+
			// NORMAL is safe with WAL and much faster than FULL.
			"&_pragma=synchronous(NORMAL)"+
			// Cap WAL file size; uncapped growth hurts read performance.
			"&_pragma=journal_size_limit(67108864)"+ // 64 MiB
			// Shared memory map — equivalent to Postgres' buffer pool.
			"&_pragma=mmap_size(134217728)"+ // 128 MiB
			// Per-connection page cache (4 KiB pages → ~8 MiB).
			"&_pragma=cache_size(2000)"+
			// Enforce FK constraints.
			"&_pragma=foreign_keys(1)"+
			// Busy timeout for write contention (5 seconds).
			"&_pragma=busy_timeout(5000)",
		path,
	)
}

// roURI builds a SQLite URI for a read-only connection.
func roURI(path string) string {
	return fmt.Sprintf(
		"file:%s"+
			// Open in read-only mode at the OS level.
			"?mode=ro"+
			// query_only is a belt-and-suspenders guard against accidental writes.
			"&_pragma=query_only(1)"+
			// Readers are not writing, so synchronous mode doesn't matter much,
			// but NORMAL avoids any unnecessary fsync calls.
			"&_pragma=synchronous(NORMAL)"+
			// Larger cache benefits read-heavy workloads.
			"&_pragma=cache_size(4000)"+ // ~16 MiB
			// Memory-mapped I/O is especially beneficial for readers.
			"&_pragma=mmap_size(268435456)"+ // 256 MiB — double the writer
			// Short busy timeout for the rare checkpoint contention case.
			"&_pragma=busy_timeout(50)",
		path,
	)
}

// -------------------------------------------------------------------------
// DB — wraps the two pools behind a single type
// -------------------------------------------------------------------------

// DB holds separate connection pools for reads and writes. Callers use
// Query/QueryContext for reads and Exec/ExecContext for writes; the routing
// is explicit so there's no magic.
type DB struct {
	rw *sql.DB // single-connection writer pool
	ro *sql.DB // multi-connection reader pool
}

func Open(ctx context.Context, path string) (*DB, error) {
	rw, err := sql.Open("sqlite", rwURI(path))
	if err != nil {
		return nil, fmt.Errorf("open rw: %w", err)
	}
	// Single writer: SQLite only supports one concurrent writer anyway.
	rw.SetMaxOpenConns(1)
	rw.SetMaxIdleConns(1)
	rw.SetConnMaxLifetime(0) // keep the connection alive; WAL state is per-conn

	if err := rw.PingContext(ctx); err != nil {
		rw.Close()
		return nil, fmt.Errorf("ping rw: %w", err)
	}

	ro, err := sql.Open("sqlite", roURI(path))
	if err != nil {
		rw.Close()
		return nil, fmt.Errorf("open ro: %w", err)
	}
	// Many readers are fine under WAL — they never block the writer.
	ro.SetMaxOpenConns(4) // tune to your workload / GOMAXPROCS
	ro.SetMaxIdleConns(4)
	ro.SetConnMaxLifetime(time.Hour)

	if err := ro.PingContext(ctx); err != nil {
		rw.Close()
		ro.Close()
		return nil, fmt.Errorf("ping ro: %w", err)
	}

	return &DB{rw: rw, ro: ro}, nil
}

func (db *DB) Close() error {
	return errors.Join(db.ro.Close(), db.rw.Close())
}

// QueryContext runs a read query on the reader pool.
func (db *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return db.ro.QueryContext(ctx, q, args...)
}

// QueryRowContext runs a single-row read query on the reader pool.
func (db *DB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return db.ro.QueryRowContext(ctx, q, args...)
}

// ExecContext runs a write statement on the writer pool.
func (db *DB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return db.rw.ExecContext(ctx, q, args...)
}

// PrepareReadContext prepares a read statement on the reader pool.
func (db *DB) PrepareReadContext(ctx context.Context, q string) (*sql.Stmt, error) {
	return db.ro.PrepareContext(ctx, q)
}

// PrepareWriteContext prepares a write statement on the writer pool.
func (db *DB) PrepareWriteContext(ctx context.Context, q string) (*sql.Stmt, error) {
	return db.rw.PrepareContext(ctx, q)
}

// BeginTx starts a transaction. Write transactions must use the rw pool;
// read-only transactions can use the ro pool.
func (db *DB) BeginTx(ctx context.Context, readOnly bool) (*sql.Tx, error) {
	if readOnly {
		return db.ro.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	}
	return db.rw.BeginTx(ctx, nil)
}

// PingContext pings both the reader and writer pools.
func (db *DB) PingContext(ctx context.Context) error {
	return errors.Join(db.rw.PingContext(ctx), db.ro.PingContext(ctx))
}
