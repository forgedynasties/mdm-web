package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVenuesListAgainstPostgres: staff PINs per venue ("" = still the default) and a
// service window evaluated in the venue's timezone.
func TestVenuesListAgainstPostgres(t *testing.T) {
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
	var a, b uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO restaurants (name) VALUES ('vl-default') RETURNING id`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `INSERT INTO restaurants (name, staff_pin) VALUES ('vl-set', '424242') RETURNING id`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM restaurants WHERE name LIKE 'vl-%'`)
	pins, err := d.RestaurantStaffPINs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pins[a] != "" || pins[b] != "424242" {
		t.Errorf("pins = %q, %q", pins[a], pins[b])
	}
	w := ServiceWindow{OpenMin: 11 * 60, CloseMin: 22 * 60}
	at := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)
	if !ServiceOpenAt(w, "America/New_York", at) || ServiceOpenAt(w, "Asia/Karachi", at) {
		t.Error("14:00 New York is open, 23:00 Karachi is closed")
	}
}
