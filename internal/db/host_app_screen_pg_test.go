package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHostAppScreenEventsAgainstPostgres(t *testing.T) {
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
	var id uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class) VALUES ('HAS-1', 't7') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number = 'HAS-1'`)
	if _, err := d.pool.Exec(ctx, `INSERT INTO device_state_events (device_id, at, key, from_val, to_val) VALUES
		($1, NOW() - interval '3 hours', 'host_app_fg', '', 'true'),
		($1, NOW() - interval '2 hours', 'screen_on', '', 'true'),
		($1, NOW() - interval '30 minutes', 'host_app_fg', 'true', 'false'),
		($1, NOW() - interval '10 minutes', 'host_app_fg', 'false', 'true'),
		($1, NOW() - interval '5 minutes', 'wifi', '', 'x')`, id); err != nil {
		t.Fatal(err)
	}
	init, evs, err := d.HostAppScreenEvents(ctx, []uuid.UUID{id}, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if init[id]["host_app_fg"] != "true" || init[id]["screen_on"] != "true" {
		t.Errorf("initial = %v", init[id])
	}
	if len(evs[id]) != 2 || evs[id][0].To != "false" || evs[id][1].To != "true" || evs[id][0].Key != "host_app_fg" {
		t.Errorf("events = %+v", evs[id])
	}
}
