package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The eligibility query is the one piece of SQL every delivery path runs — WS push, the
// connect flush, both redrive sweeps and the HTTP pull. A column referenced there but not
// carried by its `live` CTE takes all of them down at once, and no unit test notices
// because they never touch a database. That happened: `c.max_attempts` read from `live`
// broke delivery entirely on stage until the CTE was taught to select it.
//
// Run it against a throwaway Postgres:
//
//	docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=x postgres:17-alpine
//	MDM_TEST_DATABASE_URL=postgres://postgres:x@localhost:55432/postgres go test ./internal/db -run Lease
func TestLeaseHoldsAndReleasesACommand(t *testing.T) {
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
	dev, _, _, _, _, err := d.UpsertCheckin(ctx, "LEASE-A"+suffix, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := d.CreateCommandBy(ctx, "shell", "", []byte(`{"cmd":"true"}`), "devices", []uuid.UUID{dev}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Creation stamps the registry's policy, so delivery reads columns instead of
	// re-deciding what a 'shell' is.
	var lane, guarantee string
	if err := d.pool.QueryRow(ctx, `SELECT lane, guarantee FROM commands WHERE id = $1`, cmd.ID).Scan(&lane, &guarantee); err != nil {
		t.Fatal(err)
	}
	if lane != "shell" || guarantee != "at_least_once" {
		t.Fatalf("policy not stamped: lane=%q guarantee=%q", lane, guarantee)
	}

	pending, err := d.GetPendingCommandsForDevice(ctx, dev)
	if err != nil {
		t.Fatalf("eligibility query failed (this is the regression this test exists for): %v", err)
	}
	if len(pending) != 1 || pending[0].ID != cmd.ID {
		t.Fatalf("want the one queued command, got %d", len(pending))
	}

	// Handed out: nobody else may have it while the lease runs.
	if _, err := d.LeaseCommand(ctx, cmd.ID, dev, 5*time.Minute, "http"); err != nil {
		t.Fatal(err)
	}
	pending, err = d.GetPendingCommandsForDevice(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a leased command was handed out again: %d", len(pending))
	}

	// A reconnect proves the previous delivery is dead, so the lease goes and the command
	// is immediately deliverable — the behaviour the WS flush has always relied on.
	if n, err := d.ReleaseDeviceLeases(ctx, dev); err != nil || n != 1 {
		t.Fatalf("release: n=%d err=%v", n, err)
	}
	pending, err = d.GetPendingCommandsForDevice(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("after release want 1 deliverable, got %d", len(pending))
	}

	// Receipt and a terminal ack: history is written, the lease is dropped, and the device
	// is credited with a completed round trip — the only evidence it is controllable.
	if err := d.MarkCommandReceived(ctx, cmd.ID, dev); err != nil {
		t.Fatal(err)
	}
	if err := d.AckCommand(ctx, cmd.ID, dev, "completed"); err != nil {
		t.Fatal(err)
	}
	events, err := d.ListCommandEvents(ctx, cmd.ID, dev)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, e := range events {
		kinds[e.Kind] = true
	}
	for _, want := range []string{"created", "leased", "received", "completed"} {
		if !kinds[want] {
			t.Fatalf("missing %q in command history: %v", want, kinds)
		}
	}
	var held bool
	if err := d.pool.QueryRow(ctx, `SELECT lease_expires_at IS NOT NULL FROM command_status
		WHERE command_id = $1 AND device_id = $2`, cmd.ID, dev).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("a finished command is still holding its lease")
	}
	var roundTrip *time.Time
	if err := d.pool.QueryRow(ctx, `SELECT last_round_trip_ok_at FROM devices WHERE id = $1`, dev).Scan(&roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip == nil {
		t.Fatal("a completed command did not credit the device with a round trip")
	}
}

// Lanes: work serializes against its own kind, not against everything for the device.
// Before this, one queue per device meant a wedged install held up a screenshot, a reboot
// and everything else until the hour-long delivery window let it go — which is the
// opposite of what an operator needs from a device that is misbehaving.
func TestLanesSerializeOnlyAgainstTheirOwnKind(t *testing.T) {
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
	dev, _, _, _, _, err := d.UpsertCheckin(ctx, "LANE-A"+suffix, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}

	// An install first (lane "install"), then a shell (lane "shell"). The install is the
	// long one; the shell must not wait behind it.
	install, err := d.CreateCommandBy(ctx, "install_apk", "https://example.invalid/a.apk", []byte(`{}`), "devices", []uuid.UUID{dev}, "test")
	if err != nil {
		t.Fatal(err)
	}
	shell, err := d.CreateCommandBy(ctx, "shell", "", []byte(`{"cmd":"true"}`), "devices", []uuid.UUID{dev}, "test")
	if err != nil {
		t.Fatal(err)
	}
	// A second shell, which MUST wait: same lane as the first.
	shell2, err := d.CreateCommandBy(ctx, "shell", "", []byte(`{"cmd":"true"}`), "devices", []uuid.UUID{dev}, "test")
	if err != nil {
		t.Fatal(err)
	}

	pending, err := d.GetPendingCommandsForDevice(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, c := range pending {
		got[c.ID] = true
	}
	if !got[install.ID] {
		t.Error("the install should be deliverable (nothing older in its lane)")
	}
	if !got[shell.ID] {
		t.Error("the shell is in another lane and must not wait behind the install")
	}
	if got[shell2.ID] {
		t.Error("the second shell shares a lane with the first and must wait for it")
	}
}
