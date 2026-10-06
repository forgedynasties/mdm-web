package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// Run with the Postgres from server_alerts_pg_test.go:
//
//	MDM_TEST_DATABASE_URL=postgres://postgres:x@localhost:55432/postgres go test ./internal/db -run HardwareSerial
func TestRecordHardwareSerialAgainstPostgres(t *testing.T) {
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
	b := 80
	a1, _, _, _, _, err := d.UpsertCheckin(ctx, "HW-A"+sfx, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	a2, _, _, _, _, err := d.UpsertCheckin(ctx, "HW-B"+sfx, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	hw1, hw2 := "AB"+sfx[:2]+sfx[3:6]+"01", "CD"+sfx[:2]+sfx[3:6]+"02"

	ev, err := d.RecordHardwareSerial(ctx, a1, "HW-A"+sfx, hw1)
	if err != nil || ev.Kind != "first" {
		t.Fatalf("first report: %+v %v", ev, err)
	}
	ev, err = d.RecordHardwareSerial(ctx, a1, "HW-A"+sfx, hw1)
	if err != nil || ev.Kind != "" {
		t.Fatalf("same again must be quiet: %+v %v", ev, err)
	}
	if got := d.HardwareSerialOf(ctx, "HW-A"+sfx); got != hw1 {
		t.Fatalf("stored %q, want %q", got, hw1)
	}
	// The same unit, now under another AIO serial (a corrupted serial).
	ev, err = d.RecordHardwareSerial(ctx, a2, "HW-B"+sfx, hw1)
	if err != nil || ev.Kind != "serial_changed" || len(ev.Others) != 1 || ev.Others[0] != "HW-A"+sfx {
		t.Fatalf("serial changed: %+v %v", ev, err)
	}
	// An AIO serial that now reports a different chip.
	ev, err = d.RecordHardwareSerial(ctx, a1, "HW-A"+sfx, hw2)
	if err != nil || ev.Kind != "board_changed" || ev.Previous != hw1 {
		t.Fatalf("board changed: %+v %v", ev, err)
	}
	rows, err := d.ListHardwareSerials(ctx, hw1, "")
	if err != nil || len(rows) != 2 {
		t.Fatalf("map for %s: %+v %v", hw1, rows, err)
	}
	cur := 0
	for _, r := range rows {
		if r.Current {
			cur++
		}
	}
	if cur != 1 { // only HW-B still reports hw1; HW-A moved to hw2
		t.Errorf("current flags: %d, want 1 (%+v)", cur, rows)
	}
}
