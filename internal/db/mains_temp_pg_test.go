package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// A Standard-client device of a mains-only class keeps no battery temperature; a
// handheld and a firmware tablet keep theirs.
func TestMainsStandardDropsBatteryTemp(t *testing.T) {
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
	has := func(serial, extra, class string) bool {
		t.Helper()
		id, _, _, _, shaped, err := d.UpsertCheckin(ctx, serial+suffix, "b1", nil, json.RawMessage(extra), false, "")
		if err != nil {
			t.Fatal(err)
		}
		if class != "" {
			if _, err := d.pool.Exec(ctx, `UPDATE devices SET device_class = $2 WHERE id = $1`, id, class); err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, shaped, err = d.UpsertCheckin(ctx, serial+suffix, "b1", nil, json.RawMessage(extra), true, ""); err != nil {
				t.Fatal(err)
			}
		}
		var x map[string]any
		if err := d.pool.QueryRow(ctx, `SELECT latest_extra FROM devices WHERE id = $1`, id).Scan(&x); err != nil {
			t.Fatal(err)
		}
		_, inRow := x["battery_temp_c"]
		var cur map[string]any
		_ = json.Unmarshal(shaped.Cur, &cur)
		_, inShaped := cur["battery_temp_c"]
		if inRow != inShaped {
			t.Fatalf("%s: row %v, shaped %v", serial, inRow, inShaped)
		}
		return inRow
	}
	box := `{"agent_type":"dpc","model":"HK1 RBOX D8","battery_temp_c":62.2,"cpu_temp_c":61}`
	if has("MT-BOX", box, "dongle") {
		t.Fatal("TV box kept its fake battery temperature")
	}
	if !has("MT-HH", `{"agent_type":"dpc","model":"SM-S146VL","battery_temp_c":31}`, "mpos") {
		t.Fatal("handheld lost its battery temperature")
	}
	if !has("MT-T7", `{"battery_temp_c":41}`, "kiosk") {
		t.Fatal("firmware device lost its battery temperature")
	}
}
