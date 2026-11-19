package db

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Export from one database, import into another: people and venues are added or updated,
// Lineup is mirrored, a clash on username is skipped, and a dry run changes nothing.
func TestSyncPeopleLineupBetweenServers(t *testing.T) {
	base := os.Getenv("MDM_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	src, err := New(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	// A second, empty database for the target.
	name := "sync_target_" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	if _, err := src.pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	dst, err := New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}

	// Source: a person, a model, a role app, a venue with its hours.
	alice, err := src.CreateUser(ctx, "alice-sync@example.com", "hash-a", "operator", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.CreateUser(ctx, "bob-sync@example.com", "hash-b", "viewer", nil, true); err != nil {
		t.Fatal(err)
	}
	src.pool.Exec(ctx, `INSERT INTO product_names (product, name, role) VALUES ('sync_model', 'Sync model', 'pos') ON CONFLICT (product) DO UPDATE SET name = EXCLUDED.name`)
	src.pool.Exec(ctx, `INSERT INTO role_apps (role, base_package) VALUES ('pos', 'aio.app.pos') ON CONFLICT (role) DO UPDATE SET base_package = EXCLUDED.base_package`)
	venue, err := src.CreateRestaurant(ctx, Restaurant{Name: "Sync venue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.pool.Exec(ctx, `UPDATE restaurants SET staff_pin = '424242' WHERE id = $1`, venue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := src.pool.Exec(ctx, `INSERT INTO service_windows (restaurant_id, open_min, close_min) VALUES ($1, 600, 1320)`, venue.ID); err != nil {
		t.Fatal(err)
	}

	// Target: bob's username already belongs to someone else; an extra model to be mirrored away.
	if _, err := dst.CreateUser(ctx, "bob-sync@example.com", "other", "viewer", nil, true); err != nil {
		t.Fatal(err)
	}
	dst.pool.Exec(ctx, `INSERT INTO product_names (product, name) VALUES ('stale_model', 'Stale')`)

	b, err := src.ExportPeopleLineup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Through JSON, as over the API.
	raw, _ := json.Marshal(b)
	var in PeopleLineupBundle
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}

	rep, err := dst.ImportPeopleLineup(ctx, &in, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep["users"].Added < 1 || len(rep["users"].Skipped) != 1 {
		t.Errorf("dry run users: %+v", rep["users"])
	}
	var n int
	if dst.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, alice.ID).Scan(&n); n != 0 {
		t.Fatal("dry run wrote")
	}

	if _, err := dst.ImportPeopleLineup(ctx, &in, false); err != nil {
		t.Fatal(err)
	}
	var hash, role string
	dst.pool.QueryRow(ctx, `SELECT password_hash, role FROM users WHERE id = $1`, alice.ID).Scan(&hash, &role)
	if hash != "hash-a" || role != "operator" {
		t.Errorf("alice on target: hash %q role %q", hash, role)
	}
	if dst.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE username = 'bob-sync@example.com'`).Scan(&n); n != 1 {
		t.Errorf("bob clash: %d accounts", n)
	}
	if dst.pool.QueryRow(ctx, `SELECT count(*) FROM product_names WHERE product = 'stale_model'`).Scan(&n); n != 0 {
		t.Errorf("Lineup not mirrored: stale model stayed")
	}
	if dst.pool.QueryRow(ctx, `SELECT count(*) FROM product_names WHERE product = 'sync_model'`).Scan(&n); n != 1 {
		t.Errorf("model missing on target")
	}
	var pin string
	dst.pool.QueryRow(ctx, `SELECT staff_pin FROM restaurants WHERE id = $1`, venue.ID).Scan(&pin)
	if pin != "424242" {
		t.Errorf("venue staff PIN on target: %q", pin)
	}
	if dst.pool.QueryRow(ctx, `SELECT count(*) FROM service_windows WHERE restaurant_id = $1`, venue.ID).Scan(&n); n != 1 {
		t.Errorf("venue hours on target: %d", n)
	}
	// A second run changes nothing.
	again, err := dst.ImportPeopleLineup(ctx, &in, true)
	if err != nil {
		t.Fatal(err)
	}
	for tbl, r := range again {
		if r.Added+r.Changed+r.Removed != 0 {
			t.Errorf("second run %s: %+v", tbl, r)
		}
	}
}
