package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// A battery-less device (a POS here) reports 0% and must not raise "Battery low in service"
// (DP02256GJ0431 at Tacos El Compa, 29 Oct 2026); a T7 tablet that is low still does.
func TestBatteryLowSkipsMainsDevices(t *testing.T) {
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
	zero, ten := 0, 10
	pos, _, _, _, _, err := d.UpsertCheckin(ctx, "BL-POS-"+sfx, "b1", &zero, json.RawMessage(`{"agent_type":"dpc","model":"D3 PRO"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET device_class = 'pos' WHERE id = $1`, pos); err != nil {
		t.Fatal(err)
	}
	tab, _, _, _, _, err := d.UpsertCheckin(ctx, "BL-T7-"+sfx, "b1", &ten, json.RawMessage(`{}`), false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"battery_low"} {
		hits, _, err := d.detectRecentRule(ctx, rule, map[string]float64{"soc_pct": 20}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, h := range hits {
			got[h.Serial] = true
		}
		if got["BL-POS-"+sfx] {
			t.Errorf("%s: the POS (no battery) was flagged", rule)
		}
		if !got["BL-T7-"+sfx] {
			t.Errorf("%s: the T7 at 10%% was not flagged (%v)", rule, tab)
		}
	}
	hits, _, err := d.detectRecentRule(ctx, "battery_high_night", map[string]float64{"soc_pct": 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Serial == "BL-POS-"+sfx {
			t.Error("battery_high_night: the POS (no battery) was flagged")
		}
	}
}
