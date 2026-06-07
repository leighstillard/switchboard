package store

import (
	"sync"
	"testing"
)

// TestForeignKeysEnforcedAcrossPool verifies FK enforcement is active on every
// pooled connection (not just the one that ran an init pragma). Concurrent
// FK-violating inserts must ALL fail.
func TestForeignKeysEnforcedAcrossPool(t *testing.T) {
	s, err := open(t.TempDir(), false) // daemon mode → pool of 4 connections
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.db.Close()

	const n = 32
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// turn_queue FKs sessions(channel_id, thread_ts); this parent does
			// not exist, so the insert must violate the FK.
			_, errs[i] = s.db.Exec(
				`INSERT INTO turn_queue (channel_id, thread_ts, enqueued_at, user_id, text) VALUES (?,?,?,?,?)`,
				"NO_SUCH_CHANNEL", "0.0", 1, "U1", "hi")
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e == nil {
			t.Fatalf("insert %d succeeded — foreign_keys not enforced on a pooled connection", i)
		}
	}
}
