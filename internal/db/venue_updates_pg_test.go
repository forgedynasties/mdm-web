package db

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestVenueUpdatePolicyAgainstPostgres(t *testing.T) {
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
	r, err := d.CreateRestaurant(ctx, Restaurant{Name: "VU test venue"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, r.ID)
	p, err := d.VenueUpdatePolicy(ctx, r.ID)
	if err != nil || !p.IsDefault(time.Now()) {
		t.Fatalf("new venue = %+v, %v; want the fleet default", p, err)
	}
	if m, _ := d.VenueUpdatePolicies(ctx); len(m) != 0 {
		if _, ok := m[r.ID]; ok {
			t.Error("a default venue is listed")
		}
	}
	until := time.Now().Add(72 * time.Hour)
	if err := d.SetVenueUpdatePolicy(ctx, r.ID, VenueUpdatePolicy{Mode: VenueUpdateOwn, Start: "01:00", End: "04:00", Paused: true, PausedUntil: &until, By: "ali"}); err != nil {
		t.Fatal(err)
	}
	m, err := d.VenueUpdatePolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m[r.ID]
	if !ok || got.Mode != VenueUpdateOwn || got.Start != "01:00" || !got.PausedAt(time.Now()) || got.By != "ali" || got.At == nil {
		t.Errorf("stored = %+v", got)
	}
	if got.PausedAt(until.Add(time.Minute)) {
		t.Error("a timed pause still holds after it ends")
	}
}
