// Package store provides the SQLite persistence layer for Switchboard.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

// Store wraps a SQLite database for audit logs, durable inbox, and state.
type Store struct {
	db *sql.DB
}

// New creates or opens the SQLite database in the given data directory.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dataDir, err)
	}

	dbPath := filepath.Join(dataDir, "switchboard.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// Close shuts down the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for use by other packages.
func (s *Store) DB() *sql.DB {
	return s.db
}

func migrate(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL DEFAULT (datetime('now')),
		direction TEXT NOT NULL,
		channel_id TEXT,
		session_id TEXT,
		message_preview TEXT,
		metadata TEXT
	);

	CREATE TABLE IF NOT EXISTS durable_inbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source TEXT NOT NULL,
		payload TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		delivered_at TEXT
	);

	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		channel_id TEXT NOT NULL,
		workdir TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		last_active TEXT
	);

	CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log(timestamp);
	CREATE INDEX IF NOT EXISTS idx_inbox_pending ON durable_inbox(delivered_at) WHERE delivered_at IS NULL;
	`
	_, err := db.Exec(schema)
	return err
}
