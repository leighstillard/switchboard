package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/format5/switchboard/internal/store"
)

// ---------------------------------------------------------------------------
// Dispatcher interface
// ---------------------------------------------------------------------------

// Dispatcher is the interface the scheduler uses to dispatch prompts.
// In production this is fulfilled by the router (via a thin adapter).
type Dispatcher interface {
	Dispatch(ctx context.Context, req DispatchRequest) (*DispatchResult, error)
}

// DispatchRequest describes a prompt dispatch to a Slack channel.
type DispatchRequest struct {
	ChannelID string
	Prompt    string
	UserID    string
}

// DispatchResult contains the outcome of a dispatch.
type DispatchResult struct {
	ThreadTS  string
	SessionID string
}

// ---------------------------------------------------------------------------
// Job
// ---------------------------------------------------------------------------

// Job is a single cron job definition from config.
type Job struct {
	ID        string
	Schedule  string
	ChannelID string
	Prompt    string
	UserID    string
	Enabled   bool
}

// parsedJob is a Job with its pre-parsed cron expression.
type parsedJob struct {
	Job
	expr *Expr
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

// Scheduler runs cron jobs on their schedules, dispatching prompts via the
// Dispatcher interface. It ticks every 30 seconds and deduplicates firings
// so each job fires at most once per minute.
type Scheduler struct {
	dispatcher Dispatcher
	store      *store.Store

	mu   sync.RWMutex
	jobs []parsedJob

	// lastFired tracks the last minute each job fired at, keyed by job ID.
	// This prevents duplicate firings for the same minute boundary.
	lastFired map[string]time.Time
}

// New creates a Scheduler from the given jobs. Jobs with invalid schedules
// cause an error; disabled jobs are accepted but skipped at runtime.
func New(jobs []Job, dispatcher Dispatcher, st *store.Store) (*Scheduler, error) {
	s := &Scheduler{
		dispatcher: dispatcher,
		store:      st,
		lastFired:  make(map[string]time.Time),
	}

	parsed, err := parseJobs(jobs)
	if err != nil {
		return nil, err
	}
	s.jobs = parsed

	slog.Info("cron: scheduler created", "jobs", len(parsed))
	return s, nil
}

// Run starts the scheduler tick loop. It blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	slog.Info("cron: scheduler started")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Check immediately on start.
	s.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("cron: scheduler stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// Reload replaces the set of jobs. Invalid schedules are logged and rejected
// (the old job set is retained on error).
func (s *Scheduler) Reload(jobs []Job) error {
	parsed, err := parseJobs(jobs)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.jobs = parsed
	s.mu.Unlock()

	slog.Info("cron: jobs reloaded", "jobs", len(parsed))
	return nil
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

// tick checks all jobs against the current minute and dispatches matches.
func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC().Truncate(time.Minute)

	s.mu.RLock()
	jobs := s.jobs
	s.mu.RUnlock()

	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		if !j.expr.Matches(now) {
			continue
		}

		// Dedup: skip if already fired this minute.
		s.mu.RLock()
		last, seen := s.lastFired[j.ID]
		s.mu.RUnlock()
		if seen && last.Equal(now) {
			continue
		}

		// Mark as fired before dispatching to avoid racing ticks.
		s.mu.Lock()
		s.lastFired[j.ID] = now
		s.mu.Unlock()

		slog.Info("cron: firing job",
			"job_id", j.ID,
			"channel", j.ChannelID,
			"schedule", j.Schedule,
		)

		go s.dispatch(ctx, j, now)
	}
}

// dispatch calls the dispatcher and writes an audit entry.
func (s *Scheduler) dispatch(ctx context.Context, j parsedJob, firedAt time.Time) {
	result, err := s.dispatcher.Dispatch(ctx, DispatchRequest{
		ChannelID: j.ChannelID,
		Prompt:    j.Prompt,
		UserID:    j.UserID,
	})
	if err != nil {
		slog.Error("cron: dispatch failed",
			"job_id", j.ID,
			"error", err,
		)
		s.audit(j, firedAt, "", "", err)
		return
	}

	slog.Info("cron: dispatch succeeded",
		"job_id", j.ID,
		"thread_ts", result.ThreadTS,
		"session_id", result.SessionID,
	)
	s.audit(j, firedAt, result.ThreadTS, result.SessionID, nil)
}

// audit writes a cron dispatch audit entry via the store.
func (s *Scheduler) audit(j parsedJob, firedAt time.Time, threadTS, sessionID string, dispatchErr error) {
	source := "cron"
	eventType := "cron_dispatch"

	summaryMap := map[string]string{
		"job_id":     j.ID,
		"channel_id": j.ChannelID,
		"schedule":   j.Schedule,
		"fired_at":   firedAt.Format(time.RFC3339),
	}
	if threadTS != "" {
		summaryMap["thread_ts"] = threadTS
	}
	if sessionID != "" {
		summaryMap["session_id"] = sessionID
	}
	if dispatchErr != nil {
		summaryMap["error"] = dispatchErr.Error()
	}
	summaryJSON, _ := json.Marshal(summaryMap)

	entry := &store.AuditEntry{
		TS:                 firedAt.Unix(),
		Source:             source,
		EventType:          eventType,
		ChannelID:          &j.ChannelID,
		ThreadTS:           nilIfEmpty(threadTS),
		PayloadSummaryJSON: string(summaryJSON),
		PayloadHash:        j.ID,
	}

	if err := s.store.InsertAudit(entry); err != nil {
		slog.Error("cron: audit insert failed", "job_id", j.ID, "error", err)
	}
}

// parseJobs validates and parses all job schedules.
func parseJobs(jobs []Job) ([]parsedJob, error) {
	out := make([]parsedJob, 0, len(jobs))
	for _, j := range jobs {
		expr, err := Parse(j.Schedule)
		if err != nil {
			return nil, fmt.Errorf("cron: job %q: %w", j.ID, err)
		}
		out = append(out, parsedJob{Job: j, expr: expr})
	}
	return out, nil
}

// nilIfEmpty returns nil for empty strings so they map to SQL NULL in audit entries.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
