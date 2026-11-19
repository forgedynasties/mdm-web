package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Fixture (access_test.go): venue V holds d1 and d2, both in service; group G (the QA group
// here) holds d2 and d3; d3 and d4 are to place.

func TestLevelsByRoleAndState(t *testing.T) {
	cases := []struct {
		role, action string
		dev          uuid.UUID
		want         bool
	}{
		// QA fixture: View everywhere, Operate on devices to place and on QA (which reaches d2 at a venue).
		{qaFixture, "view", d1, true},
		{qaFixture, "screenshot", d1, true},
		{qaFixture, "query", d1, true},
		{qaFixture, "reboot", d1, false},
		{qaFixture, "reboot", d2, true},
		{qaFixture, "reboot", d3, true},
		{qaFixture, "reboot", d4, true},
		{qaFixture, "remote", d3, false}, // Manage; to place is Operate
		{qaFixture, "wifi", d4, false},
		{qaFixture, "shell", d3, false}, // super admins only
		// Support: Manage in service, nothing else.
		{"operator", "remote", d1, true},
		{"operator", "reboot", d2, true},
		{"operator", "view", d3, false},
		// Viewer: View in service, but its ceiling has no screenshot.
		{"viewer", "view", d1, true},
		{"viewer", "screenshot", d1, false},
		{"viewer", "view", d4, false},
		// Ops lead: to place and in service.
		{"fleet_manager", "remote", d1, true},
		{"fleet_manager", "view", d4, true},
		// Admin (old): everything but the shell; firmware updates too.
		{"fleet_admin", "ota", d4, true},
		{"fleet_admin", "shell", d4, false},
		// Super admin.
		{"admin", "shell", d1, true},
	}
	for _, c := range cases {
		dev := c.dev
		d := newAccess(c.role, db.AccessPolicy{}).decide(c.action, &dev)
		if d.Allowed != c.want {
			t.Errorf("%s %s on %s: got %v (%s), want %v", c.role, c.action, dev, d.Allowed, d.Reason, c.want)
		}
	}
}

func TestPersonGrantsAddAndExpire(t *testing.T) {
	v := venueV
	future, past := time.Now().Add(48*time.Hour), time.Now().Add(-time.Hour)
	a := newAccess(qaFixture, db.AccessPolicy{})
	a.lvls = append(a.lvls, db.AccessLevel{SubjectType: "user", Subject: "u", ScopeType: "venue", ScopeID: &v, Level: "manage", Until: &future})
	if !a.canDevice("remote", d1) {
		t.Error("a person's Manage on venue V should give remote control on d1")
	}
	b := newAccess(qaFixture, db.AccessPolicy{})
	b.lvls = append(b.lvls, db.AccessLevel{SubjectType: "user", Subject: "u", ScopeType: "venue", ScopeID: &v, Level: "manage", Until: &past})
	if b.canDevice("reboot", d1) {
		t.Error("an expired grant must not count")
	}
	if !b.canDevice("view", d1) {
		t.Error("the role's View everywhere still applies")
	}
}

func TestLevelVisibility(t *testing.T) {
	if newAccess(qaFixture, db.AccessPolicy{}).hidesDevices() {
		t.Error("engineers view everything: their lists are not filtered")
	}
	sup := newAccess("operator", db.AccessPolicy{})
	if !sup.hidesDevices() {
		t.Fatal("support sees devices in service only: lists are filtered")
	}
	ids := sup.visibleIDs()
	if len(ids) != 2 {
		t.Errorf("support sees %v, want d1 and d2", ids)
	}
	// A person's grant widens visibility.
	g := groupG
	v := newAccess("viewer", db.AccessPolicy{})
	v.lvls = append(v.lvls, db.AccessLevel{SubjectType: "user", Subject: "u", ScopeType: "group", ScopeID: &g, Level: "view"})
	if !v.visible(d3) {
		t.Error("a viewer given View on group G sees d3 (Lab)")
	}
}

// Field manages the devices they enrolled until they are placed, and nothing else; once
// placed (at a venue, in service) a device is out of their reach (19 Nov 2026).
func TestFieldOwnLevel(t *testing.T) {
	v := venueV
	mine, theirs, placed := uuid.New(), uuid.New(), uuid.New()
	a := newAccess("field", db.AccessPolicy{})
	a.username = "kevin"
	a.scopes = map[uuid.UUID]db.DeviceScope{
		mine:   {EnrolledBy: "kevin", Placement: db.PlacementQueue},
		theirs: {EnrolledBy: "someone", Placement: db.PlacementQueue},
		placed: {EnrolledBy: "kevin", RestaurantID: &v},
	}
	if !a.canDevice("remote", mine) || a.canDevice("view", theirs) || a.canDevice("view", placed) {
		t.Error("field manages only the unplaced devices they enrolled")
	}
}

func TestCustomersUnchanged(t *testing.T) {
	v := venueV
	o := newAccess("owner", db.AccessPolicy{Grants: []db.AccessGrant{allow("restaurant", &v, "view")}})
	if !o.canDevice("view", d1) || o.canDevice("view", d3) {
		t.Error("an owner sees their venue's devices only")
	}
	if !o.canDevice("reboot", d1) {
		t.Error("an owner reboots their venue's devices in service")
	}
	if !o.hidesDevices() {
		t.Error("customer lists are always filtered")
	}
}

func TestLookOnlyAndLocks(t *testing.T) {
	a := newAccess(qaFixture, db.AccessPolicy{})
	if !a.lookOnly(d1) || a.lookOnly(d2) || a.lookOnly(d3) {
		t.Error("engineer: d1 view only; d2 (QA) and d3 (Lab) operable")
	}
	devs := []db.Device{{ID: d1}, {ID: d2}, {ID: d4}}
	a.markViewOnly(devs)
	if !devs[0].ViewOnly || devs[1].ViewOnly || devs[2].ViewOnly {
		t.Errorf("view-only marks = %v %v %v, want true false false", devs[0].ViewOnly, devs[1].ViewOnly, devs[2].ViewOnly)
	}
	// Venue V has d2 (QA) to operate, so no lock; a group holding only d1 is locked.
	h := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	sc := a.scopes[d1]
	sc.Groups = append(sc.Groups, h)
	a.scopes[d1] = sc
	venues, groups := a.collectionLocks()
	if venues[venueV.String()] {
		t.Error("venue V holds a device engineers operate: no lock")
	}
	if !groups[h.String()] || groups[groupG.String()] {
		t.Errorf("locked groups = %v, want only the d1-only group", groups)
	}
	if !a.restaurantsViewOnly() {
		t.Error("engineers only look at restaurants (QA reaching a venue device doesn't change that)")
	}
	if newAccess("operator", db.AccessPolicy{}).restaurantsViewOnly() || newAccess("admin", db.AccessPolicy{}).restaurantsViewOnly() {
		t.Error("support and super admins operate restaurant devices")
	}
	if v, g := newAccess("admin", db.AccessPolicy{}).collectionLocks(); len(v)+len(g) != 0 {
		t.Error("super admins see no locks")
	}
}

func TestAccessSentence(t *testing.T) {
	g := groupG
	gs := seedLevels(qaFixture)
	gs[2].ScopeName = "QA"
	_ = g
	s := accessSentence("QA testers", "They", gs, time.Now())
	for _, want := range []string{"QA testers can operate devices to place and the QA group, and look at every other device."} {
		if !strings.Contains(s, want) {
			t.Errorf("sentence %q lacks %q", s, want)
		}
	}
}

func TestParseScope(t *testing.T) {
	id := uuid.New()
	for in, want := range map[string]string{
		"all": "all", "state:to_place": "state", "own": "own", "group:" + id.String(): "group", "venue:" + id.String(): "venue",
	} {
		if st, _, _, ok := parseScope(in); !ok || st != want {
			t.Errorf("parseScope(%q) = %q, %v", in, st, ok)
		}
	}
	for _, bad := range []string{"", "state:nope", "group:x", "all:1", "device:1", "visit", "state:setting_up", "state:lab"} {
		if _, _, _, ok := parseScope(bad); ok {
			t.Errorf("parseScope(%q) accepted", bad)
		}
	}
}
