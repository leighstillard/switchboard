// Package store provides the SQLite persistence layer for Switchboard (spec §6).
//
// It manages sessions, turn queues, thread correlations, webhook inbox,
// and audit logging with WAL mode, integrity checks, and versioned migrations.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// Session represents a mapping between a Slack thread and a jcode session.
type Session struct {
	ChannelID    string
	ThreadTS     string
	JcodeSession string
	FriendlyName string
	Workdir      string
	CreatedAt    int64
	LastActivity int64
	Status       string // "idle", "processing", "closed"
	ExpiresAt    *int64
}

// TurnQueueItem represents a queued user message awaiting delivery.
type TurnQueueItem struct {
	ID              int64
	ChannelID       string
	ThreadTS        string
	EnqueuedAt      int64
	UserID          string
	Text            string
	AttachmentsJSON *string
}

// ThreadCorrelation maps an external event (e.g. GitHub PR) to a Slack thread.
type ThreadCorrelation struct {
	Source      string
	ExternalKey string
	ChannelID   string
	ThreadTS    string
	CreatedAt   int64
	ExpiresAt   *int64
	CreatedBy   string
}

// WebhookInboxItem represents a durable webhook payload.
type WebhookInboxItem struct {
	ID             int64
	ReceivedAt     int64
	Source         string
	IdempotencyKey string
	HeadersJSON    string
	BodyBlob       []byte
	Status         string // "pending", "processing", "done", "failed"
	Attempts       int
	LastError      *string
}

// AuditEntry is an immutable audit log record.
type AuditEntry struct {
	ID                 int64
	TS                 int64
	Source             string
	EventType          string
	ChannelID          *string
	ThreadTS           *string
	RoutedBy           *string
	RoutingConfidence  *int
	PayloadSummaryJSON string
	PayloadHash        string
}

// MaintenanceConfig controls the nightly cleanup job.
type MaintenanceConfig struct {
	AuditRetentionDays         int
	DoneWebhookRetentionDays   int
	FailedWebhookRetentionDays int
	MaxCorrelationRows         int
}

// CronJob represents a runtime-managed cron job stored in SQLite.
type CronJob struct {
	ID        string
	Schedule  string
	ChannelID string
	Prompt    string
	UserID    string
	Enabled   bool
	CreatedAt int64
	UpdatedAt int64
}

// LLMRoutingDecision records an LLM routing decision for audit.
type LLMRoutingDecision struct {
	ID             int64
	WebhookInboxID *int64 // nullable; set when durable inbox is used
	DecidedAt      int64
	Model          string
	ThreadID       *string
	Confidence     int
	Reasoning      *string
	PostedTo       string // "suggested" or "fallback"
	UserFeedback   *string // "confirmed", "rejected", nil
	FeedbackAt     *int64
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// Store wraps a SQLite database providing all Switchboard persistence.
// It is safe for concurrent use from multiple goroutines.
type Store struct {
	db *sql.DB
	mu sync.Mutex // serialises write operations for SQLite
}

// New creates or opens the SQLite database in dataDir/switchboard.db.
// It enables WAL mode, runs an integrity check, and applies migrations.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dataDir, err)
	}

	dbPath := filepath.Join(dataDir, "switchboard.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}

	// Connection pool settings for SQLite: one writer, multiple readers via WAL.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0) // never expire

	// Enable WAL mode and set busy timeout.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}

	// Integrity check -- refuse to start on corruption.
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: integrity_check query failed: %w", err)
	}
	if result != "ok" {
		db.Close()
		return nil, fmt.Errorf("store: database integrity check failed: %s", result)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// Close shuts down the database connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for advanced use by other packages.
func (s *Store) DB() *sql.DB {
	return s.db
}

// ---------------------------------------------------------------------------
// Migrations (PRAGMA user_version)
// ---------------------------------------------------------------------------

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	if version < 1 {
		if err := migrateV1(db); err != nil {
			return fmt.Errorf("v1: %w", err)
		}
	}
	if version < 2 {
		if err := migrateV2(db); err != nil {
			return fmt.Errorf("v2: %w", err)
		}
	}
	if version < 3 {
		if err := migrateV3(db); err != nil {
			return fmt.Errorf("v3: %w", err)
		}
	}

	return nil
}

func migrateV1(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		// -- sessions
		`CREATE TABLE IF NOT EXISTS sessions (
			channel_id      TEXT NOT NULL,
			thread_ts       TEXT NOT NULL,
			jcode_session   TEXT NOT NULL,
			friendly_name   TEXT,
			workdir         TEXT NOT NULL,
			created_at      INTEGER NOT NULL,
			last_activity   INTEGER NOT NULL,
			status          TEXT NOT NULL,
			expires_at      INTEGER,
			PRIMARY KEY (channel_id, thread_ts)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_jcode ON sessions(jcode_session)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_active ON sessions(status, last_activity)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at) WHERE expires_at IS NOT NULL`,

		// -- turn_queue
		`CREATE TABLE IF NOT EXISTS turn_queue (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_id      TEXT NOT NULL,
			thread_ts       TEXT NOT NULL,
			enqueued_at     INTEGER NOT NULL,
			user_id         TEXT NOT NULL,
			text            TEXT NOT NULL,
			attachments_json TEXT,
			FOREIGN KEY (channel_id, thread_ts) REFERENCES sessions(channel_id, thread_ts)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_turn_queue_session ON turn_queue(channel_id, thread_ts, enqueued_at)`,

		// -- thread_correlations
		`CREATE TABLE IF NOT EXISTS thread_correlations (
			source          TEXT NOT NULL,
			external_key    TEXT NOT NULL,
			channel_id      TEXT NOT NULL,
			thread_ts       TEXT NOT NULL,
			created_at      INTEGER NOT NULL,
			expires_at      INTEGER,
			created_by      TEXT NOT NULL,
			PRIMARY KEY (source, external_key, channel_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_corr_thread ON thread_correlations(channel_id, thread_ts)`,
		`CREATE INDEX IF NOT EXISTS idx_corr_expires ON thread_correlations(expires_at) WHERE expires_at IS NOT NULL`,

		// -- webhook_inbox
		`CREATE TABLE IF NOT EXISTS webhook_inbox (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			received_at     INTEGER NOT NULL,
			source          TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			headers_json    TEXT NOT NULL,
			body_blob       BLOB NOT NULL,
			status          TEXT NOT NULL,
			attempts        INTEGER NOT NULL DEFAULT 0,
			last_error      TEXT,
			UNIQUE (source, idempotency_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_inbox_pending ON webhook_inbox(status, received_at) WHERE status = 'pending'`,

		// -- audit_log
		`CREATE TABLE IF NOT EXISTS audit_log (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			ts            INTEGER NOT NULL,
			source        TEXT NOT NULL,
			event_type    TEXT NOT NULL,
			channel_id    TEXT,
			thread_ts     TEXT,
			routed_by     TEXT,
			routing_confidence INTEGER,
			payload_summary_json TEXT NOT NULL,
			payload_hash  TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_routing ON audit_log(routed_by, ts)`,

		// -- set version
		`PRAGMA user_version = 1`,
	}

	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:min(len(stmt), 60)], err)
		}
	}

	return tx.Commit()
}

func migrateV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		// -- LLM routing decisions (v1.1 Feature 2)
		`CREATE TABLE IF NOT EXISTS llm_routing_decisions (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			webhook_inbox_id INTEGER REFERENCES webhook_inbox(id) ON DELETE SET NULL,
			decided_at       INTEGER NOT NULL,
			model            TEXT NOT NULL,
			thread_id        TEXT,
			confidence       INTEGER NOT NULL,
			reasoning        TEXT,
			posted_to        TEXT NOT NULL,
			user_feedback    TEXT,
			feedback_at      INTEGER
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_llm_routing_inbox ON llm_routing_decisions(webhook_inbox_id) WHERE webhook_inbox_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_llm_routing_feedback ON llm_routing_decisions(user_feedback, decided_at)`,

		// -- set version
		`PRAGMA user_version = 2`,
	}

	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:min(len(stmt), 60)], err)
		}
	}

	return tx.Commit()
}

func migrateV3(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		// -- Runtime cron jobs (managed via CLI)
		`CREATE TABLE IF NOT EXISTS cron_jobs (
			id          TEXT PRIMARY KEY,
			schedule    TEXT NOT NULL,
			channel_id  TEXT NOT NULL,
			prompt      TEXT NOT NULL,
			user_id     TEXT NOT NULL DEFAULT '',
			enabled     INTEGER NOT NULL DEFAULT 1,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL
		)`,

		// -- set version
		`PRAGMA user_version = 3`,
	}

	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:min(len(stmt), 60)], err)
		}
	}

	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Cron Jobs
// ---------------------------------------------------------------------------

// InsertCronJob inserts a new runtime cron job. Fails if the ID already exists.
func (s *Store) InsertCronJob(job *CronJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO cron_jobs (id, schedule, channel_id, prompt, user_id, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Schedule, job.ChannelID, job.Prompt, job.UserID,
		boolToInt(job.Enabled), job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert cron job: %w", err)
	}
	return nil
}

// GetCronJob retrieves a cron job by ID. Returns nil, nil if not found.
func (s *Store) GetCronJob(id string) (*CronJob, error) {
	row := s.db.QueryRow(`
		SELECT id, schedule, channel_id, prompt, user_id, enabled, created_at, updated_at
		FROM cron_jobs WHERE id = ?`, id)

	job := &CronJob{}
	var enabled int
	err := row.Scan(&job.ID, &job.Schedule, &job.ChannelID, &job.Prompt,
		&job.UserID, &enabled, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get cron job: %w", err)
	}
	job.Enabled = enabled != 0
	return job, nil
}

// ListCronJobs returns all runtime cron jobs ordered by ID.
func (s *Store) ListCronJobs() ([]*CronJob, error) {
	rows, err := s.db.Query(`
		SELECT id, schedule, channel_id, prompt, user_id, enabled, created_at, updated_at
		FROM cron_jobs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list cron jobs: %w", err)
	}
	defer rows.Close()

	var out []*CronJob
	for rows.Next() {
		job := &CronJob{}
		var enabled int
		if err := rows.Scan(&job.ID, &job.Schedule, &job.ChannelID, &job.Prompt,
			&job.UserID, &enabled, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan cron job: %w", err)
		}
		job.Enabled = enabled != 0
		out = append(out, job)
	}
	return out, rows.Err()
}

// DeleteCronJob removes a cron job by ID. Returns an error if not found.
func (s *Store) DeleteCronJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`DELETE FROM cron_jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete cron job: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("store: cron job %q not found", id)
	}
	return nil
}

// UpdateCronJobEnabled sets the enabled flag for a cron job.
func (s *Store) UpdateCronJobEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		UPDATE cron_jobs SET enabled = ?, updated_at = ?
		WHERE id = ?`,
		boolToInt(enabled), time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("store: update cron job enabled: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("store: cron job %q not found", id)
	}
	return nil
}

// boolToInt converts a bool to 0/1 for SQLite storage.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSession inserts a new session row.
func (s *Store) CreateSession(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO sessions (channel_id, thread_ts, jcode_session, friendly_name,
		                      workdir, created_at, last_activity, status, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ChannelID, sess.ThreadTS, sess.JcodeSession, nilIfEmpty(sess.FriendlyName),
		sess.Workdir, sess.CreatedAt, sess.LastActivity, sess.Status, sess.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// GetSession retrieves a session by its composite primary key.
func (s *Store) GetSession(channelID, threadTS string) (*Session, error) {
	return s.scanSession(s.db.QueryRow(`
		SELECT channel_id, thread_ts, jcode_session, friendly_name,
		       workdir, created_at, last_activity, status, expires_at
		FROM sessions
		WHERE channel_id = ? AND thread_ts = ?`, channelID, threadTS))
}

// GetSessionByJcodeID retrieves a session by its jcode session identifier.
func (s *Store) GetSessionByJcodeID(jcodeSession string) (*Session, error) {
	return s.scanSession(s.db.QueryRow(`
		SELECT channel_id, thread_ts, jcode_session, friendly_name,
		       workdir, created_at, last_activity, status, expires_at
		FROM sessions
		WHERE jcode_session = ?`, jcodeSession))
}

// UpdateSessionStatus sets the status of a session.
func (s *Store) UpdateSessionStatus(channelID, threadTS, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		UPDATE sessions SET status = ?, last_activity = ?
		WHERE channel_id = ? AND thread_ts = ?`,
		status, time.Now().Unix(), channelID, threadTS,
	)
	if err != nil {
		return fmt.Errorf("store: update session status: %w", err)
	}
	return expectOneRow(res, "session", channelID, threadTS)
}

// UpdateSessionActivity bumps last_activity to now.
func (s *Store) UpdateSessionActivity(channelID, threadTS string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		UPDATE sessions SET last_activity = ?
		WHERE channel_id = ? AND thread_ts = ?`,
		time.Now().Unix(), channelID, threadTS,
	)
	if err != nil {
		return fmt.Errorf("store: update session activity: %w", err)
	}
	return expectOneRow(res, "session", channelID, threadTS)
}

// DeleteSession removes a session record so it can be recreated with a new jcode session.
func (s *Store) DeleteSession(channelID, threadTS string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		DELETE FROM sessions
		WHERE channel_id = ? AND thread_ts = ?`,
		channelID, threadTS,
	)
	if err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// SetSessionFriendlyName sets or clears the friendly name.
func (s *Store) SetSessionFriendlyName(channelID, threadTS, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		UPDATE sessions SET friendly_name = ?
		WHERE channel_id = ? AND thread_ts = ?`,
		nilIfEmpty(name), channelID, threadTS,
	)
	if err != nil {
		return fmt.Errorf("store: set session friendly name: %w", err)
	}
	return expectOneRow(res, "session", channelID, threadTS)
}

// ListActiveSessions returns all sessions that are not closed.
func (s *Store) ListActiveSessions() ([]*Session, error) {
	rows, err := s.db.Query(`
		SELECT channel_id, thread_ts, jcode_session, friendly_name,
		       workdir, created_at, last_activity, status, expires_at
		FROM sessions
		WHERE status != 'closed'
		ORDER BY last_activity DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list active sessions: %w", err)
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		sess, err := s.scanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// ExpireSessions marks sessions whose expires_at has passed as closed.
// Returns the number of sessions expired.
func (s *Store) ExpireSessions() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		UPDATE sessions SET status = 'closed'
		WHERE expires_at IS NOT NULL
		  AND expires_at <= ?
		  AND status != 'closed'`,
		time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("store: expire sessions: %w", err)
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Turn Queue
// ---------------------------------------------------------------------------

// EnqueueTurn adds a user message to the turn queue.
func (s *Store) EnqueueTurn(t *TurnQueueItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		INSERT INTO turn_queue (channel_id, thread_ts, enqueued_at, user_id, text, attachments_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.ChannelID, t.ThreadTS, t.EnqueuedAt, t.UserID, t.Text, t.AttachmentsJSON,
	)
	if err != nil {
		return fmt.Errorf("store: enqueue turn: %w", err)
	}
	id, _ := res.LastInsertId()
	t.ID = id
	return nil
}

// CountTurns returns the number of queued turns for a session.
func (s *Store) CountTurns(channelID, threadTS string) (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM turn_queue
		WHERE channel_id = ? AND thread_ts = ?`,
		channelID, threadTS,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count turns: %w", err)
	}
	return n, nil
}

// DrainTurns atomically retrieves and deletes all queued turns for a session,
// ordered by enqueue time.
func (s *Store) DrainTurns(channelID, threadTS string) ([]*TurnQueueItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: drain turns begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id, channel_id, thread_ts, enqueued_at, user_id, text, attachments_json
		FROM turn_queue
		WHERE channel_id = ? AND thread_ts = ?
		ORDER BY enqueued_at ASC, id ASC`,
		channelID, threadTS,
	)
	if err != nil {
		return nil, fmt.Errorf("store: drain turns query: %w", err)
	}

	var items []*TurnQueueItem
	for rows.Next() {
		t := &TurnQueueItem{}
		if err := rows.Scan(&t.ID, &t.ChannelID, &t.ThreadTS, &t.EnqueuedAt,
			&t.UserID, &t.Text, &t.AttachmentsJSON); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: drain turns scan: %w", err)
		}
		items = append(items, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: drain turns rows: %w", err)
	}

	if len(items) > 0 {
		_, err = tx.Exec(`
			DELETE FROM turn_queue
			WHERE channel_id = ? AND thread_ts = ?`,
			channelID, threadTS,
		)
		if err != nil {
			return nil, fmt.Errorf("store: drain turns delete: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: drain turns commit: %w", err)
	}
	return items, nil
}

// DeleteOrphanedTurns removes turn_queue entries older than 24 hours.
// Returns the number of rows deleted.
func (s *Store) DeleteOrphanedTurns() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	res, err := s.db.Exec(`DELETE FROM turn_queue WHERE enqueued_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: delete orphaned turns: %w", err)
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Thread Correlations
// ---------------------------------------------------------------------------

// UpsertCorrelation inserts or updates a thread correlation.
func (s *Store) UpsertCorrelation(c *ThreadCorrelation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO thread_correlations
			(source, external_key, channel_id, thread_ts, created_at, expires_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, external_key, channel_id) DO UPDATE SET
			thread_ts  = excluded.thread_ts,
			expires_at = excluded.expires_at,
			created_by = excluded.created_by`,
		c.Source, c.ExternalKey, c.ChannelID, c.ThreadTS,
		c.CreatedAt, c.ExpiresAt, c.CreatedBy,
	)
	if err != nil {
		return fmt.Errorf("store: upsert correlation: %w", err)
	}
	return nil
}

// LookupCorrelation finds all threads correlated to a given source+key.
func (s *Store) LookupCorrelation(source, externalKey string) ([]*ThreadCorrelation, error) {
	rows, err := s.db.Query(`
		SELECT source, external_key, channel_id, thread_ts, created_at, expires_at, created_by
		FROM thread_correlations
		WHERE source = ? AND external_key = ?`,
		source, externalKey,
	)
	if err != nil {
		return nil, fmt.Errorf("store: lookup correlation: %w", err)
	}
	defer rows.Close()
	return scanCorrelations(rows)
}

// LookupCorrelationForThread finds all correlations associated with a thread.
func (s *Store) LookupCorrelationForThread(channelID, threadTS string) ([]*ThreadCorrelation, error) {
	rows, err := s.db.Query(`
		SELECT source, external_key, channel_id, thread_ts, created_at, expires_at, created_by
		FROM thread_correlations
		WHERE channel_id = ? AND thread_ts = ?`,
		channelID, threadTS,
	)
	if err != nil {
		return nil, fmt.Errorf("store: lookup correlation for thread: %w", err)
	}
	defer rows.Close()
	return scanCorrelations(rows)
}

// ExpireCorrelations removes correlations whose expires_at has passed.
// Returns the number of rows deleted.
func (s *Store) ExpireCorrelations() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		DELETE FROM thread_correlations
		WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("store: expire correlations: %w", err)
	}
	return res.RowsAffected()
}

// CapCorrelations keeps only the newest maxRows correlations.
// Returns the number of rows deleted.
func (s *Store) CapCorrelations(maxRows int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		DELETE FROM thread_correlations
		WHERE rowid NOT IN (
			SELECT rowid FROM thread_correlations
			ORDER BY created_at DESC
			LIMIT ?
		)`, maxRows,
	)
	if err != nil {
		return 0, fmt.Errorf("store: cap correlations: %w", err)
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Webhook Inbox
// ---------------------------------------------------------------------------

// InsertWebhook inserts a webhook payload. Returns (true, nil) on insert,
// (false, nil) if deduplicated (source+idempotency_key already exists).
func (s *Store) InsertWebhook(w *WebhookInboxItem) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		INSERT INTO webhook_inbox
			(received_at, source, idempotency_key, headers_json, body_blob, status, attempts, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, idempotency_key) DO NOTHING`,
		w.ReceivedAt, w.Source, w.IdempotencyKey, w.HeadersJSON,
		w.BodyBlob, w.Status, w.Attempts, w.LastError,
	)
	if err != nil {
		return false, fmt.Errorf("store: insert webhook: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		id, _ := res.LastInsertId()
		w.ID = id
	}
	return n > 0, nil
}

// ClaimPendingWebhook atomically finds and claims the oldest pending webhook
// for the given source by setting its status to "processing" and incrementing
// attempts. Returns nil, nil if none available.
func (s *Store) ClaimPendingWebhook(source string) (*WebhookInboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: claim webhook begin: %w", err)
	}
	defer tx.Rollback()

	var query string
	var args []interface{}
	if source != "" {
		query = `
			SELECT id, received_at, source, idempotency_key, headers_json,
			       body_blob, status, attempts, last_error
			FROM webhook_inbox
			WHERE source = ? AND status = 'pending'
			ORDER BY received_at ASC
			LIMIT 1`
		args = []interface{}{source}
	} else {
		query = `
			SELECT id, received_at, source, idempotency_key, headers_json,
			       body_blob, status, attempts, last_error
			FROM webhook_inbox
			WHERE status = 'pending'
			ORDER BY received_at ASC
			LIMIT 1`
	}

	row := tx.QueryRow(query, args...)

	w := &WebhookInboxItem{}
	err = row.Scan(&w.ID, &w.ReceivedAt, &w.Source, &w.IdempotencyKey,
		&w.HeadersJSON, &w.BodyBlob, &w.Status, &w.Attempts, &w.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: claim webhook scan: %w", err)
	}

	_, err = tx.Exec(`
		UPDATE webhook_inbox SET status = 'processing', attempts = attempts + 1
		WHERE id = ?`, w.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: claim webhook update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: claim webhook commit: %w", err)
	}

	w.Status = "processing"
	w.Attempts++
	return w, nil
}

// MarkWebhookDone sets a webhook's status to "done".
func (s *Store) MarkWebhookDone(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`UPDATE webhook_inbox SET status = 'done' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: mark webhook done: %w", err)
	}
	return nil
}

// MarkWebhookFailed sets a webhook's status to "failed" with an error message.
func (s *Store) MarkWebhookFailed(id int64, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		UPDATE webhook_inbox SET status = 'failed', last_error = ?
		WHERE id = ?`, errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("store: mark webhook failed: %w", err)
	}
	return nil
}

// CleanupDoneWebhooks deletes "done" webhooks older than the given number of days.
// Returns the number of rows deleted.
func (s *Store) CleanupDoneWebhooks(olderThanDays int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-time.Duration(olderThanDays) * 24 * time.Hour).Unix()
	res, err := s.db.Exec(`
		DELETE FROM webhook_inbox
		WHERE status IN ('done', 'failed') AND received_at < ?`, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("store: cleanup done webhooks: %w", err)
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Audit Log
// ---------------------------------------------------------------------------

// InsertAudit appends an entry to the audit log.
func (s *Store) InsertAudit(a *AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		INSERT INTO audit_log
			(ts, source, event_type, channel_id, thread_ts, routed_by,
			 routing_confidence, payload_summary_json, payload_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.Source, a.EventType, a.ChannelID, a.ThreadTS,
		a.RoutedBy, a.RoutingConfidence, a.PayloadSummaryJSON, a.PayloadHash,
	)
	if err != nil {
		return fmt.Errorf("store: insert audit: %w", err)
	}
	id, _ := res.LastInsertId()
	a.ID = id
	return nil
}

// CleanupAuditLog deletes audit entries older than the given number of days.
// Returns the number of rows deleted.
func (s *Store) CleanupAuditLog(olderThanDays int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-time.Duration(olderThanDays) * 24 * time.Hour).Unix()
	res, err := s.db.Exec(`DELETE FROM audit_log WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: cleanup audit log: %w", err)
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

// RunMaintenance executes all periodic cleanup tasks. Intended to be called
// from a nightly goroutine or cron-like scheduler.
func (s *Store) RunMaintenance(cfg MaintenanceConfig) error {
	start := time.Now()
	slog.Info("store: maintenance started")

	var errs []error

	if n, err := s.ExpireSessions(); err != nil {
		errs = append(errs, fmt.Errorf("expire sessions: %w", err))
	} else if n > 0 {
		slog.Info("store: expired sessions", "count", n)
	}

	if n, err := s.DeleteOrphanedTurns(); err != nil {
		errs = append(errs, fmt.Errorf("delete orphaned turns: %w", err))
	} else if n > 0 {
		slog.Info("store: deleted orphaned turns", "count", n)
	}

	if n, err := s.ExpireCorrelations(); err != nil {
		errs = append(errs, fmt.Errorf("expire correlations: %w", err))
	} else if n > 0 {
		slog.Info("store: expired correlations", "count", n)
	}

	if cfg.MaxCorrelationRows > 0 {
		if n, err := s.CapCorrelations(cfg.MaxCorrelationRows); err != nil {
			errs = append(errs, fmt.Errorf("cap correlations: %w", err))
		} else if n > 0 {
			slog.Info("store: capped correlations", "deleted", n, "max", cfg.MaxCorrelationRows)
		}
	}

	if cfg.DoneWebhookRetentionDays > 0 {
		if n, err := s.CleanupDoneWebhooks(cfg.DoneWebhookRetentionDays); err != nil {
			errs = append(errs, fmt.Errorf("cleanup done webhooks: %w", err))
		} else if n > 0 {
			slog.Info("store: cleaned up webhooks", "count", n, "retention_days", cfg.DoneWebhookRetentionDays)
		}
	}

	if cfg.AuditRetentionDays > 0 {
		if n, err := s.CleanupAuditLog(cfg.AuditRetentionDays); err != nil {
			errs = append(errs, fmt.Errorf("cleanup audit log: %w", err))
		} else if n > 0 {
			slog.Info("store: cleaned up audit log", "count", n, "retention_days", cfg.AuditRetentionDays)
		}
	}

	elapsed := time.Since(start)
	if len(errs) > 0 {
		slog.Error("store: maintenance completed with errors", "elapsed", elapsed, "errors", len(errs))
		return errors.Join(errs...)
	}

	slog.Info("store: maintenance completed", "elapsed", elapsed)
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// scanSession scans a single session row from a *sql.Row.
func (s *Store) scanSession(row *sql.Row) (*Session, error) {
	sess := &Session{}
	var friendlyName sql.NullString
	err := row.Scan(
		&sess.ChannelID, &sess.ThreadTS, &sess.JcodeSession, &friendlyName,
		&sess.Workdir, &sess.CreatedAt, &sess.LastActivity, &sess.Status, &sess.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan session: %w", err)
	}
	if friendlyName.Valid {
		sess.FriendlyName = friendlyName.String
	}
	return sess, nil
}

// scanSessionRow scans a session from *sql.Rows (use inside a rows.Next() loop).
func (s *Store) scanSessionRow(rows *sql.Rows) (*Session, error) {
	sess := &Session{}
	var friendlyName sql.NullString
	err := rows.Scan(
		&sess.ChannelID, &sess.ThreadTS, &sess.JcodeSession, &friendlyName,
		&sess.Workdir, &sess.CreatedAt, &sess.LastActivity, &sess.Status, &sess.ExpiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: scan session row: %w", err)
	}
	if friendlyName.Valid {
		sess.FriendlyName = friendlyName.String
	}
	return sess, nil
}

// scanCorrelations scans multiple ThreadCorrelation rows.
func scanCorrelations(rows *sql.Rows) ([]*ThreadCorrelation, error) {
	var out []*ThreadCorrelation
	for rows.Next() {
		c := &ThreadCorrelation{}
		if err := rows.Scan(&c.Source, &c.ExternalKey, &c.ChannelID,
			&c.ThreadTS, &c.CreatedAt, &c.ExpiresAt, &c.CreatedBy); err != nil {
			return nil, fmt.Errorf("store: scan correlation: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// expectOneRow returns an error if an UPDATE/DELETE affected zero rows.
func expectOneRow(res sql.Result, entity, key1, key2 string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store: %s not found (%s, %s)", entity, key1, key2)
	}
	return nil
}

// nilIfEmpty returns nil for empty strings so they map to SQL NULL.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// LLM Routing Decisions
// ---------------------------------------------------------------------------

// InsertLLMDecision records an LLM routing decision.
func (s *Store) InsertLLMDecision(d *LLMRoutingDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		INSERT INTO llm_routing_decisions
			(webhook_inbox_id, decided_at, model, thread_id, confidence,
			 reasoning, posted_to, user_feedback, feedback_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.WebhookInboxID, d.DecidedAt, d.Model, d.ThreadID,
		d.Confidence, d.Reasoning, d.PostedTo, d.UserFeedback, d.FeedbackAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert llm decision: %w", err)
	}
	id, _ := res.LastInsertId()
	d.ID = id
	return nil
}

// UpdateLLMFeedback records user feedback on an LLM routing decision.
// feedback must be "confirmed" or "rejected".
func (s *Store) UpdateLLMFeedback(id int64, feedback string) error {
	if feedback != "confirmed" && feedback != "rejected" {
		return fmt.Errorf("store: invalid feedback value %q: must be \"confirmed\" or \"rejected\"", feedback)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	res, err := s.db.Exec(`
		UPDATE llm_routing_decisions
		SET user_feedback = ?, feedback_at = ?
		WHERE id = ?`,
		feedback, now, id,
	)
	if err != nil {
		return fmt.Errorf("store: update llm feedback: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update llm feedback rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store: llm routing decision not found (id %d)", id)
	}
	return nil
}

// GetLLMDecisionByWebhookID retrieves the routing decision for a webhook.
func (s *Store) GetLLMDecisionByWebhookID(webhookInboxID int64) (*LLMRoutingDecision, error) {
	row := s.db.QueryRow(`
		SELECT id, webhook_inbox_id, decided_at, model, thread_id,
		       confidence, reasoning, posted_to, user_feedback, feedback_at
		FROM llm_routing_decisions
		WHERE webhook_inbox_id = ?
		ORDER BY decided_at DESC
		LIMIT 1`, webhookInboxID)

	d := &LLMRoutingDecision{}
	err := row.Scan(&d.ID, &d.WebhookInboxID, &d.DecidedAt, &d.Model,
		&d.ThreadID, &d.Confidence, &d.Reasoning, &d.PostedTo,
		&d.UserFeedback, &d.FeedbackAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get llm decision: %w", err)
	}
	return d, nil
}

// LLMRoutingStats returns accuracy statistics for the LLM router.
type LLMRoutingStats struct {
	Total     int
	Confirmed int
	Rejected  int
	Pending   int
}

// GetLLMRoutingStats returns feedback statistics for the last N decisions.
func (s *Store) GetLLMRoutingStats(limit int) (*LLMRoutingStats, error) {
	rows, err := s.db.Query(`
		SELECT user_feedback FROM llm_routing_decisions
		ORDER BY decided_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: get llm stats: %w", err)
	}
	defer rows.Close()

	stats := &LLMRoutingStats{}
	for rows.Next() {
		var feedback *string
		if err := rows.Scan(&feedback); err != nil {
			return nil, fmt.Errorf("store: scan llm stats: %w", err)
		}
		stats.Total++
		if feedback == nil {
			stats.Pending++
		} else if *feedback == "confirmed" {
			stats.Confirmed++
		} else if *feedback == "rejected" {
			stats.Rejected++
		}
	}
	return stats, rows.Err()
}
