package db

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestCommandAckAndLeaseAgainstPostgres: QA audit, 18 Nov 2026. A command's output is
// kept only from a device it targets; a replayed ack of a finished command changes
// nothing (an unenroll ack must not mark a device unenrolled twice, or a failed one
// completed); and a command can be leased to only one poll at a time.
func TestCommandAckAndLeaseAgainstPostgres(t *testing.T) {
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
	sfx := time.Now().Format("150405.000")
	mk := func(serial string) uuid.UUID {
		id, _, _, _, _, err := d.UpsertCheckin(ctx, serial+sfx, "v1", nil, json.RawMessage(`{}`), false, "")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b := mk("ACKA"), mk("ACKB")
	cmd, err := d.CreateCommandBy(ctx, "unenroll", "", nil, "devices", []uuid.UUID{a}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Output from a device the command doesn't target is refused and not stored.
	if err := d.SaveCommandResult(ctx, cmd.ID, b, "planted"); !errors.Is(err, ErrCommandNotTargeted) {
		t.Fatalf("output from a device not targeted: %v, want ErrCommandNotTargeted", err)
	}
	if err := d.AckCommand(ctx, cmd.ID, b, "completed"); !errors.Is(err, ErrCommandNotTargeted) {
		t.Fatalf("ack from a device not targeted: %v", err)
	}

	// One lease at a time.
	if _, err := d.TryLeaseCommand(ctx, cmd.ID, a, time.Minute, "http"); err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := d.TryLeaseCommand(ctx, cmd.ID, a, time.Minute, "http"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second lease: %v, want ErrLeaseHeld", err)
	}

	// A failed ack, then a replayed "completed": nothing moves, the device is not unenrolled.
	if err := d.AckCommand(ctx, cmd.ID, a, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := d.AckCommand(ctx, cmd.ID, a, "completed"); err != nil {
		t.Fatal(err)
	}
	var status, enroll string
	if err := d.pool.QueryRow(ctx, `SELECT status FROM command_status WHERE command_id=$1 AND device_id=$2`, cmd.ID, a).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `SELECT enrollment_status FROM devices WHERE id=$1`, a).Scan(&enroll); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("status after a replayed ack: %s, want failed", status)
	}
	if enroll == EnrollUnenrolled {
		t.Errorf("a replayed ack of a failed unenroll marked the device unenrolled")
	}
	if _, err := d.TryLeaseCommand(ctx, cmd.ID, a, time.Minute, "http"); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("a finished command was leased again: %v", err)
	}
}
