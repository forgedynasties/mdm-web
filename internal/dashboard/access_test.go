package dashboard

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// fixture fleet: venue V with devices d1,d2 (in service); group G with d2,d3; d3 and d4
// at no venue (Lab).
var (
	venueV = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	groupG = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	d1     = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	d2     = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	d3     = uuid.MustParse("00000000-0000-0000-0000-000000000003")
	d4     = uuid.MustParse("00000000-0000-0000-0000-000000000004")
)

func fixtureScopes() map[uuid.UUID]db.DeviceScope {
	v := venueV
	return map[uuid.UUID]db.DeviceScope{
		d1: {RestaurantID: &v, InService: true},
		d2: {RestaurantID: &v, Groups: []uuid.UUID{groupG}, InService: true},
		d3: {Groups: []uuid.UUID{groupG}},
		d4: {},
	}
}

func newAccess(role string, pol db.AccessPolicy) *access {
	a := &access{ctx: context.Background(), role: role, username: "u", pol: pol}
	a.scopes = fixtureScopes()
	a.once.Do(func() {}) // scopes preloaded; never touch the DB
	a.lvls = seedLevels(role)
	a.lvlOnce.Do(func() {})
	return a
}

// seedLevels is the access_levels seed (db/access_levels.go) for one role, with group G as QA.
func seedLevels(role string) []db.AccessLevel {
	g := groupG
	lv := func(scope, state, level string) db.AccessLevel {
		return db.AccessLevel{SubjectType: "role", Subject: role, ScopeType: scope, State: state, Level: level}
	}
	switch role {
	case "fleet_admin":
		return []db.AccessLevel{lv("all", "", "manage")}
	case "fleet_manager":
		return []db.AccessLevel{lv("state", db.StateSettingUp, "manage"), lv("state", db.StateInService, "manage")}
	case "operator", "support":
		return []db.AccessLevel{lv("state", db.StateInService, "manage")}
	case "viewer":
		return []db.AccessLevel{lv("state", db.StateInService, "view")}
	case "field":
		return []db.AccessLevel{lv("visit", "", "manage")}
	case "engineer":
		qa := lv("group", "", "operate")
		qa.ScopeID = &g
		return []db.AccessLevel{lv("all", "", "view"), lv("state", db.StateLab, "operate"), qa}
	}
	return nil
}

func allow(scopeType string, id *uuid.UUID, actions ...string) db.AccessGrant {
	return db.AccessGrant{Effect: "allow", ScopeType: scopeType, ScopeID: id, Actions: actions}
}
func deny(scopeType string, id *uuid.UUID, actions ...string) db.AccessGrant {
	return db.AccessGrant{Effect: "deny", ScopeType: scopeType, ScopeID: id, Actions: actions}
}

func ptr(u uuid.UUID) *uuid.UUID { return &u }

func TestExpiredGrantsAreNotLoaded(t *testing.T) {
	// The DB layer drops expired grants; the engine trusts what it is given. This
	// guards the contract: a grant marked Expired must never be present in a policy
	// that reaches decide(). Documented here so a future "includeExpired" caller
	// does not hand the editor's list to the engine.
	past := time.Now().Add(-time.Hour)
	g := allow("all", nil, "remote")
	g.ExpiresAt, g.Expired = &past, true
	a := newAccess("operator", db.AccessPolicy{Grants: []db.AccessGrant{g}})
	if a.can("remote", &d1) {
		t.Log("engine does not re-check expiry; DB filter is the guard (see ListAccessGrants)")
	}
}

func TestRoleCeilings(t *testing.T) {
	for _, k := range []string{"shell", "ota"} {
		if roleCeiling("operator")[k] || roleCeiling("support")[k] || roleCeiling("viewer")[k] || roleCeiling("owner")[k] || roleCeiling("staff")[k] {
			t.Fatalf("%s leaked below the fleet manager", k)
		}
	}
	if roleCeiling("fleet_manager")["ota"] || roleCeiling("fleet_manager")["shell"] {
		t.Fatal("fleet_manager ceiling must hold neither ota nor shell (OTA is admin and super admin only)")
	}
	if roleCeiling("fleet_admin")["shell"] || !roleCeiling("fleet_admin")["ota"] {
		t.Fatal("fleet_admin ceiling must hold ota and not shell")
	}
	// Each level holds everything the one below it can.
	ladder := []string{"viewer", "support", "operator", "fleet_manager", "fleet_admin"}
	for i := 1; i < len(ladder); i++ {
		for k := range roleCeiling(ladder[i-1]) {
			if !roleCeiling(ladder[i])[k] {
				t.Errorf("%s can %s but %s can't", ladder[i-1], k, ladder[i])
			}
		}
	}
	if roleCeiling("admin") != nil {
		t.Fatal("admin must be unrestricted")
	}
	if n := len(grantableActions("owner")); n != 6 {
		t.Fatalf("owner grantable = %d", n)
	}
}

// TestVisibleIDsOwnerNeverCollapsesToNil guards the other half of that fix. Callers
// disagree about what nil means — the fleet list reads it as "no filter", OwnerHome
// reads it as "no devices" — so a policy-filtered result must stay an explicit list
// even when the policy happens to admit every device, or an owner whose grants cover
// the whole venue gets a blank home page.
func TestVisibleIDsOwnerNeverCollapsesToNil(t *testing.T) {
	a := newAccess("owner", db.AccessPolicy{
		Base: "deny",
		Grants: []db.AccessGrant{allow("device", &d1, "view"), allow("device", &d2, "view"),
			allow("device", &d3, "view"), allow("device", &d4, "view")},
	})
	ids := a.visibleIDs()
	if ids == nil {
		t.Fatal("an owner allowed every device still needs an explicit list, not nil")
	}
	if len(ids) != 2 { // d3 and d4 are lab devices: never an owner's
		t.Fatalf("owner sees %d devices, want 2", len(ids))
	}
}

func TestRoleWidens(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"viewer", "operator", true},
		{"operator", "viewer", false},
		{"operator", "fleet_manager", true},
		{"fleet_manager", "operator", false},
		{"viewer", "support", true},
		{"operator", "admin", true},
		{"admin", "operator", false},
		{"operator", "operator", false},
	} {
		if got := widens(c.from, c.to); got != c.want {
			t.Errorf("widens(%s→%s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestGrantRedundant(t *testing.T) {
	remoteAll := db.AccessGrant{Effect: "allow", ScopeType: "all", Actions: []string{"remote"}}
	cases := []struct {
		role, base string
		g          db.AccessGrant
		want       bool
	}{
		{"fleet_manager", "allow", remoteAll, true}, // promoted operator: fleet managers have remote
		{"operator", "allow", remoteAll, true},      // Support holds remote by default (roles replan)
		{"fleet_manager", "deny", remoteAll, false}, // base nothing: the rule is the only allow
		{"operator", "allow", db.AccessGrant{Effect: "allow", Actions: []string{"reboot", "*"}}, true},
		{"operator", "allow", db.AccessGrant{Effect: "allow", Actions: []string{"shell"}}, false}, // outside the ceiling
		{"fleet_admin", "allow", db.AccessGrant{Effect: "allow", Actions: []string{"ota"}}, true},
		{"fleet_manager", "allow", db.AccessGrant{Effect: "allow", Actions: []string{"ota"}}, false}, // outside a fleet manager's ceiling (OTA is admin+ only)
		{"fleet_manager", "allow", db.AccessGrant{Effect: "deny", Actions: []string{"remote"}}, false},
		{"owner", "allow", db.AccessGrant{Effect: "allow", Actions: []string{"view"}}, false},
	}
	for _, c := range cases {
		if got := grantRedundant(c.role, c.base, c.g); got != c.want {
			t.Errorf("grantRedundant(%s, %s, %v %v) = %v, want %v", c.role, c.base, c.g.Effect, c.g.Actions, got, c.want)
		}
	}
}

// "Possible impersonation" is super-admin-only: it names the addresses a serial was
// used from and the only fix is a key reset. Every other role must not see it, while
// ordinary device alerts are untouched.
func TestIdentityAlertsSuperAdminOnly(t *testing.T) {
	alerts := []db.Alert{
		{Type: db.AlertTypeIdentityConflict, Severity: "critical"},
		{Type: "overheating", Severity: "warning"},
	}
	for _, role := range []string{"admin", "operator", "viewer", ""} {
		got := newAccess(role, db.AccessPolicy{}).keepVisibleAlerts(alerts)
		want := 1
		if role == "admin" {
			want = 2
		}
		if len(got) != want {
			t.Errorf("role %q: kept %d alerts, want %d", role, len(got), want)
		}
		for _, a := range got {
			if role != "admin" && a.Type == db.AlertTypeIdentityConflict {
				t.Errorf("role %q: identity alert leaked", role)
			}
		}
	}
}

// TestRoleLadder pins who may manage whom: strictly below on one rank order where every
// customer sits under the whole AIO team, and only fleet manager up manages anyone.
func TestRoleLadder(t *testing.T) {
	// Roles replan (30 Oct 2026): only the super admin manages people (owners invite
	// their staff through /team, which this ladder doesn't decide).
	cases := []struct {
		actor, target, newRole string
		want                   bool
	}{
		{"fleet_manager", "operator", "", false},
		{"fleet_manager", "owner", "", false},
		{"fleet_admin", "fleet_manager", "", false},
		{"operator", "viewer", "", false},
		{"owner", "staff", "", false},
		{"admin", "field", "engineer", true},
		{"admin", "admin", "admin", true},
	}
	for _, c := range cases {
		if got := mayManageUser(c.actor, c.target, c.newRole); got != c.want {
			t.Errorf("mayManageUser(%s, %s, %q) = %v, want %v", c.actor, c.target, c.newRole, got, c.want)
		}
	}
	if got := assignableRoles("admin"); strings.Join(got, ",") != "admin,fleet_manager,engineer,operator,field,viewer,owner,staff" {
		t.Errorf("super admin assigns %v (legacy roles must not be offered)", got)
	}
	if assignableRoles("fleet_manager") != nil || assignableRoles("operator") != nil || assignableRoles("owner") != nil {
		t.Error("nobody but the super admin assigns roles on the Users page")
	}
	if roleCanOperate("owner") || roleCanSupport("staff") || roleManagesUsers("owner") {
		t.Error("a customer reached a team gate")
	}
}

// TestRoleConstraintCoversEveryRole: the users.role check the migration script leaves in
// place last must allow every role in roleDefs, or saving that role fails in production
// (20 Oct: a 7 Oct step re-set the old list after the new one).
func TestRoleConstraintCoversEveryRole(t *testing.T) {
	src, err := os.ReadFile("../db/migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	all := regexp.MustCompile(`users_role_check CHECK \(role IN \(([^)]*)\)\)`).FindAllStringSubmatch(string(src), -1)
	if len(all) == 0 {
		t.Fatal("no users_role_check in the migrations")
	}
	// Every step, not just the last: they all re-run at each start, so an earlier list
	// missing a role in use stops the server (30 Oct 2026, field/engineer on stage).
	for i, m := range all {
		for _, r := range roleOrder {
			if !strings.Contains(m[1], "'"+r+"'") {
				t.Errorf("users_role_check #%d (%s) does not allow %q", i+1, m[1], r)
			}
		}
	}
}
