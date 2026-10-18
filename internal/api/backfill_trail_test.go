package api

import (
	"encoding/json"
	"testing"
)

func TestTrailScan(t *testing.T) {
	if trailScan(nil) != nil || trailScan(json.RawMessage(`{}`)) != nil || trailScan(json.RawMessage(`[]`)) != nil {
		t.Fatal("empty or not a list must give nil")
	}
	if trailScan(json.RawMessage(`[{"ssid":"x","rssi":-50}]`)) != nil {
		t.Fatal("APs without a BSSID are not a scan")
	}
	var aps []map[string]any
	for i := 0; i < 12; i++ {
		aps = append(aps, map[string]any{"bssid": "aa:bb:cc:dd:ee:0" + string(rune('a'+i)), "rssi": -50 - i})
	}
	raw, _ := json.Marshal(aps)
	var got []map[string]any
	_ = json.Unmarshal(trailScan(raw), &got)
	if len(got) != 8 {
		t.Fatalf("kept %d APs, want the 8 the protocol allows", len(got))
	}
}
