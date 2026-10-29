package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLastSeenBatchAgainstPostgres: queued stamps land in one write, never backwards.
func TestLastSeenBatchAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	b := 50
	stamp := time.Now().Format("150405.000")
	a, _, _, _, _, err := d.UpsertCheckin(ctx, "LSA"+stamp, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	c, _, _, _, _, err := d.UpsertCheckin(ctx, "LSB"+stamp, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	d.StartLastSeenBatcher(ctx)
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	past := time.Now().Add(-24 * time.Hour)
	_ = d.QueueLastSeen(ctx, a, future)
	_ = d.QueueLastSeen(ctx, c, past) // older than its check-in: must not win
	time.Sleep(3 * lastSeenFlush)
	var gotA, gotC time.Time
	if err := d.pool.QueryRow(ctx, `SELECT last_seen_at FROM devices WHERE id = $1`, a).Scan(&gotA); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `SELECT last_seen_at FROM devices WHERE id = $1`, c).Scan(&gotC); err != nil {
		t.Fatal(err)
	}
	if !gotA.Equal(future) {
		t.Errorf("A last_seen = %s, want %s", gotA, future)
	}
	if gotC.Before(time.Now().Add(-time.Hour)) {
		t.Errorf("C moved backwards to %s", gotC)
	}
}
