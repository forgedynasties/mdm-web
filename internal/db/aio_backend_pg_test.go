package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestBackendVenueAgainstPostgres: hours per weekday round-trip through service_windows,
// the B1 follow list, backend hours, the known-id set and the shaped rollup still runs.
func TestBackendVenueAgainstPostgres(t *testing.T) {
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
	r, err := d.CreateRestaurant(ctx, Restaurant{Name: "BKV Huanchaco"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, r.ID)

	wk := Week{{Closed: true}, {Open: 660, Close: 1200}, {Open: 660, Close: 1200}, {Open: 660, Close: 1200}, {Open: 660, Close: 1260}, {Open: 540, Close: 1200}, {Open: 540, Close: 1200}}
	if err := d.SetServiceWindow(ctx, ServiceWindow{RestaurantID: &r.ID, NightOpenMin: 1410, NightCloseMin: 360, Week: &wk}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := d.GetRestaurantServiceWindow(ctx, r.ID)
	if err != nil || !ok || got.Week == nil || *got.Week != wk || got.OpenMin != 540 || got.CloseMin != 1260 {
		t.Fatalf("window = %+v ok=%v err=%v", got, ok, err)
	}
	all, err := d.ListRestaurantServiceWindows(ctx)
	if err != nil || all[r.ID].Week == nil {
		t.Fatalf("list: %v %+v", err, all[r.ID])
	}

	if err := d.SetVenueBackendLink(ctx, r.ID, "prod", 2007, "test"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetVenueBackendFollow(ctx, r.ID, map[string]bool{"address": true, "hours": true, "pin": true}); err != nil {
		t.Fatal(err)
	}
	if err := d.StopFollowingBackend(ctx, r.ID, "hours"); err != nil {
		t.Fatal(err)
	}
	f, err := d.VenueBackendFollow(ctx, r.ID)
	if err != nil || !f["address"] || !f["pin"] || f["hours"] {
		t.Fatalf("follow = %v %v", f, err)
	}
	// Relinking the same restaurant keeps the list; another restaurant clears it.
	d.SetVenueBackendLink(ctx, r.ID, "prod", 2007, "test")
	if f, _ := d.VenueBackendFollow(ctx, r.ID); !f["address"] {
		t.Fatal("relink to the same restaurant cleared the follow list")
	}
	d.SetVenueBackendLink(ctx, r.ID, "stage", 4, "test")
	if f, _ := d.VenueBackendFollow(ctx, r.ID); len(f) != 0 {
		t.Fatalf("another restaurant kept %v", f)
	}

	if err := d.SetBackendHours(ctx, "prod", 2007, &wk); err != nil {
		t.Fatal(err)
	}
	bw, err := d.BackendHoursOf(ctx, "prod", 2007)
	if err != nil || bw == nil || *bw != wk {
		t.Fatalf("backend hours %v %v", bw, err)
	}
	if bw, err := d.BackendHoursOf(ctx, "prod", 99999); err != nil || bw != nil {
		t.Fatalf("missing hours %v %v", bw, err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM backend_hours WHERE restaurant_id = 2007`)

	var dev uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class, latest_extra, restaurant_id)
		VALUES ('bkv-1', 't7', '{"app_android_ids":[{"package":"aio.app.nugget","android_id":"ca6ba20578e28ae6"},{"package":"com.other","android_id":"ffff"}]}', $1) RETURNING id`, r.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE id = $1`, dev)
	known, err := d.KnownBackendIDs(ctx)
	if err != nil || !known["BKV-1"] || !known["CA6BA20578E28AE6"] || known["FFFF"] {
		t.Fatalf("known ids: %v", err)
	}
	// The shaped rollup's per-weekday SQL is valid.
	if _, err := d.RollupDailyStatsShaped(ctx, time.Now().UTC().Truncate(24*time.Hour)); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	gs, err := d.queryGuests(ctx, "d.id = $1", dev)
	if err != nil || len(gs) != 1 || gs[0].Guest.ServiceHours == nil {
		t.Fatalf("guest: %v %+v", err, gs)
	}
}
