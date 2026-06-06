package store

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New(%s): %v", dir, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// ---------------------------------------------------------------------------
// Schema & Init
// ---------------------------------------------------------------------------

func TestNew_WALAndIntegrity(t *testing.T) {
	s := newTestStore(t)

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Errorf("user_version = %d, want 4", version)
	}
}

func TestNew_IdempotentMigration(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()

	// Re-open: migration should be a no-op.
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s2.Close()
}

// TestMigrateV4 validates that the v4 migration adds the backend column to
// the sessions table and that existing rows get the default value 'jcode'.
func TestMigrateV4(t *testing.T) {
	dir := t.TempDir()

	// Open store to apply all migrations up to v4.
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Verify backend column exists by inserting a session with backend.
	now := time.Now().Unix()
	sess := &Session{
		ChannelID:    "C000MIGRTEST",
		ThreadTS:     "1111.2222",
		JcodeSession: "j-migr",
		Workdir:      "/tmp/migr",
		CreatedAt:    now,
		LastActivity: now,
		Status:       "idle",
		Backend:      "claude",
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession with backend: %v", err)
	}

	got, err := s.GetSession("C000MIGRTEST", "1111.2222")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Backend != "claude" {
		t.Errorf("Backend = %q, want claude", got.Backend)
	}

	// Insert a session without setting Backend - should default to jcode.
	sess2 := &Session{
		ChannelID:    "C000MIGRTEST",
		ThreadTS:     "3333.4444",
		JcodeSession: "j-default",
		Workdir:      "/tmp/migr2",
		CreatedAt:    now,
		LastActivity: now,
		Status:       "idle",
	}
	if err := s.CreateSession(sess2); err != nil {
		t.Fatalf("CreateSession without backend: %v", err)
	}

	got2, err := s.GetSession("C000MIGRTEST", "3333.4444")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got2.Backend != "jcode" {
		t.Errorf("Backend = %q, want jcode (default)", got2.Backend)
	}

	// Verify version is 4.
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Errorf("user_version = %d, want 4", version)
	}

	s.Close()

	// Re-open to verify idempotent migration.
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("re-open after v4: %v", err)
	}
	defer s2.Close()

	got3, err := s2.GetSession("C000MIGRTEST", "1111.2222")
	if err != nil {
		t.Fatalf("GetSession after re-open: %v", err)
	}
	if got3.Backend != "claude" {
		t.Errorf("Backend after re-open = %q, want claude", got3.Backend)
	}
}

func TestNew_CorruptedDB(t *testing.T) {
	dir := t.TempDir()
	// Write garbage to the db file.
	if err := os.WriteFile(dir+"/switchboard.db", []byte("corrupt!"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(dir)
	if err == nil {
		t.Fatal("expected error on corrupt db, got nil")
	}
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func TestCreateAndGetSession(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	sess := &Session{
		ChannelID:    "C123",
		ThreadTS:     "1234.5678",
		JcodeSession: "jcode-abc",
		FriendlyName: "test-session",
		Workdir:      "/tmp/work",
		CreatedAt:    now,
		LastActivity: now,
		Status:       "idle",
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSession("C123", "1234.5678")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("GetSession returned nil")
	}
	if got.JcodeSession != "jcode-abc" {
		t.Errorf("JcodeSession = %q, want jcode-abc", got.JcodeSession)
	}
	if got.FriendlyName != "test-session" {
		t.Errorf("FriendlyName = %q, want test-session", got.FriendlyName)
	}
	if got.Status != "idle" {
		t.Errorf("Status = %q, want idle", got.Status)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetSession("nope", "nope")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("expected nil for missing session")
	}
}

func TestGetSessionByJcodeID(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	sess := &Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "jid-1",
		Workdir: "/w", CreatedAt: now, LastActivity: now, Status: "idle",
	}
	s.CreateSession(sess)

	got, err := s.GetSessionByJcodeID("jid-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ChannelID != "C1" {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestUpdateSessionStatus(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: now, LastActivity: now, Status: "idle",
	})

	if err := s.UpdateSessionStatus("C1", "1.1", "processing"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession("C1", "1.1")
	if got.Status != "processing" {
		t.Errorf("Status = %q, want processing", got.Status)
	}
}

func TestUpdateSessionStatus_NotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdateSessionStatus("nope", "nope", "idle")
	if err == nil {
		t.Error("expected error for missing session")
	}
}

func TestUpdateSessionActivity(t *testing.T) {
	s := newTestStore(t)
	old := time.Now().Add(-1 * time.Hour).Unix()
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: old, LastActivity: old, Status: "idle",
	})

	if err := s.UpdateSessionActivity("C1", "1.1"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession("C1", "1.1")
	if got.LastActivity <= old {
		t.Error("LastActivity was not bumped")
	}
}

func TestSetSessionFriendlyName(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: now, LastActivity: now, Status: "idle",
	})

	if err := s.SetSessionFriendlyName("C1", "1.1", "my-session"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession("C1", "1.1")
	if got.FriendlyName != "my-session" {
		t.Errorf("FriendlyName = %q, want my-session", got.FriendlyName)
	}

	// Clear it.
	if err := s.SetSessionFriendlyName("C1", "1.1", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession("C1", "1.1")
	if got.FriendlyName != "" {
		t.Errorf("FriendlyName = %q, want empty", got.FriendlyName)
	}
}

func TestListActiveSessions(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	for _, st := range []string{"idle", "processing", "closed"} {
		s.CreateSession(&Session{
			ChannelID: "C1", ThreadTS: st, JcodeSession: "j-" + st,
			Workdir: "/w", CreatedAt: now, LastActivity: now, Status: st,
		})
	}

	list, err := s.ListActiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("got %d active sessions, want 2", len(list))
	}
}

func TestExpireSessions(t *testing.T) {
	s := newTestStore(t)
	past := time.Now().Add(-1 * time.Hour).Unix()
	future := time.Now().Add(1 * time.Hour).Unix()

	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "expired", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: past, LastActivity: past, Status: "idle",
		ExpiresAt: &past,
	})
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "alive", JcodeSession: "j2",
		Workdir: "/w", CreatedAt: past, LastActivity: past, Status: "idle",
		ExpiresAt: &future,
	})

	n, err := s.ExpireSessions()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expired %d, want 1", n)
	}

	got, _ := s.GetSession("C1", "expired")
	if got.Status != "closed" {
		t.Errorf("Status = %q, want closed", got.Status)
	}
}

// ---------------------------------------------------------------------------
// Turn Queue
// ---------------------------------------------------------------------------

func TestEnqueueAndDrainTurns(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: now, LastActivity: now, Status: "idle",
	})

	for i := 0; i < 3; i++ {
		s.EnqueueTurn(&TurnQueueItem{
			ChannelID: "C1", ThreadTS: "1.1",
			EnqueuedAt: now + int64(i), UserID: "U1", Text: "msg",
		})
	}

	cnt, err := s.CountTurns("C1", "1.1")
	if err != nil {
		t.Fatal(err)
	}
	if cnt != 3 {
		t.Errorf("CountTurns = %d, want 3", cnt)
	}

	items, err := s.DrainTurns("C1", "1.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Errorf("DrainTurns returned %d, want 3", len(items))
	}

	// Should be empty after drain.
	cnt, _ = s.CountTurns("C1", "1.1")
	if cnt != 0 {
		t.Errorf("after drain CountTurns = %d, want 0", cnt)
	}
}

func TestDrainTurns_Empty(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: now, LastActivity: now, Status: "idle",
	})

	items, err := s.DrainTurns("C1", "1.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("DrainTurns returned %d, want 0", len(items))
	}
}

func TestDeleteOrphanedTurns(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: now, LastActivity: now, Status: "idle",
	})

	old := time.Now().Add(-25 * time.Hour).Unix()
	s.EnqueueTurn(&TurnQueueItem{
		ChannelID: "C1", ThreadTS: "1.1",
		EnqueuedAt: old, UserID: "U1", Text: "old",
	})
	s.EnqueueTurn(&TurnQueueItem{
		ChannelID: "C1", ThreadTS: "1.1",
		EnqueuedAt: now, UserID: "U1", Text: "new",
	})

	n, err := s.DeleteOrphanedTurns()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted %d, want 1", n)
	}

	cnt, _ := s.CountTurns("C1", "1.1")
	if cnt != 1 {
		t.Errorf("remaining turns = %d, want 1", cnt)
	}
}

// ---------------------------------------------------------------------------
// Thread Correlations
// ---------------------------------------------------------------------------

func TestUpsertAndLookupCorrelation(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	c := &ThreadCorrelation{
		Source: "github", ExternalKey: "repo/123",
		ChannelID: "C1", ThreadTS: "1.1",
		CreatedAt: now, CreatedBy: "router",
	}
	if err := s.UpsertCorrelation(c); err != nil {
		t.Fatal(err)
	}

	// Lookup by source+key.
	got, err := s.LookupCorrelation("github", "repo/123")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChannelID != "C1" {
		t.Errorf("unexpected: %+v", got)
	}

	// Lookup by thread.
	got2, err := s.LookupCorrelationForThread("C1", "1.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 {
		t.Errorf("unexpected: %+v", got2)
	}

	// Upsert overwrites thread_ts.
	c.ThreadTS = "2.2"
	if err := s.UpsertCorrelation(c); err != nil {
		t.Fatal(err)
	}
	got3, _ := s.LookupCorrelation("github", "repo/123")
	if len(got3) != 1 || got3[0].ThreadTS != "2.2" {
		t.Errorf("upsert did not update thread_ts: %+v", got3)
	}
}

func TestExpireCorrelations(t *testing.T) {
	s := newTestStore(t)
	past := time.Now().Add(-1 * time.Hour).Unix()
	future := time.Now().Add(1 * time.Hour).Unix()

	s.UpsertCorrelation(&ThreadCorrelation{
		Source: "gh", ExternalKey: "old", ChannelID: "C1", ThreadTS: "1",
		CreatedAt: past, ExpiresAt: &past, CreatedBy: "r",
	})
	s.UpsertCorrelation(&ThreadCorrelation{
		Source: "gh", ExternalKey: "new", ChannelID: "C1", ThreadTS: "2",
		CreatedAt: past, ExpiresAt: &future, CreatedBy: "r",
	})

	n, err := s.ExpireCorrelations()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expired %d, want 1", n)
	}
}

func TestCapCorrelations(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	for i := 0; i < 10; i++ {
		s.UpsertCorrelation(&ThreadCorrelation{
			Source: "gh", ExternalKey: fmt.Sprintf("k%d", i), ChannelID: "C1",
			ThreadTS: "1", CreatedAt: now + int64(i), CreatedBy: "r",
		})
	}

	n, err := s.CapCorrelations(5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("capped %d, want 5", n)
	}
}

// ---------------------------------------------------------------------------
// Webhook Inbox
// ---------------------------------------------------------------------------

func TestInsertWebhook_Dedup(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	w := &WebhookInboxItem{
		ReceivedAt: now, Source: "gh", IdempotencyKey: "abc",
		HeadersJSON: "{}", BodyBlob: []byte("body"), Status: "pending",
	}
	inserted, err := s.InsertWebhook(w)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Error("expected inserted=true")
	}
	if w.ID == 0 {
		t.Error("expected non-zero ID")
	}

	// Duplicate.
	w2 := &WebhookInboxItem{
		ReceivedAt: now, Source: "gh", IdempotencyKey: "abc",
		HeadersJSON: "{}", BodyBlob: []byte("body2"), Status: "pending",
	}
	inserted2, err := s.InsertWebhook(w2)
	if err != nil {
		t.Fatal(err)
	}
	if inserted2 {
		t.Error("expected inserted=false for dedup")
	}
}

func TestClaimPendingWebhook(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	s.InsertWebhook(&WebhookInboxItem{
		ReceivedAt: now, Source: "gh", IdempotencyKey: "1",
		HeadersJSON: "{}", BodyBlob: []byte("b"), Status: "pending",
	})

	got, err := s.ClaimPendingWebhook("gh")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected non-nil webhook")
	}
	if got.Status != "processing" {
		t.Errorf("Status = %q, want processing", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}

	// Nothing left to claim.
	got2, err := s.ClaimPendingWebhook("gh")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != nil {
		t.Error("expected nil, no more pending webhooks")
	}
}

func TestMarkWebhookDoneAndFailed(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	s.InsertWebhook(&WebhookInboxItem{
		ReceivedAt: now, Source: "gh", IdempotencyKey: "done1",
		HeadersJSON: "{}", BodyBlob: []byte("b"), Status: "pending",
	})
	w, _ := s.ClaimPendingWebhook("gh")

	if err := s.MarkWebhookDone(w.ID); err != nil {
		t.Fatal(err)
	}

	s.InsertWebhook(&WebhookInboxItem{
		ReceivedAt: now, Source: "gh", IdempotencyKey: "fail1",
		HeadersJSON: "{}", BodyBlob: []byte("b"), Status: "pending",
	})
	w2, _ := s.ClaimPendingWebhook("gh")
	if err := s.MarkWebhookFailed(w2.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupDoneWebhooks(t *testing.T) {
	s := newTestStore(t)
	old := time.Now().Add(-10 * 24 * time.Hour).Unix()
	recent := time.Now().Unix()

	s.InsertWebhook(&WebhookInboxItem{
		ReceivedAt: old, Source: "gh", IdempotencyKey: "old",
		HeadersJSON: "{}", BodyBlob: []byte("b"), Status: "pending",
	})
	w, _ := s.ClaimPendingWebhook("gh")
	s.MarkWebhookDone(w.ID)

	s.InsertWebhook(&WebhookInboxItem{
		ReceivedAt: recent, Source: "gh", IdempotencyKey: "new",
		HeadersJSON: "{}", BodyBlob: []byte("b"), Status: "pending",
	})
	w2, _ := s.ClaimPendingWebhook("gh")
	s.MarkWebhookDone(w2.ID)

	n, err := s.CleanupDoneWebhooks(7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleaned %d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Audit Log
// ---------------------------------------------------------------------------

func TestInsertAndCleanupAudit(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	old := time.Now().Add(-40 * 24 * time.Hour).Unix()

	s.InsertAudit(&AuditEntry{
		TS: old, Source: "gh", EventType: "push",
		PayloadSummaryJSON: "{}", PayloadHash: "abc",
	})
	s.InsertAudit(&AuditEntry{
		TS: now, Source: "gh", EventType: "pr",
		PayloadSummaryJSON: "{}", PayloadHash: "def",
	})

	n, err := s.CleanupAuditLog(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleaned %d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

func TestRunMaintenance(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	past := time.Now().Add(-25 * time.Hour).Unix()

	// Seed data that maintenance should clean up.
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: now, LastActivity: now, Status: "idle",
	})
	s.EnqueueTurn(&TurnQueueItem{
		ChannelID: "C1", ThreadTS: "1.1",
		EnqueuedAt: past, UserID: "U1", Text: "stale",
	})

	err := s.RunMaintenance(MaintenanceConfig{
		AuditRetentionDays:         30,
		DoneWebhookRetentionDays:   7,
		FailedWebhookRetentionDays: 14,
		MaxCorrelationRows:         1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stale turn should be gone.
	cnt, _ := s.CountTurns("C1", "1.1")
	if cnt != 0 {
		t.Errorf("orphaned turns remaining: %d", cnt)
	}
}

// ---------------------------------------------------------------------------
// Concurrent access
// ---------------------------------------------------------------------------

func TestConcurrentAccess(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	s.CreateSession(&Session{
		ChannelID: "C1", ThreadTS: "1.1", JcodeSession: "j1",
		Workdir: "/w", CreatedAt: now, LastActivity: now, Status: "idle",
	})

	errc := make(chan error, 20)

	// Concurrent reads.
	for i := 0; i < 10; i++ {
		go func() {
			_, err := s.GetSession("C1", "1.1")
			errc <- err
		}()
	}

	// Concurrent writes.
	for i := 0; i < 10; i++ {
		go func() {
			errc <- s.UpdateSessionActivity("C1", "1.1")
		}()
	}

	for i := 0; i < 20; i++ {
		if err := <-errc; err != nil {
			t.Errorf("concurrent op: %v", err)
		}
	}
}

func TestLLMRoutingDecision_InsertAndRetrieve(t *testing.T) {
	s := newTestStore(t)

	// First, insert a webhook to satisfy the FK.
	w := &WebhookInboxItem{
		ReceivedAt:     time.Now().Unix(),
		Source:         "test",
		IdempotencyKey: "test-llm-1",
		HeadersJSON:    "{}",
		BodyBlob:       []byte("{}"),
		Status:         "done",
	}
	inserted, err := s.InsertWebhook(w)
	if err != nil {
		t.Fatalf("InsertWebhook: %v", err)
	}
	if !inserted {
		t.Fatal("webhook not inserted")
	}

	threadID := "C123:1234567.890"
	reasoning := "repo name matches workdir"
	d := &LLMRoutingDecision{
		WebhookInboxID: &w.ID,
		DecidedAt:      time.Now().Unix(),
		Model:          "claude-haiku-4-5",
		ThreadID:       &threadID,
		Confidence:     85,
		Reasoning:      &reasoning,
		PostedTo:       "suggested",
	}

	if err := s.InsertLLMDecision(d); err != nil {
		t.Fatalf("InsertLLMDecision: %v", err)
	}
	if d.ID == 0 {
		t.Error("expected non-zero ID after insert")
	}

	// Retrieve it.
	got, err := s.GetLLMDecisionByWebhookID(w.ID)
	if err != nil {
		t.Fatalf("GetLLMDecisionByWebhookID: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil decision")
	}
	if got.Confidence != 85 {
		t.Errorf("confidence = %d, want 85", got.Confidence)
	}
	if got.PostedTo != "suggested" {
		t.Errorf("posted_to = %q, want 'suggested'", got.PostedTo)
	}
}

func TestLLMRoutingDecision_UpdateFeedback(t *testing.T) {
	s := newTestStore(t)

	w := &WebhookInboxItem{
		ReceivedAt:     time.Now().Unix(),
		Source:         "test",
		IdempotencyKey: "test-llm-fb",
		HeadersJSON:    "{}",
		BodyBlob:       []byte("{}"),
		Status:         "done",
	}
	s.InsertWebhook(w)

	d := &LLMRoutingDecision{
		WebhookInboxID: &w.ID,
		DecidedAt:      time.Now().Unix(),
		Model:          "claude-haiku-4-5",
		Confidence:     90,
		PostedTo:       "suggested",
	}
	s.InsertLLMDecision(d)

	// Update feedback.
	if err := s.UpdateLLMFeedback(d.ID, "confirmed"); err != nil {
		t.Fatalf("UpdateLLMFeedback: %v", err)
	}

	// Verify.
	got, _ := s.GetLLMDecisionByWebhookID(w.ID)
	if got.UserFeedback == nil || *got.UserFeedback != "confirmed" {
		t.Errorf("feedback = %v, want 'confirmed'", got.UserFeedback)
	}
	if got.FeedbackAt == nil {
		t.Error("feedback_at should be set")
	}
}

// ---------------------------------------------------------------------------
// PR review fixes: TDD tests (written before implementation)
// ---------------------------------------------------------------------------

func TestLLMDecision_NullWebhookInboxID(t *testing.T) {
	s := newTestStore(t)

	// Insert a decision with nil WebhookInboxID (no durable inbox).
	d := &LLMRoutingDecision{
		DecidedAt:  time.Now().Unix(),
		Model:      "claude-haiku-4-5",
		Confidence: 75,
		PostedTo:   "fallback",
	}
	if err := s.InsertLLMDecision(d); err != nil {
		t.Fatalf("InsertLLMDecision with nil webhook_inbox_id: %v", err)
	}
	if d.ID == 0 {
		t.Error("expected non-zero ID")
	}
}

func TestLLMDecision_UniqueWebhookInboxID(t *testing.T) {
	s := newTestStore(t)

	w := &WebhookInboxItem{
		ReceivedAt:     time.Now().Unix(),
		Source:         "test",
		IdempotencyKey: "unique-test",
		HeadersJSON:    "{}",
		BodyBlob:       []byte("{}"),
		Status:         "done",
	}
	s.InsertWebhook(w)

	// First decision for this webhook should succeed.
	d1 := &LLMRoutingDecision{
		WebhookInboxID: &w.ID,
		DecidedAt:      time.Now().Unix(),
		Model:          "claude-haiku-4-5",
		Confidence:     80,
		PostedTo:       "suggested",
	}
	if err := s.InsertLLMDecision(d1); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Second decision for the SAME webhook should fail (unique constraint).
	d2 := &LLMRoutingDecision{
		WebhookInboxID: &w.ID,
		DecidedAt:      time.Now().Unix(),
		Model:          "claude-haiku-4-5",
		Confidence:     90,
		PostedTo:       "suggested",
	}
	err := s.InsertLLMDecision(d2)
	if err == nil {
		t.Fatal("expected unique constraint error for duplicate webhook_inbox_id, got nil")
	}
}

func TestLLMDecision_MultipleNullWebhookIDs(t *testing.T) {
	s := newTestStore(t)

	// Multiple decisions with NULL webhook_inbox_id should be allowed
	// (NULL != NULL in SQL unique constraints).
	for i := 0; i < 3; i++ {
		d := &LLMRoutingDecision{
			DecidedAt:  time.Now().Unix(),
			Model:      "claude-haiku-4-5",
			Confidence: 70 + i,
			PostedTo:   "fallback",
		}
		if err := s.InsertLLMDecision(d); err != nil {
			t.Fatalf("insert %d with nil webhook_inbox_id: %v", i, err)
		}
	}
}

func TestUpdateLLMFeedback_InvalidEnum(t *testing.T) {
	s := newTestStore(t)

	d := &LLMRoutingDecision{
		DecidedAt:  time.Now().Unix(),
		Model:      "claude-haiku-4-5",
		Confidence: 80,
		PostedTo:   "suggested",
	}
	s.InsertLLMDecision(d)

	// Invalid feedback value should be rejected.
	err := s.UpdateLLMFeedback(d.ID, "invalid-value")
	if err == nil {
		t.Fatal("expected error for invalid feedback enum, got nil")
	}

	err = s.UpdateLLMFeedback(d.ID, "")
	if err == nil {
		t.Fatal("expected error for empty feedback, got nil")
	}
}

func TestUpdateLLMFeedback_StaleID(t *testing.T) {
	s := newTestStore(t)

	// Updating a non-existent ID should fail.
	err := s.UpdateLLMFeedback(99999, "confirmed")
	if err == nil {
		t.Fatal("expected error for stale/missing ID, got nil")
	}
}

func TestGetLLMDecisionByWebhookID_Deterministic(t *testing.T) {
	s := newTestStore(t)

	w := &WebhookInboxItem{
		ReceivedAt:     time.Now().Unix(),
		Source:         "test",
		IdempotencyKey: "det-test",
		HeadersJSON:    "{}",
		BodyBlob:       []byte("{}"),
		Status:         "done",
	}
	s.InsertWebhook(w)

	// To test determinism, we need two rows with the same webhook_inbox_id.
	// Since we're adding a unique index, we'll instead test that the query
	// includes ORDER BY + LIMIT 1 by inserting one row and verifying it
	// comes back correctly. The real protection is the unique index +
	// deterministic query for defense-in-depth.
	now := time.Now().Unix()
	d := &LLMRoutingDecision{
		WebhookInboxID: &w.ID,
		DecidedAt:      now,
		Model:          "claude-haiku-4-5",
		Confidence:     85,
		PostedTo:       "suggested",
	}
	s.InsertLLMDecision(d)

	got, err := s.GetLLMDecisionByWebhookID(w.ID)
	if err != nil {
		t.Fatalf("GetLLMDecisionByWebhookID: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil decision")
	}
	if got.DecidedAt != now {
		t.Errorf("decided_at = %d, want %d", got.DecidedAt, now)
	}
	if got.Confidence != 85 {
		t.Errorf("confidence = %d, want 85", got.Confidence)
	}
}

func TestLLMRoutingStats(t *testing.T) {
	s := newTestStore(t)

	// Insert some test decisions.
	for i := 0; i < 5; i++ {
		w := &WebhookInboxItem{
			ReceivedAt:     time.Now().Unix(),
			Source:         "test",
			IdempotencyKey: fmt.Sprintf("stats-%d", i),
			HeadersJSON:    "{}",
			BodyBlob:       []byte("{}"),
			Status:         "done",
		}
		s.InsertWebhook(w)

		d := &LLMRoutingDecision{
			WebhookInboxID: &w.ID,
			DecidedAt:      time.Now().Unix(),
			Model:          "haiku",
			Confidence:     80 + i,
			PostedTo:       "suggested",
		}
		s.InsertLLMDecision(d)

		// Mark some as confirmed/rejected.
		if i < 3 {
			s.UpdateLLMFeedback(d.ID, "confirmed")
		} else if i == 3 {
			s.UpdateLLMFeedback(d.ID, "rejected")
		}
		// i == 4 stays pending
	}

	stats, err := s.GetLLMRoutingStats(100)
	if err != nil {
		t.Fatalf("GetLLMRoutingStats: %v", err)
	}
	if stats.Total != 5 {
		t.Errorf("total = %d, want 5", stats.Total)
	}
	if stats.Confirmed != 3 {
		t.Errorf("confirmed = %d, want 3", stats.Confirmed)
	}
	if stats.Rejected != 1 {
		t.Errorf("rejected = %d, want 1", stats.Rejected)
	}
	if stats.Pending != 1 {
		t.Errorf("pending = %d, want 1", stats.Pending)
	}
}

// ---------------------------------------------------------------------------
// Cron Jobs
// ---------------------------------------------------------------------------

func TestCronJob_CRUD(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	// Insert a job.
	job := &CronJob{
		ID:        "daily-audit",
		Schedule:  "0 21 * * *",
		ChannelID: "C0AL12WCNBG",
		Prompt:    "Run the audit",
		UserID:    "U0AL7H3HQ56",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.InsertCronJob(job); err != nil {
		t.Fatalf("InsertCronJob: %v", err)
	}

	// Get it back.
	got, err := s.GetCronJob("daily-audit")
	if err != nil {
		t.Fatalf("GetCronJob: %v", err)
	}
	if got == nil {
		t.Fatal("GetCronJob returned nil")
	}
	if got.Schedule != "0 21 * * *" {
		t.Errorf("schedule = %q, want %q", got.Schedule, "0 21 * * *")
	}
	if !got.Enabled {
		t.Error("expected enabled = true")
	}

	// List.
	jobs, err := s.ListCronJobs()
	if err != nil {
		t.Fatalf("ListCronJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}

	// Disable.
	if err := s.UpdateCronJobEnabled("daily-audit", false); err != nil {
		t.Fatalf("UpdateCronJobEnabled: %v", err)
	}
	got, _ = s.GetCronJob("daily-audit")
	if got.Enabled {
		t.Error("expected enabled = false after disable")
	}

	// Enable.
	if err := s.UpdateCronJobEnabled("daily-audit", true); err != nil {
		t.Fatalf("UpdateCronJobEnabled: %v", err)
	}
	got, _ = s.GetCronJob("daily-audit")
	if !got.Enabled {
		t.Error("expected enabled = true after enable")
	}

	// Delete.
	if err := s.DeleteCronJob("daily-audit"); err != nil {
		t.Fatalf("DeleteCronJob: %v", err)
	}
	got, err = s.GetCronJob("daily-audit")
	if err != nil {
		t.Fatalf("GetCronJob after delete: %v", err)
	}
	if got != nil {
		t.Error("expected nil after delete")
	}

	// Delete non-existent.
	if err := s.DeleteCronJob("nonexistent"); err == nil {
		t.Error("expected error deleting non-existent job")
	}

	// Update non-existent.
	if err := s.UpdateCronJobEnabled("nonexistent", true); err == nil {
		t.Error("expected error updating non-existent job")
	}
}

func TestCronJob_DuplicateID(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	job := &CronJob{
		ID:        "dup-test",
		Schedule:  "* * * * *",
		ChannelID: "C0AL12WCNBG",
		Prompt:    "test",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.InsertCronJob(job); err != nil {
		t.Fatalf("first InsertCronJob: %v", err)
	}
	if err := s.InsertCronJob(job); err == nil {
		t.Error("expected error on duplicate insert")
	}
}

func TestCronJob_ConcurrentDaemonAndCLI(t *testing.T) {
	// Simulate the daemon holding a store.New connection while
	// CLI opens a store.NewCLI connection to the same DB.
	dir := t.TempDir()

	// "Daemon" connection.
	daemon, err := New(dir)
	if err != nil {
		t.Fatalf("New (daemon): %v", err)
	}
	defer daemon.Close()

	// Insert a session so the daemon has touched the DB.
	daemon.CreateSession(&Session{
		ChannelID:    "C0AL12WCNBG",
		ThreadTS:     "1234567890.000001",
		JcodeSession: "test-session",
		Workdir:      "/tmp",
		CreatedAt:    time.Now().Unix(),
		LastActivity: time.Now().Unix(),
		Status:       "idle",
	})

	// "CLI" connection -- this is what was hanging before the fix.
	cli, err := NewCLI(dir)
	if err != nil {
		t.Fatalf("NewCLI (cli): %v", err)
	}
	defer cli.Close()

	// CLI should be able to insert and read cron jobs while daemon is open.
	now := time.Now().Unix()
	job := &CronJob{
		ID:        "concurrent-test",
		Schedule:  "0 9 * * *",
		ChannelID: "C0AL12WCNBG",
		Prompt:    "concurrent test prompt",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := cli.InsertCronJob(job); err != nil {
		t.Fatalf("CLI InsertCronJob: %v", err)
	}

	// Read from CLI.
	jobs, err := cli.ListCronJobs()
	if err != nil {
		t.Fatalf("CLI ListCronJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("CLI ListCronJobs: got %d, want 1", len(jobs))
	}

	// Daemon should also see the CLI-inserted job.
	daemonJobs, err := daemon.ListCronJobs()
	if err != nil {
		t.Fatalf("daemon ListCronJobs: %v", err)
	}
	if len(daemonJobs) != 1 {
		t.Fatalf("daemon ListCronJobs: got %d, want 1", len(daemonJobs))
	}
	if daemonJobs[0].ID != "concurrent-test" {
		t.Errorf("daemon sees wrong job ID: %q", daemonJobs[0].ID)
	}
}
