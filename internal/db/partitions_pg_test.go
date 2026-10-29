package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPartitionsAgainstPostgres converts the telemetry tables to monthly partitions with
// data in them, then checks writes, migrations, retention, thinning and cascades still
// work. Run against Postgres 14 (what RDS runs). See TestServerAndAlertQueriesAgainstPostgres.
func TestPartitionsAgainstPostgres(t *testing.T) {
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
	if done, _ := d.IsPartitioned(ctx, "device_samples"); done {
		t.Skip("this database was converted by an earlier run")
	}
	b := 50
	dev, _, _, _, _, err := d.UpsertCheckin(ctx, "PART"+time.Now().Format("150405.000"), "v2.1.099", &b, nil, false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := d.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	now := time.Now().UTC()
	old := monthStart(now).AddDate(0, -4, 0) // a whole month well past 60 days
	// Samples: one a minute for two days of the old month, plus recent ones and a bogus
	// 1970 reading that must survive in the default partition.
	for i := 0; i < 2*24*60; i++ {
		exec(`INSERT INTO device_samples (device_id, at, battery_pct) VALUES ($1, $2, 50)`, dev, old.Add(time.Duration(i)*time.Minute))
	}
	for i := 0; i < 30; i++ {
		exec(`INSERT INTO device_samples (device_id, at, battery_pct) VALUES ($1, $2, 60)`, dev, now.Add(-time.Duration(i)*time.Hour))
	}
	exec(`INSERT INTO device_samples (device_id, at, battery_pct) VALUES ($1, '1970-01-02', 1)`, dev)
	exec(`INSERT INTO battery_samples (device_id, t, up_s, ev, pct) VALUES ($1, $2, 1, 'b', 40), ($1, $3, 2, 'b', 41)`,
		dev, monthStart(now).AddDate(0, -8, 0), now)
	exec(`INSERT INTO app_usage (device_id, bucket, daily, package, fg_ms) VALUES ($1, $2, true, 'aio.app.nugget', 5), ($1, $3, false, 'aio.app.nugget', 7)`,
		dev, monthStart(now).AddDate(0, -5, 0), now.Truncate(time.Hour))
	samplesBefore := count(`SELECT COUNT(*) FROM device_samples`)

	for _, s := range PartSpecs {
		did, err := d.MigrateToPartitioned(ctx, s)
		if err != nil || !did {
			t.Fatalf("migrate %s: did=%v err=%v", s.Table, did, err)
		}
		if ok, _ := d.IsPartitioned(ctx, s.Table); !ok {
			t.Fatalf("%s not partitioned after migrate", s.Table)
		}
		if again, err := d.MigrateToPartitioned(ctx, s); err != nil || again {
			t.Fatalf("second migrate of %s: did=%v err=%v", s.Table, again, err)
		}
	}
	if n := count(`SELECT COUNT(*) FROM device_samples`); n != samplesBefore {
		t.Fatalf("samples after swap = %d, want %d", n, samplesBefore)
	}
	if n := count(`SELECT COUNT(*) FROM device_samples_pdefault`); n != 1 {
		t.Errorf("the 1970 reading should sit in the default partition, got %d rows there", n)
	}
	if n := count(`SELECT COUNT(*) FROM pg_constraint WHERE conname = 'device_samples_pkey' AND conrelid = 'device_samples'::regclass`); n != 1 {
		t.Error("device_samples_pkey should be back under its own name on the partitioned table")
	}
	if n := count(`SELECT COUNT(*) FROM app_usage`); n != 2 {
		t.Errorf("app_usage rows = %d, want 2", n)
	}

	// Startup migrations must still run cleanly on the partitioned tables.
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatalf("migrations after partitioning: %v", err)
	}
	// Writes keep working and land in the right month; an upsert still conflicts.
	exec(`INSERT INTO device_samples (device_id, at, battery_pct) VALUES ($1, $2, 70)`, dev, now.Add(time.Minute))
	exec(`INSERT INTO app_usage (device_id, bucket, daily, package, fg_ms) VALUES ($1, $2, false, 'aio.app.nugget', 3)
		ON CONFLICT (device_id, daily, bucket, package) DO UPDATE SET fg_ms = app_usage.fg_ms + EXCLUDED.fg_ms`, dev, now.Truncate(time.Hour))
	if n := count(`SELECT fg_ms FROM app_usage WHERE NOT daily`); n != 10 {
		t.Errorf("app_usage upsert after partitioning: fg_ms = %d, want 10", n)
	}
	cur := partName("device_samples", monthStart(now))
	if n := count(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, cur)); n < 2 {
		t.Errorf("this month's partition holds %d rows", n)
	}

	// A row for a month with no partition goes to the default; making the month moves it.
	far := monthStart(now).AddDate(0, partMonthsAhead+1, 0)
	exec(`INSERT INTO device_samples (device_id, at, battery_pct) VALUES ($1, $2, 80)`, dev, far.Add(time.Hour))
	if err := d.ensureMonth(ctx, PartSpec{"device_samples", "at"}, far); err != nil {
		t.Fatalf("ensure a month with rows in default: %v", err)
	}
	if n := count(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, partName("device_samples", far))); n != 1 {
		t.Errorf("the row should have moved into its new month, found %d", n)
	}
	if err := d.EnsurePartitions(ctx); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}

	// Thinning: the old month to one per 5 minutes, once.
	before, after, done, err := d.ThinSamplesMonth(ctx, now.AddDate(0, 0, -60), 300)
	if err != nil || !done {
		t.Fatalf("thin: done=%v err=%v", done, err)
	}
	if before != 2*24*60 || after != 2*24*12 {
		t.Errorf("thinned %d → %d, want %d → %d", before, after, 2*24*60, 2*24*12)
	}
	if _, _, again, err := d.ThinSamplesMonth(ctx, now.AddDate(0, 0, -60), 300); err != nil || again {
		t.Errorf("a thinned month must not be thinned again: again=%v err=%v", again, err)
	}
	if n := count(`SELECT COUNT(*) FROM pg_constraint WHERE conrelid = to_regclass($1) AND contype = 'p'`, partName("device_samples", old)); n != 1 {
		t.Error("the thinned month should have its primary key back")
	}

	// Retention: whole months older than the cut go; the 1970 row in default too.
	n, err := d.DropMonthsBefore(ctx, PartSpec{"battery_samples", "t"}, now.Add(-180*24*time.Hour))
	if err != nil || n < 1 {
		t.Errorf("drop battery months: n=%d err=%v", n, err)
	}
	if c := count(`SELECT COUNT(*) FROM battery_samples`); c != 1 {
		t.Errorf("battery rows after retention = %d, want the recent 1", c)
	}
	if _, err := d.PruneBatteryPartitioned(ctx, 180*24*time.Hour); err != nil {
		t.Errorf("PruneBatteryPartitioned: %v", err)
	}

	// Deleting a device still cascades into every partition.
	exec(`DELETE FROM devices WHERE id = $1`, dev)
	for _, s := range PartSpecs {
		if c := count(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE device_id = $1`, s.Table), dev); c != 0 {
			t.Errorf("%s keeps %d rows of a deleted device", s.Table, c)
		}
	}

	// The kept copies go once past their keep.
	if err := d.DropUnpartitioned(ctx, 0); err != nil {
		t.Fatalf("DropUnpartitioned: %v", err)
	}
	if c := count(`SELECT COUNT(*) FROM pg_class WHERE relname LIKE '%\_unpart' ESCAPE '\'`); c != 0 {
		t.Errorf("%d old copies left", c)
	}
	_ = uuid.Nil
}
