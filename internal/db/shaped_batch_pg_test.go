package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestShapedHistoryAgainstPostgres runs real check-ins through UpsertCheckin and writes
// their history with WriteShapedBatch, then reads back what landed. It needs a
// throwaway database (it migrates it), so it only runs with MDM_TEST_DATABASE_URL set:
//
//	docker run -d --name pgtest -e POSTGRES_PASSWORD=x -p 55432:5432 postgres:17-alpine
//	MDM_TEST_DATABASE_URL=postgres://postgres:x@localhost:55432/postgres go test ./internal/db -run Postgres
func TestShapedHistoryAgainstPostgres(t *testing.T) {
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
	d.SetCheckinSampleSec(60)

	serial := "PGTEST" + time.Now().Format("150405")
	report := func(extra string, battery int) ShapedTelemetry {
		t.Helper()
		b := battery
		_, _, _, _, shaped, err := d.UpsertCheckin(ctx, serial, "v2.1.099", &b, json.RawMessage(extra), false, "t7")
		if err != nil {
			t.Fatal(err)
		}
		return shaped
	}
	// Four reports, written as one batch the way the queue does:
	//  1. first sight: stores a sample, seeds every state key it carries
	//  2. same state, same battery, inside the window: coalesced, and its seeds are repeats
	//  3. battery moved: stores a sample, no state events
	//  4. charging flipped: a real transition, stores a sample
	idle := `{"charging":false,"timezone":"Asia/Karachi","battery_temp_c":31.5,"wifi_rssi":-60,"ram_usage_mb":{"used":1200,"total":3800},"storage_free_gb":12.25}`
	items := []ShapedTelemetry{
		report(idle, 80),
		report(idle, 80),
		report(idle, 79),
		report(`{"charging":true,"timezone":"Asia/Karachi","battery_temp_c":32}`, 79),
	}
	if items[1].Stored {
		t.Fatal("an unchanged report inside the sample window stored a sample")
	}
	if err := d.WriteShapedBatch(ctx, items); err != nil {
		t.Fatal(err)
	}

	id := items[0].DeviceID
	var samples int
	var temp, storage *float64
	var rssi *int16
	var used, total *int32
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM device_samples WHERE device_id = $1`, id).Scan(&samples); err != nil {
		t.Fatal(err)
	}
	if samples != 3 {
		t.Fatalf("%d samples, want 3 (reports 1, 3 and 4)", samples)
	}
	if err := d.pool.QueryRow(ctx, `SELECT temp_c, wifi_rssi, ram_used_mb, ram_total_mb, storage_free_gb
		FROM device_samples WHERE device_id = $1 AND at = $2`, id, items[0].At).
		Scan(&temp, &rssi, &used, &total, &storage); err != nil {
		t.Fatalf("first sample not at the check-in's own instant: %v", err)
	}
	if temp == nil || *temp != 31.5 || rssi == nil || *rssi != -60 || used == nil || *used != 1200 ||
		total == nil || *total != 3800 || storage == nil || *storage != 12.25 {
		t.Fatalf("first sample columns: temp %v rssi %v ram %v/%v storage %v", temp, rssi, used, total, storage)
	}

	rows, err := d.pool.Query(ctx, `SELECT key, from_val, to_val FROM device_state_events
		WHERE device_id = $1 ORDER BY at, key`, id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var k, f, to string
		if err := rows.Scan(&k, &f, &to); err != nil {
			t.Fatal(err)
		}
		got = append(got, k+":"+f+">"+to)
	}
	want := []string{"charging:>false", "timezone:>Asia/Karachi", "charging:false>true"}
	if len(got) != len(want) {
		t.Fatalf("state events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state events %v, want %v", got, want)
		}
	}

	// A second batch for the same device must not seed again: the probe sees batch one.
	again := report(idle, 70)
	if err := d.WriteShapedBatch(ctx, []ShapedTelemetry{again}); err != nil {
		t.Fatal(err)
	}
	var seeds int
	_ = d.pool.QueryRow(ctx, `SELECT count(*) FROM device_state_events WHERE device_id = $1 AND key = 'timezone'`, id).Scan(&seeds)
	if seeds != 1 {
		t.Fatalf("timezone has %d events after a second batch, want 1", seeds)
	}
}
