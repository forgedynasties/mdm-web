package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A shell line that never finishes must not hold a device's queue: it fails once past its
// deadline (counted from when the device got it), or when the device reboots after getting
// it. Queued-but-not-received commands, and other lanes, are left alone.
func TestStuckShellCommandsFail(t *testing.T) {
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
	b := 80
	dev, _, _, _, _, err := d.UpsertCheckin(ctx, "STUCK-"+suffix, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(typ string, receivedAgo time.Duration, status string) uuid.UUID {
		c, err := d.CreateCommandBy(ctx, typ, "", []byte(`{"cmd":"sleep 9999"}`), "devices", []uuid.UUID{dev}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if receivedAgo >= 0 {
			if _, err := d.pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status, received_at, updated_at)
				VALUES ($1, $2, $3, NOW() - make_interval(secs => $4), NOW())`, c.ID, dev, status, receivedAgo.Seconds()); err != nil {
				t.Fatal(err)
			}
		}
		return c.ID
	}
	status := func(id uuid.UUID) string {
		var s string
		_ = d.pool.QueryRow(ctx, `SELECT COALESCE((SELECT status FROM command_status WHERE command_id = $1), 'none')`, id).Scan(&s)
		return s
	}
	hung := mk("shell", 20*time.Minute, "running")       // past the 5 min deadline
	fresh := mk("shell", time.Minute, "running")         // still inside it
	queued := mk("shell", -1, "")                        // never reached the device
	install := mk("install_apk", 3*time.Hour, "running") // another lane

	stuck, err := d.FailOverdueShellCommands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 1 || stuck[0].CommandID != hung {
		t.Errorf("overdue sweep failed %v, want only the hung shell", stuck)
	}
	if status(hung) != "failed" || status(fresh) != "running" || status(queued) != "none" || status(install) != "running" {
		t.Errorf("after the sweep: hung=%s fresh=%s queued=%s install=%s", status(hung), status(fresh), status(queued), status(install))
	}

	// The device reboots: the fresh shell it was running is over too.
	stuck, err = d.FailShellCommandsBeforeBoot(ctx, dev, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 1 || stuck[0].CommandID != fresh || status(queued) != "none" || status(install) != "running" {
		t.Errorf("reboot rule failed %v; queued=%s install=%s", stuck, status(queued), status(install))
	}
}
