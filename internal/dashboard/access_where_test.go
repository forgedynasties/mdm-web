package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// withWhere is newAccess with the person's Where applied the way levels() applies it.
func withWhere(role string, w *db.AccessWhere, own ...db.AccessLevel) *access {
	a := newAccess(role, db.AccessPolicy{})
	a.lvls = db.EffectiveLevels(append(seedLevels(role), own...), w, time.Now())
	return a
}

// Fixture (access_test.go): V holds d1, d2 (in service); G holds d2 and d3; d3, d4 are Lab.
func TestWhereOnPerson(t *testing.T) {
	g := groupG
	qaOnly := &db.AccessWhere{Username: "abdullah", Scope: "groups", GroupIDs: []uuid.UUID{g}, Elsewhere: "view"}
	for _, c := range []struct {
		name   string
		a      *access
		action string
		dev    uuid.UUID
		want   bool
	}{
		// No Where: the role's defaults exactly (View all, Operate Lab and QA).
		{"default: reboot a Lab device", withWhere("engineer", nil), "reboot", d4, true},
		{"default: reboot at a venue", withWhere("engineer", nil), "reboot", d1, false},
		// Where = the QA group: Operate there, View elsewhere.
		{"groups: reboot in QA (at a venue)", withWhere("engineer", qaOnly), "reboot", d2, true},
		{"groups: reboot in QA (Lab)", withWhere("engineer", qaOnly), "reboot", d3, true},
		{"groups: Lab device outside QA is view only", withWhere("engineer", qaOnly), "reboot", d4, false},
		{"groups: still looks at it", withWhere("engineer", qaOnly), "screenshot", d4, true},
		{"groups: no Manage from an Operate role", withWhere("engineer", qaOnly), "remote", d2, false},
		// Deployed = at a venue, whatever the state.
		{"deployed: in service", withWhere("engineer", &db.AccessWhere{Scope: "deployed", Elsewhere: "view"}), "reboot", d1, true},
		{"deployed: not Lab", withWhere("engineer", &db.AccessWhere{Scope: "deployed", Elsewhere: "view"}), "reboot", d4, false},
		// The role decides what: Support (Manage) narrowed to Lab manages Lab devices only.
		{"support on Lab: remote on Lab", withWhere("operator", &db.AccessWhere{Scope: "lab", Elsewhere: "hidden"}), "remote", d4, true},
		{"support on Lab: in service hidden", withWhere("operator", &db.AccessWhere{Scope: "lab", Elsewhere: "hidden"}), "view", d1, false},
		// Their own grants still add on top.
		{"own grant adds", withWhere("engineer", qaOnly, db.AccessLevel{SubjectType: "user", ScopeType: "venue", ScopeID: &venueV, Level: "manage"}), "remote", d1, true},
	} {
		dev := c.dev
		if got := c.a.decide(c.action, &dev).Allowed; got != c.want {
			t.Errorf("%s: %s on %s = %v, want %v", c.name, c.action, dev, got, c.want)
		}
	}
}

func TestWhereElsewhereHidden(t *testing.T) {
	a := withWhere("engineer", &db.AccessWhere{Scope: "groups", GroupIDs: []uuid.UUID{groupG}, Elsewhere: "hidden"})
	if !a.hidesDevices() {
		t.Fatal("Elsewhere hidden: lists are filtered")
	}
	if a.visible(d1) || a.visible(d4) || !a.visible(d2) || !a.visible(d3) {
		t.Errorf("visible: d1 %v d2 %v d3 %v d4 %v, want only the QA devices d2 and d3",
			a.visible(d1), a.visible(d2), a.visible(d3), a.visible(d4))
	}
	if b := withWhere("engineer", &db.AccessWhere{Scope: "groups", GroupIDs: []uuid.UUID{groupG}, Elsewhere: "view"}); b.hidesDevices() {
		t.Error("Elsewhere view only: they still see every device")
	}
}

func TestWhereLocks(t *testing.T) {
	a := withWhere("engineer", &db.AccessWhere{Scope: "groups", GroupIDs: []uuid.UUID{groupG}, Elsewhere: "view"})
	if !a.lookOnly(d4) || a.lookOnly(d3) {
		t.Error("a Lab device outside QA is view only; QA's Lab device is not")
	}
	if !a.restaurantsViewOnly() {
		t.Error("QA only: the Restaurants tab is locked")
	}
	if withWhere("engineer", &db.AccessWhere{Scope: "deployed", Elsewhere: "view"}).restaurantsViewOnly() {
		t.Error("Deployed: restaurants are operable")
	}
}

func TestWhereLeavesOthersAlone(t *testing.T) {
	w := &db.AccessWhere{Scope: "groups", GroupIDs: []uuid.UUID{groupG}, Elsewhere: "hidden"}
	// Super admins aren't on levels; customers keep their venue wall.
	if !withWhere("admin", w).canDevice("shell", d4) {
		t.Error("super admin unchanged")
	}
	v := venueV
	o := newAccess("owner", db.AccessPolicy{Grants: []db.AccessGrant{allow("restaurant", &v, "view")}})
	o.lvls = db.EffectiveLevels(nil, w, time.Now())
	if !o.canDevice("view", d1) || o.canDevice("view", d3) {
		t.Error("an owner still sees their venue only")
	}
	// No Where: EffectiveLevels hands the grants back unchanged.
	in := seedLevels("engineer")
	if out := db.EffectiveLevels(in, nil, time.Now()); len(out) != len(in) {
		t.Errorf("no Where changed the grants: %d → %d", len(in), len(out))
	}
}

func TestWhereSentence(t *testing.T) {
	g := groupG
	w := &db.AccessWhere{Username: "a", Scope: "groups", GroupIDs: []uuid.UUID{g}, Elsewhere: "view"}
	gs := db.EffectiveLevels(seedLevels("engineer"), w, time.Now())
	for i := range gs {
		if gs[i].ScopeType == "group" {
			gs[i].ScopeName = "QA"
		}
	}
	if s := whereSentence("Abdullah", gs, w, time.Now()); s != "Abdullah can operate the QA group and look at every other device." {
		t.Errorf("sentence = %q", s)
	}
	w.Elsewhere = "hidden"
	gs = db.EffectiveLevels(seedLevels("engineer"), w, time.Now())
	for i := range gs {
		gs[i].ScopeName = "QA"
	}
	if s := whereSentence("Abdullah", gs, w, time.Now()); !strings.HasSuffix(s, "Other devices are hidden from Abdullah.") || strings.Contains(s, "every") {
		t.Errorf("hidden sentence = %q", s)
	}
}
