package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestServerAndAlertQueriesAgainstPostgres runs the queries that failed on live
// without anyone noticing, because their errors were dropped: the Server page's work
// counts (a renamed table zeroed the whole block) and the cleared-alert resolver
// (an untyped "-$1" meant no cleared alert ever resolved). It also runs the two
// charging rules, rewritten so the window start is visible to the planner.
//
//	docker run -d --name pgtest -e POSTGRES_PASSWORD=x -p 55432:5432 postgres:17-alpine
//	MDM_TEST_DATABASE_URL=postgres://postgres:x@localhost:55432/postgres go test ./internal/db -run Postgres
func TestServerAndAlertQueriesAgainstPostgres(t *testing.T) {
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

	if _, _, _, _, total, _, _, err := d.ServerWorkCounts(ctx); err != nil {
		t.Fatalf("work counts: %v", err)
	} else if total < 0 {
		t.Fatalf("devices total %d", total)
	}

	b := 50
	id, _, _, _, _, err := d.UpsertCheckin(ctx, "PGALERT"+time.Now().Format("150405"), "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAlertIfAbsent(ctx, nil, "battery_low", id, "warning", "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE alerts SET cleared_at = NOW() - INTERVAL '11 minutes'
		WHERE device_id = $1 AND status <> 'resolved'`, id); err != nil {
		t.Fatal(err)
	}
	n, err := d.resolveClearedAlerts(ctx)
	if err != nil {
		t.Fatalf("resolve cleared: %v", err)
	}
	if n < 1 {
		t.Errorf("an alert clear for 11 minutes did not resolve (window is %d)", alertClearMin)
	}

	for _, typ := range []string{"wlc_continuous", "slow_charge_night"} {
		if _, _, err := d.detectRecentRule(ctx, typ, nil, nil); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}
