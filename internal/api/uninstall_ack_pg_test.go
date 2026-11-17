package api

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ws"
)

// A successful uninstall drops the package from the device's list at once; a failed one
// leaves it (17 Nov 2026; user: "if uninstall is successful we dont ever need a new list").
func TestUninstallAckForgetsPackage(t *testing.T) {
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
	h := &Handler{db: d, hub: ws.NewHub(), cfg: &config.Config{}}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO devices (serial_number, agent_kind) VALUES ('UA-1', 'dpc') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM devices WHERE serial_number = 'UA-1'`) })
	pool.Exec(ctx, `INSERT INTO device_packages (device_id, package_name) VALUES ($1, 'com.example.a'), ($1, 'com.example.b')`, id)
	send := func(pkg string) uuid.UUID {
		p, _ := json.Marshal(map[string]string{"package": pkg})
		c, err := d.CreateCommandBy(ctx, "uninstall", "", p, "devices", []uuid.UUID{id}, "test")
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	h.afterUninstallAck(ctx, send("com.example.a"), id, "completed")
	h.afterUninstallAck(ctx, send("com.example.b"), id, "failed")
	var a, b int
	pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE package_name = 'com.example.a'), count(*) FILTER (WHERE package_name = 'com.example.b') FROM device_packages WHERE device_id = $1`, id).Scan(&a, &b)
	if a != 0 || b != 1 {
		t.Errorf("after the acks: a=%d (want 0, uninstalled), b=%d (want 1, failed)", a, b)
	}
}
