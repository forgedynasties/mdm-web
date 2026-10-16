package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func compState(t *testing.T, id string, s *compSnapshot, d *db.Device) (string, string) {
	t.Helper()
	c, ok := complianceCheck(id)
	if !ok {
		t.Fatalf("no check %q", id)
	}
	return c.eval(s, d, c.Param)
}

// What the clients report decides the device checks; a flag the client never sent is
// unknown, not failing (most of the fleet runs clients that report none of them).
func TestComplianceDeviceChecks(t *testing.T) {
	s := &compSnapshot{now: time.Now()}
	d := &db.Device{LatestExtra: json.RawMessage(`{"su_present":true,"dev_options_enabled":false,"adb_tcp":5555,
		"unknown_sources":true,"unknown_source_apps":["com.apkpure.aegon"],"play_protect":false,"storage_encrypted":true}`)}
	for id, want := range map[string]string{
		"not_rooted": db.ComplianceFail, "dev_options_off": db.CompliancePass, "wireless_adb_off": db.ComplianceFail,
		"unknown_sources_off": db.ComplianceFail, "play_protect": db.ComplianceFail, "encrypted": db.CompliancePass,
		"adb_off": db.ComplianceUnknown,
	} {
		if got, detail := compState(t, id, s, d); got != want {
			t.Errorf("%s: %s (%s), want %s", id, got, detail, want)
		}
	}
	if _, detail := compState(t, "unknown_sources_off", s, d); detail != "Allowed for com.apkpure.aegon" {
		t.Errorf("unknown sources detail %q", detail)
	}
	quiet := &db.Device{}
	for _, c := range complianceChecks {
		if !c.Device {
			continue
		}
		if got, _ := c.eval(s, quiet, c.Param); got != db.ComplianceUnknown {
			t.Errorf("%s on a device that reports nothing: %s, want unknown", c.ID, got)
		}
	}
}

func TestComplianceServerChecks(t *testing.T) {
	now := time.Now()
	venue := uuid.New()
	s := &compSnapshot{now: now, prodFW: map[string]string{"t7": "v2.1.031"}, lock: map[uuid.UUID]string{}, ownKey: map[uuid.UUID]bool{}}
	old := &db.Device{ID: uuid.New(), Product: "t7", BuildID: "v2.0.96l", LastSeenAt: now.Add(-30 * time.Hour)}
	cur := &db.Device{ID: uuid.New(), Product: "t7", BuildID: "v2.1.031", LastSeenAt: now.Add(-time.Hour), RestaurantID: &venue}
	s.lock[cur.ID] = "locked"
	s.lock[old.ID] = "exited"
	cases := []struct {
		check string
		d     *db.Device
		want  string
	}{
		{"firmware_approved", old, db.ComplianceFail},
		{"firmware_approved", cur, db.CompliancePass},
		{"checked_in", old, db.ComplianceFail},
		{"checked_in", cur, db.CompliancePass},
		{"locked", old, db.ComplianceFail},
		{"locked", cur, db.CompliancePass},
		{"own_key", cur, db.ComplianceFail},
	}
	for _, tc := range cases {
		if got, detail := compState(t, tc.check, s, tc.d); got != tc.want {
			t.Errorf("%s on %s: %s (%s), want %s", tc.check, tc.d.BuildID, got, detail, tc.want)
		}
	}
	s.labs = map[uuid.UUID]db.LabSession{old.ID: {}}
	if got, _ := compState(t, "firmware_approved", s, old); got != db.ComplianceNA {
		t.Errorf("a device in a lab session is measured for firmware: %s", got)
	}
}

// The first covering override that names a check (or caps every check) decides; the
// baseline otherwise.
func TestComplianceResolution(t *testing.T) {
	venue, group := uuid.New(), uuid.New()
	adb, _ := complianceCheck("adb_off")
	seen, _ := complianceCheck("checked_in")
	r := compRules{
		baseline: map[string]db.ComplianceBaselineRow{"checked_in": {Setting: db.ComplianceSetting{Param: 12}}},
		overrides: []db.ComplianceOverride{
			{ScopeType: "lab", Cap: db.ComplianceRecommended},
			{ScopeType: "group", ScopeID: &group, Checks: map[string]db.ComplianceSetting{"adb_off": {Level: db.ComplianceOff}}},
			{ScopeType: "venue", ScopeID: &venue, Checks: map[string]db.ComplianceSetting{"checked_in": {Param: 2}}},
		},
	}
	atVenue := &db.Device{RestaurantID: &venue}
	if set, _ := r.settingFor(adb, atVenue, db.DeviceScope{}, false); set.Level != db.ComplianceRequired {
		t.Errorf("adb at a venue: %+v, want the baseline's required", set)
	}
	if set, _ := r.settingFor(seen, atVenue, db.DeviceScope{}, false); set.Param != 2 || set.Level != db.ComplianceRequired {
		t.Errorf("checked_in at the venue: %+v, want 2 hours, required", set)
	}
	if set, _ := r.settingFor(seen, &db.Device{RestaurantID: &venue}, db.DeviceScope{Groups: []uuid.UUID{group}}, true); set.Level != db.ComplianceRecommended || set.Param != 12 {
		t.Errorf("in a lab session: %+v, want capped to recommended at the baseline's 12 hours", set)
	}
	if set, _ := r.settingFor(adb, atVenue, db.DeviceScope{Groups: []uuid.UUID{group}}, false); set.Level != db.ComplianceOff {
		t.Errorf("adb in the group: %+v, want off", set)
	}
	if set, _ := r.settingFor(adb, &db.Device{}, db.DeviceScope{}, false); set.Level != db.ComplianceRequired {
		t.Errorf("adb with no override covering it: %+v, want the baseline's required", set)
	}
}

func TestComplianceVerdict(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	res := map[db.ComplianceResultKey]db.ComplianceResult{
		{DeviceID: a, CheckID: "x"}: {Level: db.ComplianceRequired, State: db.ComplianceUnknown},
		{DeviceID: a, CheckID: "y"}: {Level: db.ComplianceRecommended, State: db.ComplianceFail},
		{DeviceID: b, CheckID: "x"}: {Level: db.ComplianceRequired, State: db.ComplianceFail},
	}
	v := compCompliant(res)
	if !v[a] || v[b] {
		t.Errorf("verdict %v: unknown and recommended failures must not break compliance; a required failure must", v)
	}
}

// Devices with no venue are not scored at all (15 Oct).
func TestComplianceVenueOnly(t *testing.T) {
	venue := uuid.New()
	s := &compSnapshot{now: time.Now(), lock: map[uuid.UUID]string{}, ownKey: map[uuid.UUID]bool{},
		devices: []db.Device{{ID: uuid.New(), RestaurantID: &venue}, {ID: uuid.New()}}}
	res := compEvaluate(s, compRules{})
	seen := map[uuid.UUID]bool{}
	for k := range res {
		seen[k.DeviceID] = true
	}
	if !seen[s.devices[0].ID] || seen[s.devices[1].ID] {
		t.Errorf("scored %v; want only the device at a venue", seen)
	}
}
