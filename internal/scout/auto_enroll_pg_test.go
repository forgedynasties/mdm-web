package scout

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"mdm/internal/db"
)

// TestAutoEnrollAgainstPostgres: a ready device found at a venue with auto-enrol on is
// handed to Approve once; off, or blocked, it is not.
func TestAutoEnrollAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	r, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "AE test venue"})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, r.ID)
	var scoutID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO devices (serial_number, restaurant_id) VALUES ('AE-SCOUT', $1) RETURNING id`, r.ID).Scan(&scoutID); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM devices WHERE id = $1`, scoutID)

	var calls []uuid.UUID
	s := New(d, nil, nil)
	s.SetAutoApprove(func(_ context.Context, id uuid.UUID) error { calls = append(calls, id); return nil })

	s.IngestSighting(ctx, scoutID, []byte(`{"serial":"AE-READY","host":"10.0.0.5","users":1}`))
	if len(calls) != 0 {
		t.Fatalf("auto-enrol off, yet Approve was called %d time(s)", len(calls))
	}
	if err := d.SetRestaurantAutoEnroll(ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	s.IngestSighting(ctx, scoutID, []byte(`{"serial":"AE-READY","host":"10.0.0.5","users":1}`))
	s.IngestSighting(ctx, scoutID, []byte(`{"serial":"AE-ACCT","host":"10.0.0.6","users":1,"accounts":1}`))
	id, _, _ := d.SightingAt(ctx, r.ID, "AE-READY")
	if len(calls) != 1 || calls[0] != id {
		t.Errorf("Approve calls = %v, want only AE-READY (%s)", calls, id)
	}
}
