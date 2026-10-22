package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAppUsageAgainstPostgres(t *testing.T) {
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
	var id uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class) VALUES ('AU-1', 't7') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number = 'AU-1'`)
	f := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }
	now := time.Now()
	// Two windows in the same hour add up; memory and CPU average; peak is the max.
	for _, pss := range []float64{400, 420} {
		if err := d.AddAppUsage(ctx, id, now, []AppUsageReport{
			{Package: "aio.app.nugget", FgMs: 15 * 60000, Launches: 1, RxBytes: 1 << 20, PssMB: f(pss), PssMaxMB: i(int(pss) + 30), CPUPct: f(10), PowerMAh: 5},
			{Package: "com.google.android.gms", System: true, RxBytes: 1 << 10, PssMB: f(200), CPUPct: f(3)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A 20-day-old hour is folded into its day by the rollup and still counted in 30 d.
	if err := d.AddAppUsage(ctx, id, now.Add(-20*24*time.Hour), []AppUsageReport{{Package: "aio.app.nugget", FgMs: 60000}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RollupAppUsage(ctx, 14*24*time.Hour, 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	rows, err := d.AppUsage(ctx, id, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]AppUsageRow{}
	for _, r := range rows {
		by[r.Package] = r
	}
	n := by["aio.app.nugget"]
	if n.FgMs != 30*60000 || n.Launches != 2 || n.RxBytes != 2<<20 || n.PssAvgMB == nil || *n.PssAvgMB != 410 || n.PssMaxMB != 450 || n.PowerMAh != 10 {
		t.Errorf("nugget 24h = %+v", n)
	}
	if !by["com.google.android.gms"].System {
		t.Error("system flag lost")
	}
	rows, _ = d.AppUsage(ctx, id, now.Add(-30*24*time.Hour))
	for _, r := range rows {
		if r.Package == "aio.app.nugget" && r.FgMs != 31*60000 {
			t.Errorf("nugget 30d fg = %d, want the rolled-up day added", r.FgMs)
		}
	}
	var hourly int
	d.pool.QueryRow(ctx, `SELECT count(*) FROM app_usage WHERE device_id = $1 AND NOT daily AND bucket < NOW() - interval '14 days'`, id).Scan(&hourly)
	if hourly != 0 {
		t.Errorf("%d old hourly rows left after the rollup", hourly)
	}
}
