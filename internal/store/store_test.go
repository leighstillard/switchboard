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
	if version != 1 {
		t.Errorf("user_version = %d, want 1", version)
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
