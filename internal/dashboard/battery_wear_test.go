package dashboard

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func climbJSON(s []int, scr int, gap int, tmax float64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"mv0": 3700, "step": 100, "s": s, "scr": []int{0, 0, 0, 0, scr}, "gap_s": gap, "end": "top", "temp_c": []float64{35, tmax}})
	return b
}

func TestReadClimb(t *testing.T) {
	at := time.Now()
	if r, ok := readClimb(at, climbJSON([]int{0, 7600, 13000, 16200, 19900}, 0, 16, 46)); !ok || r.Secs != 19900 || r.Hot {
		t.Fatalf("clean climb: %+v %v", r, ok)
	}
	if _, ok := readClimb(at, climbJSON([]int{0, 7600, 13000, 16200, 19900}, 300, 16, 46)); ok {
		t.Fatal("screen on for 5 min must not count")
	}
	if _, ok := readClimb(at, climbJSON([]int{0, 7600, 13000, 16200, 19900}, 0, 120, 46)); ok {
		t.Fatal("a sparse climb must not count")
	}
	partial, _ := json.Marshal(map[string]any{"mv0": 3700, "step": 100, "s": []int{0, 3944}, "end": "unplug"})
	if _, ok := readClimb(at, partial); ok {
		t.Fatal("a partial ladder must not count")
	}
	if r, ok := readClimb(at, climbJSON([]int{0, 8888, 13967, 17291, 21234}, 14, 8, 54)); !ok || !r.Hot {
		t.Fatalf("hot climb: %+v %v", r, ok)
	}
}

// The live numbers of 15–17 Oct: two fast lab units among a tight fleet.
func TestComputeFleetWearLive(t *testing.T) {
	full := map[string][]int{
		"00044": {16827, 16731}, "00387": {17606, 17048}, "00064": {20430}, "00077": {20308, 20103},
		"00089": {20047, 19931}, "00122": {19317, 19751}, "00142": {19653, 19914}, "00172": {20005, 19831},
		"00226": {20177, 20070}, "00231": {19620}, "00280": {20111, 19898}, "00429": {19532, 19698},
		"00442": {19554, 19964}, "00485": {20292}, "00606": {21234}, "00790": {19482}, "00833": {20546},
	}
	var climbs []db.DeviceBatteryClimb
	base := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)
	for sn, ts := range full {
		id := uuid.New()
		for i, secs := range ts {
			climbs = append(climbs, db.DeviceBatteryClimb{DeviceID: id, Serial: "AT070AABU" + sn, At: base.Add(time.Duration(i) * 24 * time.Hour),
				Payload: climbJSON([]int{0, secs / 3, secs / 2, secs * 4 / 5, secs}, 0, 16, 46)})
		}
	}
	fw := computeFleetWear(climbs)
	if fw.Devices != 17 || fw.MedianSecs != 19918 {
		t.Fatalf("devices %d median %d", fw.Devices, fw.MedianSecs)
	}
	if fw.FastCount != 2 || fw.Rows[0].Serial != "AT070AABU00044" || fw.Rows[0].VsFleet != 84 || fw.Rows[1].VsFleet != 87 {
		t.Fatalf("fast rows: %d %+v %+v", fw.FastCount, fw.Rows[0], fw.Rows[1])
	}
	if got := fmtClimb(fw.MedianSecs); got != "5 h 32 m" {
		t.Fatalf("median label %q", got)
	}
}

func TestComputeWearCard(t *testing.T) {
	var climbs []db.BatteryClimb
	start := time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		secs := 20000 - i*100 // a pack losing ~0.5 % a night
		climbs = append(climbs, db.BatteryClimb{At: start.Add(time.Duration(i) * 24 * time.Hour), Payload: climbJSON([]int{0, 1, 2, 3, secs}, 0, 16, 45)})
	}
	c := computeWearCard(climbs, 19918)
	if !c.HasOwn || c.OwnSecs != 19900 || len(c.Readings) != wearCardReadings {
		t.Fatalf("card: %+v", c)
	}
	if want := (17500 + 17400 + 17300 + 17200 + 17100) / 5; c.MeanSecs != want {
		t.Fatalf("mean %d want %d", c.MeanSecs, want)
	}
	if !c.Fast || c.VsOwn >= 90 {
		t.Fatalf("expected a fast pack well below its own start: %+v", c)
	}
	short := computeWearCard(climbs[:4], 19918)
	if short.HasOwn {
		t.Fatal("four nights are not a baseline")
	}
	none := computeWearCard(nil, 19918)
	if none.Clean != 0 || fmt.Sprint(none.Readings) != "[]" {
		t.Fatalf("empty card: %+v", none)
	}
}
