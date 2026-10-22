package dashboard

import (
	"testing"
	"time"

	"mdm/internal/db"
)

func TestBuildChartNet(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 6, 0, 0, 0, time.UTC)
	h := func(n float64) time.Time { return t0.Add(time.Duration(n * float64(time.Hour))) }
	events := []db.StateAt{
		{At: h(-5), Key: "online", Value: "true"}, // before the window: carries in
		{At: h(-1), Key: "wifi", Value: `"AIO-Guest"`},
		{At: h(4), Key: "online", Value: "false"},
		{At: h(4.5), Key: "online", Value: "false"}, // repeat: no new run
		{At: h(6), Key: "online", Value: "true"},
		{At: h(6), Key: "wifi", Value: `"AIO-Kitchen"`},
	}
	got := buildChartNet(events, h(0), h(12), h(10))
	want := []onlineRun{{h(0).UnixMilli(), h(4).UnixMilli(), true}, {h(4).UnixMilli(), h(6).UnixMilli(), false}, {h(6).UnixMilli(), h(10).UnixMilli(), true}}
	if len(got.Online) != len(want) {
		t.Fatalf("runs = %+v", got.Online)
	}
	for i := range want {
		if got.Online[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, got.Online[i], want[i])
		}
	}
	if len(got.SSID) != 1 || got.SSID[0].SSID != "AIO-Kitchen" || got.SSID[0].X != h(6).UnixMilli() {
		t.Errorf("ssid marks = %+v, want only the switch inside the window", got.SSID)
	}
	if e := buildChartNet(nil, h(0), h(1), h(1)); e.Online == nil || e.SSID == nil {
		t.Error("an empty window must still give arrays, not null")
	}
}

func TestMergeShapedWifi(t *testing.T) {
	v, zero := int16(-61), int16(0)
	pts := mergeShapedSeries([]db.DeviceSample{{At: time.Now(), WifiRSSI: &v}, {At: time.Now(), WifiRSSI: &zero}}, nil)
	if !pts[0].HasWifi || pts[0].WifiRSSI != -61 {
		t.Errorf("rssi -61 lost: %+v", pts[0])
	}
	if pts[1].HasWifi {
		t.Error("0 dBm (no link) charted as a signal level")
	}
}
