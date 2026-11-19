package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// No Lab state (19 Nov 2026), marker no-lab-v1: a device at no venue that was not queued is
// retired the way the dashboard's Retire does it (status + audit entry by 'migration'), a
// queued device and a venue device are untouched, enrollment profiles that placed into Lab
// place into the queue, Engineers become Support (users and open sessions) and their grants go.
// A second run changes nothing.
func TestNoLabMigration(t *testing.T) {
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
	sfx := time.Now().Format("150405000")
	venue, err := d.CreateRestaurant(ctx, Restaurant{Name: "No-lab venue " + sfx})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(serial, placement string, rid any, hidden bool) string {
		var id string
		if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, placement, restaurant_id, hidden, enrollment_status)
			VALUES ($1, $2, $3, $4, 'enrolled') RETURNING id::text`, serial, placement, rid, hidden).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	lab := mk("NL-LAB-"+sfx, "lab", nil, false)
	legacy := mk("NL-OLD-"+sfx, "", nil, false)
	queued := mk("NL-Q-"+sfx, PlacementQueue, nil, false)
	placed := mk("NL-V-"+sfx, "lab", venue.ID, false)
	hiddenLab := mk("NL-H-"+sfx, "", nil, true)

	eng, err := d.CreateUser(ctx, "nl-eng-"+sfx+"@example.com", "x", "engineer", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteUser(ctx, eng.ID)
	sessID := strings.Repeat("ab", 16) + sfx + strings.Repeat("0", 64-32-len(sfx)) // a hashed-looking id: migrations drop others
	if _, err := d.pool.Exec(ctx, `INSERT INTO sessions (id, user_id, username, role, expires_at) VALUES ($1, $2, $3, 'engineer', now() + interval '1 hour')`,
		sessID, eng.ID, eng.Username); err != nil {
		t.Fatal(err)
	}
	var profID string
	if err := d.pool.QueryRow(ctx, `INSERT INTO enrollment_profiles (name, token, placement, created_by) VALUES ($1, $2, 'lab', 'test') RETURNING id::text`,
		"nl-prof-"+sfx, "enr_nl"+sfx).Scan(&profID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO access_levels (subject_type, subject, scope_type, state, level, created_by) VALUES
		('role', 'engineer', 'state', 'lab', 'operate', 'test'), ('user', $1, 'state', 'lab', 'view', 'test')
		ON CONFLICT DO NOTHING`, eng.Username); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM access_seed WHERE k = 'no-lab-v1'`); err != nil {
		t.Fatal(err)
	}
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}

	status := func(id string) string {
		var s string
		if err := d.pool.QueryRow(ctx, `SELECT enrollment_status FROM devices WHERE id = $1::uuid`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for name, id := range map[string]string{"lab": lab, "legacy": legacy} {
		if s := status(id); s != EnrollRetired {
			t.Errorf("%s device: %q, want retired", name, s)
		}
		var n int
		d.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE actor = 'migration' AND action = 'device.retire' AND device_id = $1::uuid`, id).Scan(&n)
		if n != 1 {
			t.Errorf("%s device: %d audit entries, want 1", name, n)
		}
	}
	for name, id := range map[string]string{"queued": queued, "at a venue": placed, "hidden": hiddenLab} {
		if s := status(id); s != "enrolled" {
			t.Errorf("%s device: %q, want untouched", name, s)
		}
	}
	// Retired devices leave the counts and lists, and can be un-retired.
	if n, err := d.CountDevices(ctx, DeviceFilter{State: StateToPlace}); err != nil {
		t.Fatal(err)
	} else if n < 1 {
		t.Errorf("to place count = %d, want the queued device", n)
	}
	var role, sessRole, profPlacement string
	d.pool.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, eng.ID).Scan(&role)
	d.pool.QueryRow(ctx, `SELECT role FROM sessions WHERE user_id = $1`, eng.ID).Scan(&sessRole)
	d.pool.QueryRow(ctx, `SELECT placement FROM enrollment_profiles WHERE id = $1::uuid`, profID).Scan(&profPlacement)
	if role != "operator" || sessRole != "operator" || profPlacement != PlacementQueue {
		t.Errorf("engineer → %q, session → %q, profile → %q", role, sessRole, profPlacement)
	}
	var grants int
	d.pool.QueryRow(ctx, `SELECT count(*) FROM access_levels WHERE (subject_type = 'role' AND subject = 'engineer') OR state = 'lab'`).Scan(&grants)
	if grants != 0 {
		t.Errorf("%d engineer / Lab grants left", grants)
	}

	// Second run: nothing changes (a device un-retired by hand stays as it is).
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET enrollment_status = 'enrolled' WHERE id = $1::uuid`, lab); err != nil {
		t.Fatal(err)
	}
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if s := status(lab); s != "enrolled" {
		t.Errorf("second run retired it again: %q", s)
	}
	var n int
	d.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE actor = 'migration' AND action = 'device.retire' AND device_id = $1::uuid`, legacy).Scan(&n)
	if n != 1 {
		t.Errorf("second run audited again: %d", n)
	}
	if got, ok := NormalizePlacement("lab"); !ok || got != PlacementQueue {
		t.Errorf("NormalizePlacement(lab) = %q %v", got, ok)
	}
	if _, ok := NormalizePlacement("bench"); ok {
		t.Error("NormalizePlacement accepted bench")
	}
}
