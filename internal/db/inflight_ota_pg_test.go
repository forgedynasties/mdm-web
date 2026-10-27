package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// InFlightOTAs, against a throwaway Postgres:
//
//	docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=x postgres:17-alpine
//	MDM_TEST_DATABASE_URL=postgres://postgres:x@localhost:55432/postgres go test ./internal/db -run InFlightOTAs
func TestInFlightOTAs(t *testing.T) {
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

	suffix := time.Now().Format("150405.000")
	dev := func(name string) uuid.UUID {
		id, _, _, _, _, err := d.UpsertCheckin(ctx, "OTA-"+name+suffix, "v2.1.041", nil, nil, false, "t7")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ota := func(id uuid.UUID, status string, age time.Duration) {
		c, err := d.CreateCommandBy(ctx, "ota", "", []byte(`{}`), "devices", []uuid.UUID{id}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `UPDATE commands SET created_at = NOW() - $2::interval WHERE id = $1`,
			c.ID, age.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO command_status (command_id, device_id, status, updated_at) VALUES ($1, $2, $3, NOW())
			ON CONFLICT (command_id, device_id) DO UPDATE SET status = EXCLUDED.status`, c.ID, id, status); err != nil {
			t.Fatal(err)
		}
	}

	downloading, installing, failed, stale, idle := dev("dl"), dev("in"), dev("fail"), dev("old"), dev("idle")
	ota(downloading, "downloading", time.Minute)
	ota(installing, "installing", time.Hour)
	ota(failed, "failed", time.Minute)
	ota(stale, "downloading", 7*time.Hour) // abandoned by a killed agent

	got, err := d.InFlightOTAs(ctx, []uuid.UUID{downloading, installing, failed, stale, idle})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]bool{downloading: true, installing: true}
	if len(got) != len(want) || !got[downloading] || !got[installing] {
		t.Fatalf("InFlightOTAs = %v, want only downloading+installing", got)
	}
}
