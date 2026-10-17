package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// A month in a venue at UTC+0 with four devices:
//
//	A: locked failed across the month's start (from 28 Sep to 3 Oct), clean after.
//	B: client_current fails on 9 Oct for two days; the auto-update completed meanwhile.
//	C: wireless adb opened on 20 Oct and is still open.
//	D: only unknown results (an old image), and no events for lock at all.
func TestBuildVenueSecurity(t *testing.T) {
	loc := time.UTC
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, loc)
	A, B, C, D := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	devices := []db.ComplianceVenueDevice{{ID: A, Serial: "SA"}, {ID: B, Serial: "SB"}, {ID: C, Serial: "SC"}, {ID: D, Serial: "SD"}}
	names := map[uuid.UUID]string{A: "Table 1", B: "Table 9"}
	at := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, loc) }
	ev := func(dev uuid.UUID, check, fromS, toS string, t time.Time) db.ComplianceEvent {
		return db.ComplianceEvent{DeviceID: dev, CheckID: check, From: fromS, To: toS, Level: db.ComplianceRequired, At: t}
	}
	res := func(dev uuid.UUID, check, state string, since time.Time) db.ComplianceResult {
		return db.ComplianceResult{DeviceID: dev, CheckID: check, State: state, Level: db.ComplianceRequired, Since: since}
	}
	events := []db.ComplianceEvent{
		ev(A, "locked", "pass", "fail", at(9, 28, 12)), // the last event before the month
		ev(A, "locked", "fail", "pass", at(10, 3, 12)),
		ev(B, "client_current", "pass", "fail", at(10, 9, 6)),
		ev(B, "client_current", "fail", "pass", at(10, 11, 6)),
		ev(C, "wireless_adb_off", "pass", "fail", at(10, 20, 10)),
	}
	results := []db.ComplianceResult{
		res(A, "locked", "pass", at(10, 3, 12)),
		res(A, "adb_off", "pass", at(8, 1, 0)),
		res(A, "client_current", "pass", at(8, 1, 0)),
		res(B, "adb_off", "pass", at(8, 1, 0)),
		res(B, "client_current", "pass", at(10, 11, 6)),
		res(B, "locked", "pass", at(8, 1, 0)), // no events in the record: steady since August
		res(C, "wireless_adb_off", "fail", at(10, 20, 10)),
		res(C, "client_current", "pass", at(8, 1, 0)),
		res(D, "adb_off", "unknown", at(8, 1, 0)),
		res(D, "client_current", "unknown", at(8, 1, 0)),
	}
	fixes := []db.SystemFix{{DeviceID: B, Type: "app_update", By: "auto-update", At: at(10, 10, 2), Status: "installed"}}

	v := buildVenueSecurity(devices, names, results, events, fixes, from, to, now, loc)
	byKey := map[string]compLine{}
	for _, l := range v.Lines {
		byKey[l.Statement.Key] = l
	}
	if l := byKey["locked"]; l.Measured != 2 || l.Clean != 1 || l.DaysMeasured != 31 || l.DaysClean != 28 {
		t.Fatalf("locked: %+v", l)
	}
	if l := byKey["closed"]; l.Measured != 3 || l.Clean != 2 || l.DaysClean != 19 || l.DaysMeasured != 31 {
		t.Fatalf("closed: %+v", l)
	}
	if l := byKey["current"]; l.Measured != 3 || l.Clean != 2 || l.DaysClean != 28 || l.AllClean() {
		t.Fatalf("current: %+v", l)
	}
	if v.Unmeasured != 2 { // D measures nothing; C has no lock rule
		t.Fatalf("unmeasured %d", v.Unmeasured)
	}
	if len(v.Exceptions) != 3 {
		t.Fatalf("exceptions: %+v", v.Exceptions)
	}
	got := []string{v.Exceptions[0].Sentence(loc), v.Exceptions[1].Sentence(loc), v.Exceptions[2].Sentence(loc)}
	want := []string{
		"Before 1 Oct: Table 1 was not locked to its app for 2 days. Back to normal on 3 Oct.",
		"9 Oct: Table 9 ran older software for 2 days. Updated automatically.",
		"20 Oct: SC had debugging or remote access open. Not resolved yet.",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("exception %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
	if len(v.Weeks) != 5 || v.Weeks[0].From.Day() != 1 || v.Weeks[1].From.Weekday() != time.Monday {
		t.Fatalf("weeks: %+v", v.Weeks)
	}
}

// A fix that was sent but never completed is not claimed, and a recommended-level
// failure still counts (only "off" doesn't).
func TestVenueSecurityFixMustComplete(t *testing.T) {
	loc := time.UTC
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	to := from.AddDate(0, 0, 7)
	B := uuid.New()
	ev := []db.ComplianceEvent{
		{DeviceID: B, CheckID: "client_current", From: "pass", To: "fail", Level: db.ComplianceRecommended, At: from.Add(24 * time.Hour)},
		{DeviceID: B, CheckID: "client_current", From: "fail", To: "pass", Level: db.ComplianceRecommended, At: from.Add(30 * time.Hour)},
	}
	res := []db.ComplianceResult{{DeviceID: B, CheckID: "client_current", State: "pass", Level: db.ComplianceRecommended, Since: from.Add(30 * time.Hour)}}
	fixes := []db.SystemFix{{DeviceID: B, Type: "app_update", By: "auto-update", At: from.Add(25 * time.Hour), Status: "failed"}}
	v := buildVenueSecurity([]db.ComplianceVenueDevice{{ID: B, Serial: "SB"}}, nil, res, ev, fixes, from, to, to, loc)
	if len(v.Exceptions) != 1 || v.Exceptions[0].AutoFixed {
		t.Fatalf("%+v", v.Exceptions)
	}
	if s := v.Exceptions[0].Sentence(loc); !strings.HasSuffix(s, "for 6 hours. Back to normal on 2 Oct.") {
		t.Fatalf("%q", s)
	}
	if v.Lines[2].Clean != 0 {
		t.Fatalf("a recommended failure must count: %+v", v.Lines[2])
	}
}

// A device whose check was switched off after it failed (no current result) has a
// zero-length lapse at that event; one with nothing at all is unmeasured.
func TestCompSegmentsEdges(t *testing.T) {
	at := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	segs := compSegments([]db.ComplianceEvent{{To: "fail", Level: "required", At: at}}, nil)
	if len(segs) != 1 || !segs[0].end.Equal(at) || !segs[0].overlaps(at.Add(-time.Hour), at.Add(time.Hour), at.Add(time.Hour)) {
		t.Fatalf("%+v", segs)
	}
	if compSegments(nil, nil) != nil {
		t.Fatal("nothing recorded is no segment")
	}
	// A from-state older than the record holds up to the first event.
	segs = compSegments([]db.ComplianceEvent{{From: "fail", To: "pass", Level: "required", At: at}}, &db.ComplianceResult{State: "pass"})
	if len(segs) != 2 || segs[0].state != "fail" || !segs[0].start.IsZero() || !segs[1].end.IsZero() {
		t.Fatalf("%+v", segs)
	}
}
