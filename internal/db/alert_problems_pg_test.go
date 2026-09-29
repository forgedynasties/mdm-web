package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestProblemQueriesAgainstPostgres runs the problem workflow's SQL: escalation on
// upsert, the workflow actions, paging state, the one-shot "resolved" pickup and the
// digest queries. See TestServerAndAlertQueriesAgainstPostgres for how to run it.
func TestProblemQueriesAgainstPostgres(t *testing.T) {
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
	b := 50
	id, _, _, _, _, err := d.UpsertCheckin(ctx, "PGPROB"+time.Now().Format("150405.000"), "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Second)

	u, err := d.upsertAlert(ctx, nil, "overheating", id, "warning", "hot", map[string]any{"after_hours": true})
	if err != nil || !u.Inserted || u.Escalated || u.ID == uuid.Nil {
		t.Fatalf("first upsert = %+v, %v", u, err)
	}
	u2, err := d.upsertAlert(ctx, nil, "overheating", id, "warning", "hot", nil)
	if err != nil || u2.Inserted || u2.Escalated || u2.ID != u.ID {
		t.Fatalf("repeat upsert = %+v, %v", u2, err)
	}
	u3, err := d.upsertAlert(ctx, nil, "overheating", id, "critical", "hot", nil)
	if err != nil || u3.Inserted || !u3.Escalated {
		t.Fatalf("escalating upsert = %+v, %v", u3, err)
	}
	mem, err := d.upsertAlert(ctx, nil, "memory_low", id, "warning", "mem", nil)
	if err != nil || !mem.Inserted {
		t.Fatalf("second type = %+v, %v", mem, err)
	}

	if err := d.MarkAlertsNotified(ctx, []uuid.UUID{u.ID}); err != nil {
		t.Fatal(err)
	}
	paged, err := d.PagedDevices(ctx, []uuid.UUID{id}, []uuid.UUID{mem.ID})
	if err != nil || !paged[id] {
		t.Fatalf("paged = %v, %v", paged, err)
	}
	ids := []uuid.UUID{u.ID, mem.ID}
	for _, op := range [][2]string{{"assign", "sam"}, {"ack", "on it"}, {"snooze", "4"}, {"snooze", "0"}} {
		if n, err := d.ProblemAction(ctx, ids, op[0], op[1]); err != nil || n != 2 {
			t.Fatalf("%s: n=%d err=%v", op[0], n, err)
		}
	}
	if _, err := d.ProblemAction(ctx, ids, "resolve", "nonsense"); err == nil {
		t.Fatal("bad reason accepted")
	}
	active, err := d.ListDeviceActiveAlerts(ctx, id, 10)
	if err != nil || len(active) != 2 || active[0].Assignee != "sam" || active[0].Note != "on it" || active[0].Status != "acknowledged" {
		t.Fatalf("active = %+v, %v", active, err)
	}
	if _, err := d.ListActiveAlerts(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if closed, err := d.TakeClosedPagedProblems(ctx); err != nil {
		t.Fatal(err)
	} else {
		for _, c := range closed {
			if c.DeviceID == id {
				t.Fatal("problem reported closed while alerts are open")
			}
		}
	}
	if n, err := d.ProblemAction(ctx, ids, "resolve", "false_alarm"); err != nil || n != 2 {
		t.Fatalf("resolve: %d %v", n, err)
	}
	found := false
	closed, err := d.TakeClosedPagedProblems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range closed {
		if c.DeviceID == id {
			found = c.Reason == "false_alarm" && len(c.Types) == 1
		}
	}
	if !found {
		t.Fatalf("closed = %+v", closed)
	}
	if again, _ := d.TakeClosedPagedProblems(ctx); len(again) != 0 {
		for _, c := range again {
			if c.DeviceID == id {
				t.Fatal("resolved message would go out twice")
			}
		}
	}
	if fa, err := d.FalseAlarmCounts(ctx, 30); err != nil || fa["overheating"] < 1 {
		t.Fatalf("false alarms = %v, %v", fa, err)
	}
	if _, err := d.AlertsFiredSince(ctx, start); err != nil {
		t.Fatal(err)
	}
	if _, err := d.OfflineAtOpen(ctx, nil); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC)
	d.ReleaseDigest(ctx, day)
	if ok, err := d.ClaimDigest(ctx, day, 3); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, _ := d.ClaimDigest(ctx, day, 3); ok {
		t.Fatal("digest claimed twice")
	}
	d.ReleaseDigest(ctx, day)
	if _, err := d.ListCrashIssues(ctx, 14, 20); err != nil {
		t.Fatal(err)
	}
	_ = d.IsReleaseCrash(ctx, "x — y")
	if _, err := d.ServiceUptime(ctx, 7, time.Now()); err != nil {
		t.Fatal(err)
	}
}
