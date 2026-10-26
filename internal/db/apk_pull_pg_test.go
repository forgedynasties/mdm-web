package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRecentAPKPullsRetriesFailedAgainstPostgres(t *testing.T) {
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
	var dev uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class) VALUES ('PULL-1', 'kds') RETURNING id`).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number = 'PULL-1'`)
	mk := func(pkg, status string, failedAgo time.Duration) {
		p, _ := json.Marshal(map[string]string{"package": pkg, "version": "1"})
		c, err := d.CreateCommand(ctx, "pull_apk", "", p, "devices", []uuid.UUID{dev})
		if err != nil {
			t.Fatal(err)
		}
		if status != "" {
			if _, err := d.pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status, updated_at) VALUES ($1, $2, $3, NOW() - $4::interval)
				ON CONFLICT (command_id, device_id) DO UPDATE SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at`, c.ID, dev, status, failedAgo.String()); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer d.pool.Exec(ctx, `DELETE FROM commands WHERE type = 'pull_apk'`)
	mk("aio.app.a", "completed", 0)
	mk("aio.app.b", "failed", time.Hour)   // failed long ago: ask again
	mk("aio.app.c", "failed", time.Minute) // failed just now: wait
	mk("aio.app.d", "", 0)                 // in flight
	got, err := d.RecentAPKPulls(ctx, 6*time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !got["aio.app.a@1"] || got["aio.app.b@1"] || !got["aio.app.c@1"] || !got["aio.app.d@1"] {
		t.Fatalf("recent = %v", got)
	}
}
