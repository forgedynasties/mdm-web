package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAssistantRecordsAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM assistant_messages WHERE username LIKE 'astest-%'`)
	conv := uuid.New()
	add := func(user, role, text string) {
		if err := d.AddAssistantMessage(ctx, conv, user, role, text, "", "/devices/X", 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	add("astest-a", "user", "q1")
	add("astest-a", "tool", "{}")
	add("astest-a", "assistant", "a1")
	add("astest-a", "user", "q2")
	add("astest-a", "assistant", "a2")
	add("astest-b", "user", "someone else's question in the same conv id")

	// History: user/assistant lines only, oldest first, only this user's, the last N.
	h, err := d.AssistantHistory(ctx, conv, "astest-a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[0].Content != "a1" || h[1].Content != "q2" || h[2].Content != "a2" {
		t.Fatalf("history = %+v", h)
	}
	// Rate limits count questions only.
	now := time.Now()
	nShort, nDay, err := d.AssistantAskedSince(ctx, "astest-a", now.Add(-5*time.Minute), now.Add(-24*time.Hour))
	if err != nil || nShort != 2 || nDay != 2 {
		t.Fatalf("asked = %d/%d, %v", nShort, nDay, err)
	}
	// Pruning by age.
	if _, err := d.pool.Exec(ctx, `UPDATE assistant_messages SET created_at = NOW() - interval '100 days' WHERE username = 'astest-b'`); err != nil {
		t.Fatal(err)
	}
	if n, err := d.PruneAssistant(ctx, now.Add(-90*24*time.Hour)); err != nil || n < 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
}
