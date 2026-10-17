package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The venue report's queries and the monthly send claim, against a throwaway Postgres:
//
//	docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=x postgres:17-alpine
//	MDM_TEST_DATABASE_URL=postgres://postgres:x@localhost:55432/postgres go test ./internal/db -run ComplianceHistory
func TestComplianceHistoryAndSendClaim(t *testing.T) {
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
	rest, err := d.CreateRestaurant(ctx, Restaurant{Name: "Report venue " + suffix, ContactEmail: "owner@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetRestaurant(ctx, rest.ID); got.ContactEmail != "owner@example.com" {
		t.Fatalf("contact email not stored: %+v", got)
	}
	b := 80
	devID, _, _, _, _, err := d.UpsertCheckin(ctx, "REPORT-A"+suffix, "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AssignDeviceToRestaurant(ctx, "REPORT-A"+suffix, &rest.ID); err != nil {
		t.Fatal(err)
	}
	devs, err := d.VenueComplianceDevices(ctx, rest.ID)
	if err != nil || len(devs) != 1 || devs[0].ID != devID {
		t.Fatalf("venue devices: %+v %v", devs, err)
	}

	// Three changes: one long before the period, two inside it.
	from := time.Now().Add(-48 * time.Hour)
	for _, e := range []struct {
		from, to string
		at       time.Time
	}{
		{"", "pass", from.Add(-30 * 24 * time.Hour)},
		{"pass", "fail", from.Add(-10 * 24 * time.Hour)},
		{"fail", "pass", from.Add(2 * time.Hour)},
	} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO compliance_events (device_id, check_id, from_state, to_state, level, at) VALUES ($1, 'locked', $2, $3, 'required', $4)`,
			devID, e.from, e.to, e.at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO compliance_results (device_id, check_id, state, level, since) VALUES ($1, 'locked', 'pass', 'required', $2)`,
		devID, from.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, evs, err := d.ComplianceHistory(ctx, []uuid.UUID{devID}, []string{"locked", "adb_off"}, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || len(evs) != 2 || evs[0].To != "fail" || evs[1].To != "pass" {
		t.Fatalf("history: %+v %+v", res, evs)
	}

	cmd, err := d.CreateCommandBy(ctx, "app_update", "https://x/a.apk", nil, "devices", []uuid.UUID{devID}, "auto-update")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'installed')
		ON CONFLICT (command_id, device_id) DO UPDATE SET status = 'installed'`, cmd.ID, devID); err != nil {
		t.Fatal(err)
	}
	fixes, err := d.SystemFixes(ctx, []uuid.UUID{devID}, from)
	if err != nil || len(fixes) != 1 || fixes[0].Status != "installed" || fixes[0].By != "auto-update" {
		t.Fatalf("fixes: %+v %v", fixes, err)
	}

	month := "2026-10"
	if ok, err := d.ClaimComplianceReportSend(ctx, rest.ID, month); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, _ := d.ClaimComplianceReportSend(ctx, rest.ID, month); ok {
		t.Fatal("a month is sent once")
	}
	if err := d.ReleaseComplianceReportSend(ctx, rest.ID, month); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.ClaimComplianceReportSend(ctx, rest.ID, month); !ok {
		t.Fatal("a released claim can be taken again")
	}
}
