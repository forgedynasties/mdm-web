package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mdm/internal/alerts"
	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ws"
)

// A firmware device that lost its key gets back in either way, but only one at a venue
// raises "Key reset automatically": Lab devices are reflashed all the time.
func TestKeyRecoveredAlertOnlyAtVenues(t *testing.T) {
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
	cfg := &config.Config{}
	h := &Handler{db: d, hub: ws.NewHub(), cfg: cfg, alerts: alerts.NewDispatcher(d, cfg)}

	venue, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "Key venue"})
	if err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, device_key_hash, restaurant_id) VALUES ('KR-VENUE', 'firmware', 'kr-v', $1)`, venue.ID)
	pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, device_key_hash) VALUES ('KR-LAB', 'firmware', 'kr-l')`)
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM alerts WHERE device_id IN (SELECT id FROM devices WHERE serial_number LIKE 'KR-%')`)
		pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'KR-%'`)
	})

	for _, s := range []string{"KR-VENUE", "KR-LAB"} {
		if !h.recoverLostKey(ctx, s, "10.0.0.9") {
			t.Fatalf("%s was not let back in", s)
		}
	}
	count := func(serial string) int {
		var n int
		pool.QueryRow(ctx, `SELECT count(*) FROM alerts a JOIN devices d ON d.id = a.device_id
			WHERE d.serial_number = $1 AND a.type = 'key_recovered'`, serial).Scan(&n)
		return n
	}
	// The alert is raised in the background.
	deadline := time.Now().Add(5 * time.Second)
	for count("KR-VENUE") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if count("KR-VENUE") != 1 {
		t.Error("a device at a venue raises the alert")
	}
	time.Sleep(200 * time.Millisecond)
	if n := count("KR-LAB"); n != 0 {
		t.Errorf("a Lab device raised %d alerts", n)
	}

	// One left open on a Lab device from before is resolved at the next start.
	pool.Exec(ctx, `INSERT INTO alerts (type, device_id, severity, summary, status)
		SELECT 'key_recovered', id, 'warning', 'old', 'open' FROM devices WHERE serial_number = 'KR-LAB'`)
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var open int
	pool.QueryRow(ctx, `SELECT count(*) FROM alerts a JOIN devices d ON d.id = a.device_id
		WHERE a.type = 'key_recovered' AND a.status <> 'resolved' AND d.serial_number LIKE 'KR-%'`).Scan(&open)
	if open != 1 {
		t.Errorf("open key alerts after a start = %d, want only the venue device's", open)
	}
}
