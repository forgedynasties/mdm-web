package dashboard

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestBuildCompWidget(t *testing.T) {
	adb, _ := complianceCheck("wireless_adb_off")
	seen, _ := complianceCheck("checked_in")
	devices := []compDeviceView{
		{Required: true, Compliant: true},
		{Required: true, Compliant: true, Unknown: 1},
		{Required: true, Compliant: true, Partly: true},
		{Required: true, Compliant: false},
		{Required: true, Compliant: false},
		{Required: false, Compliant: true}, // recommendations only: not scored
	}
	views := map[string]*compCheckView{
		adb.ID: {compCheck: adb, Failing: []compDeviceFail{
			{Venue: "Flights Vegas", Level: db.ComplianceRequired}, {Venue: "Flights Vegas", Level: db.ComplianceRequired},
			{Venue: "Pho 88", Level: db.ComplianceRequired}, {Venue: "Pho 88", Level: db.ComplianceRecommended}}},
		seen.ID: {compCheck: seen, Failing: []compDeviceFail{{Venue: "Pho 88", Level: db.ComplianceRequired}}},
	}
	w := buildCompWidget(devices, views, compRules{})
	if w.InScope != 5 || w.Compliant != 3 || w.Failing != 2 || w.Unknown != 2 || w.Partly != 1 || w.Pct != 60 {
		t.Fatalf("counts: %+v", w)
	}
	if len(w.Top) != 2 || w.Top[0].ID != adb.ID || w.Top[0].Count != 3 {
		t.Fatalf("top checks: %+v", w.Top)
	}
	if w.Top[0].Line != "2 at Flights Vegas · auto-fix on" {
		t.Errorf("adb line = %q (auto-fix is on by default for it)", w.Top[0].Line)
	}
	if w.Top[1].Line != "all at Pho 88" || w.Top[1].Href != "/compliance?tab=devices&show=failing&check=checked_in" {
		t.Errorf("second row: %+v", w.Top[1])
	}
	if empty := buildCompWidget(nil, views, compRules{}); empty.InScope != 0 || empty.OkDash != "" {
		t.Errorf("no devices: %+v", empty)
	}
}

func TestCompPctFromEvents(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	ev := func(d uuid.UUID, check, to, level string, when time.Time) db.ComplianceEvent {
		return db.ComplianceEvent{DeviceID: d, CheckID: check, To: to, Level: level, At: when}
	}
	events := []db.ComplianceEvent{
		ev(a, "locked", db.ComplianceFail, db.ComplianceRequired, at.Add(-48*time.Hour)),
		ev(a, "locked", db.CompliancePass, db.ComplianceRequired, at.Add(-24*time.Hour)), // fixed before `at`
		ev(b, "locked", db.ComplianceFail, db.ComplianceRequired, at.Add(-time.Hour)),
		ev(b, "locked", db.CompliancePass, db.ComplianceRequired, at.Add(time.Hour)), // after `at`: doesn't count
		ev(c, "locked", db.ComplianceFail, db.ComplianceRecommended, at.Add(-time.Hour)),
	}
	pct, ok := compPctFromEvents(events, at)
	if !ok || pct != 50 {
		t.Fatalf("pct = %d, %v; want 50 (a passing, b failing, c not held to a required check)", pct, ok)
	}
	if _, ok := compPctFromEvents(events, at.Add(-72*time.Hour)); ok {
		t.Error("no history that far back must give no number")
	}
}
