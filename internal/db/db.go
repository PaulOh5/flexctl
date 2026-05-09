// Package db opens and migrates the SQLite store used by the control plane.
//
// We use modernc.org/sqlite (pure Go, no CGO) so the resulting binary is a
// single static file that ships cleanly to any GPU host.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// Open opens the SQLite database at path with WAL, foreign keys, and an
// IMMEDIATE transaction lock so that BeginTx serializes writers.
//
// Concurrent allocations rely on the IMMEDIATE lock plus the
// `gpu_alloc_active` UNIQUE partial index to prevent two callers from
// attaching the same GPU to two different environments.
func Open(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")

	dsn := fmt.Sprintf("file:%s?%s", path, q.Encode())
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	// modernc.org/sqlite is single-writer at the file level; cap the pool
	// to a small number to avoid SQLITE_BUSY storms under load.
	d.SetMaxOpenConns(8)
	d.SetMaxIdleConns(4)

	if err := d.Ping(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return d, nil
}

// schemaSQL is the full schema. Idempotent on repeat runs.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS gpu_inventory (
    uuid              TEXT PRIMARY KEY,
    device_index      INTEGER NOT NULL,
    name              TEXT NOT NULL,
    memory_total_mb   INTEGER NOT NULL,
    last_seen_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id          TEXT PRIMARY KEY,
    email       TEXT UNIQUE NOT NULL,
    host_uid    INTEGER UNIQUE NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS environments (
    id                    TEXT PRIMARY KEY,
    user_id               TEXT NOT NULL REFERENCES users(id),
    state                 TEXT NOT NULL CHECK (state IN ('pending','running','stopping','stopped','failed')),
    image                 TEXT NOT NULL,
    sidecar_container_id  TEXT,
    work_container_id     TEXT,
    tailscale_hostname    TEXT,
    created_at            INTEGER NOT NULL,
    started_at            INTEGER,
    stopped_at            INTEGER,
    last_exit_reason      TEXT
);

CREATE TABLE IF NOT EXISTS gpu_allocation (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    gpu_uuid            TEXT NOT NULL REFERENCES gpu_inventory(uuid),
    user_id             TEXT NOT NULL REFERENCES users(id),
    env_id              TEXT NOT NULL REFERENCES environments(id),
    state               TEXT NOT NULL CHECK (state IN ('allocated','released','failed')),
    leased_at           INTEGER NOT NULL,
    heartbeat_at        INTEGER NOT NULL,
    lease_ttl_seconds   INTEGER NOT NULL DEFAULT 30,
    released_at         INTEGER
);

-- Race-safe at-most-one-active-allocation invariant.
CREATE UNIQUE INDEX IF NOT EXISTS gpu_alloc_active
    ON gpu_allocation(gpu_uuid) WHERE state='allocated';

CREATE INDEX IF NOT EXISTS gpu_alloc_env ON gpu_allocation(env_id);
CREATE INDEX IF NOT EXISTS gpu_alloc_user ON gpu_allocation(user_id);
`

// Migrate creates tables and indexes if missing.
func Migrate(ctx context.Context, d *sql.DB) error {
	if _, err := d.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return nil
}
