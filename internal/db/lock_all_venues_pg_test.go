package db

import (
	"context"
	"os"
	"testing"
)

// Every venue locks by default (24 Nov 2026): existing venues get a lock rule once (AIO Lab aside),
// and a venue added later gets one from a trigger.
func TestVenueLockedByDefault(t *testing.T) {
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
	var n int
	if err := d.pool.QueryRow(ctx, `INSERT INTO restaurants (name) VALUES ('Lock Test Venue') RETURNING 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	var rules int
	if err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM kiosk_policies k JOIN restaurants r ON r.id = k.target_id WHERE k.target_type = 'restaurant' AND r.name = 'Lock Test Venue' AND k.kiosk_package = ''`).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if rules != 1 {
		t.Fatalf("a new venue should get one lock rule, got %d", rules)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO restaurants (name) VALUES ('AIO Lab')`); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM kiosk_policies k JOIN restaurants r ON r.id = k.target_id WHERE r.name = 'AIO Lab'`).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if rules != 0 {
		t.Fatalf("AIO Lab must not be locked by default, got %d rules", rules)
	}
}
