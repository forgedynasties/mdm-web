package dashboard

import (
	"testing"
	"time"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

func TestBackendWeek(t *testing.T) {
	wk := backendWeek([]aiobackend.DayHours{
		{Day: "monday", Open: false},
		{Day: "tuesday", Open: true, Slots: [][2]string{{"11:00", "14:00"}, {"17:00", "20:00"}}},
		{Day: "friday", Open: true, Slots: [][2]string{{"11:00", "21:00"}}},
		{Day: "saturday", Open: true, Slots: [][2]string{{"18:00", "02:00"}}},
		{Day: "sunday", Open: true},
	})
	if wk == nil {
		t.Fatal("nil week")
	}
	want := db.Week{{Closed: true}, {Open: 660, Close: 1200}, {Closed: true}, {Closed: true}, {Open: 660, Close: 1260}, {Open: 1080, Close: 120}, {Open: 0, Close: 1440}}
	if *wk != want {
		t.Fatalf("got %+v", *wk)
	}
	if backendWeek(nil) != nil || backendWeek([]aiobackend.DayHours{{Day: "monday"}}) != nil {
		t.Fatal("no open day should give no week")
	}
}

func TestMergeBackend(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	wk := &db.Week{{Closed: true}, {Open: 660, Close: 1200}}
	b := venueValues{Address: "609 1st St Brentwood, CA 94513", Lat: f(37.9319), Lon: f(-121.6957), Timezone: "America/Los_Angeles", Week: wk}

	// Empty venue: everything filled and followed.
	out, changed, follow := mergeBackend(venueValues{}, b, map[string]bool{})
	if len(changed) != 4 || out.Address != b.Address || out.Timezone != b.Timezone || out.Week != wk || !follow["hours"] || !follow["pin"] {
		t.Fatalf("empty: %+v %v %v", out, changed, follow)
	}
	// A value someone typed that differs stays (Set here); an equal one starts following.
	cur := venueValues{Address: "613 1st St", Lat: f(37.9319), Lon: f(-121.6957), Timezone: "America/Los_Angeles", OwnHours: true, Week: nil}
	out, changed, follow = mergeBackend(cur, b, map[string]bool{})
	if len(changed) != 0 || out.Address != "613 1st St" || follow["address"] || !follow["pin"] || !follow["timezone"] || follow["hours"] {
		t.Fatalf("set here: %+v %v %v", out, changed, follow)
	}
	// Following: the backend's new value replaces the MDM's.
	out, changed, _ = mergeBackend(cur, b, map[string]bool{"address": true, "hours": true})
	if out.Address != b.Address || out.Week != wk || len(changed) != 2 {
		t.Fatalf("follow: %+v %v", out, changed)
	}
	// A timezone Go doesn't know is ignored.
	_, changed, _ = mergeBackend(venueValues{}, venueValues{Timezone: "Mars/Olympus"}, map[string]bool{})
	if len(changed) != 0 {
		t.Fatalf("bad tz applied: %v", changed)
	}
}

func TestServiceWindowWeek(t *testing.T) {
	wk := db.Week{{Closed: true}, {Open: 660, Close: 1200}, {Open: 660, Close: 1200}, {Open: 660, Close: 1200}, {Open: 660, Close: 1260}, {Open: 540, Close: 1200}, {Open: 540, Close: 1200}}
	w := db.ServiceWindow{OpenMin: 540, CloseMin: 1260, TZ: "America/Los_Angeles", Week: &wk}
	la, _ := time.LoadLocation("America/Los_Angeles")
	mon10 := time.Date(2026, 10, 26, 12, 0, 0, 0, la) // a Monday, noon: closed all day
	fri20 := time.Date(2026, 10, 23, 20, 30, 0, 0, la)
	tue1030 := time.Date(2026, 10, 27, 10, 30, 0, 0, la)
	if w.InService(mon10, "") {
		t.Error("Monday is closed")
	}
	if !w.InService(fri20, "") {
		t.Error("Friday 20:30 is open (closes 21:00)")
	}
	if w.InService(tue1030, "") {
		t.Error("Tuesday opens at 11:00")
	}
	if !db.ServiceOpenAt(w, "", fri20) || db.ServiceOpenAt(w, "", mon10) {
		t.Error("ServiceOpenAt disagrees")
	}
	if got := nextOpenLabel(w, mon10); got != "Tue 11:00" {
		t.Errorf("next open %q", got)
	}
	if o, c, _ := wk.Span(); o != 540 || c != 1260 {
		t.Errorf("span %d–%d", o, c)
	}
	days := weekDays(&wk, 0, 0)
	if days[0].Label() != "Closed" || days[4].Label() != "11:00 – 21:00" {
		t.Errorf("labels %q %q", days[0].Label(), days[4].Label())
	}
}
