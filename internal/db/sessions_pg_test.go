package db

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSessionIDsStoredHashedAgainstPostgres(t *testing.T) {
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
	raw := "abc123sessionrawhexvalue"
	if err := d.CreateSession(ctx, Session{ID: raw, Username: "sess-t", Role: "viewer", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM sessions WHERE username IN ('sess-t', 'sess-old')`)
	var n int
	d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE id = $1`, raw).Scan(&n)
	if n != 0 {
		t.Fatal("the raw id must not be stored")
	}
	s, err := d.GetSession(ctx, raw)
	if err != nil || s.ID != raw || s.Username != "sess-t" {
		t.Fatalf("GetSession = %+v, %v", s, err)
	}
	// A row written before hashing still works, and is hashed on first use.
	old := "deadbeefoldrow"
	if _, err := d.pool.Exec(ctx, `INSERT INTO sessions (id, username, role, expires_at) VALUES ($1, 'sess-old', 'viewer', NOW() + interval '1 hour')`, old); err != nil {
		t.Fatal(err)
	}
	if s, err := d.GetSession(ctx, old); err != nil || s.Username != "sess-old" {
		t.Fatalf("old row: %+v, %v", s, err)
	}
	d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE id = $1`, old).Scan(&n)
	if n != 0 {
		t.Fatal("an old row must be hashed on first use")
	}
	if err := d.DeleteSession(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetSession(ctx, raw); err == nil {
		t.Fatal("deleted session still found")
	}
}
