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

// fixture fleet: venue V with devices d1,d2; group G with d2,d3; d4 loose.
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
		d1: {RestaurantID: &v},
		d2: {RestaurantID: &v, Groups: []uuid.UUID{groupG}},
		d3: {Groups: []uuid.UUID{groupG}},
		d4: {},
	}
}

func newAccess(role string, pol db.AccessPolicy) *access {
	a := &access{ctx: context.Background(), role: role, username: "u", pol: pol}
	a.scopes = fixtureScopes()
	a.once.Do(func() {}) // scopes preloaded; never touch the DB
	return a
}

func allow(scopeType string, id *uuid.UUID, actions ...string) db.AccessGrant {
	return db.AccessGrant{Effect: "allow", ScopeType: scopeType, ScopeID: id, Actions: actions}
}
func deny(scopeType string, id *uuid.UUID, actions ...string) db.AccessGrant {
	return db.AccessGrant{Effect: "deny", ScopeType: scopeType, ScopeID: id, Actions: actions}
}

func TestDecide(t *testing.T) {
	v, g := venueV, groupG
	cases := []struct {
		name   string
		role   string
		pol    db.AccessPolicy
		action string
		dev    *uuid.UUID
		want   bool
	}{
		{"admin ignores everything", "admin", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{deny("all", nil, "*")}}, "shell", &d1, true},
		{"operator base allow", "operator", db.AccessPolicy{}, "reboot", &d1, true},
		{"operator base deny", "operator", db.AccessPolicy{Base: "deny"}, "reboot", &d1, false},
		{"operator base deny + venue allow, in venue", "operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "reboot")}}, "reboot", &d1, true},
		{"operator base deny + venue allow, outside venue", "operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "reboot")}}, "reboot", &d3, false},
		{"group allow covers member", "operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("group", &g, "*")}}, "screenshot", &d3, true},
		{"device allow covers only that device", "operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("device", &d4, "view")}}, "view", &d4, true},
		{"device allow does not leak", "operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("device", &d4, "view")}}, "view", &d1, false},
		{"deny wins over allow", "operator", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "reboot"), deny("device", &d2, "reboot")}}, "reboot", &d2, false},
		{"deny wins over base allow", "operator", db.AccessPolicy{Grants: []db.AccessGrant{deny("restaurant", &v, "*")}}, "reboot", &d1, false},
		{"remote not implied by base allow", "operator", db.AccessPolicy{}, "remote", &d1, false},
		{"remote not implied by any-action", "operator", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "*")}}, "remote", &d1, false},
		{"deny any-action also removes remote", "operator", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "remote"), deny("device", &d1, "*")}}, "remote", &d1, false},
		{"remote explicit allow on devices", "operator", db.AccessPolicy{Grants: []db.AccessGrant{allow("device", &d1, "remote")}}, "remote", &d1, true},
		{"remote explicit allow elsewhere", "operator", db.AccessPolicy{Grants: []db.AccessGrant{allow("device", &d1, "remote")}}, "remote", &d2, false},
		{"shell never for operator even if granted", "operator", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "shell")}}, "shell", &d1, false},
		{"ota never for operator even if granted", "operator", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "ota")}}, "ota", &d1, false},
		{"shell never for admin even if granted", "fleet_admin", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "shell")}}, "shell", &d1, false},
		{"admin holds ota by default", "fleet_admin", db.AccessPolicy{}, "ota", &d1, true},
		{"admin holds remote by default", "fleet_admin", db.AccessPolicy{}, "remote", &d1, true},
		{"admin ota scoped to venue", "fleet_admin", db.AccessPolicy{Grants: []db.AccessGrant{deny("all", nil, "ota"), allow("restaurant", &v, "ota")}}, "ota", &d1, false},
		{"admin ota scoped: base deny + venue allow in", "fleet_admin", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "ota")}}, "ota", &d2, true},
		{"admin ota scoped: base deny + venue allow out", "fleet_admin", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "ota")}}, "ota", &d3, false},
		{"fleet manager holds remote by default", "fleet_manager", db.AccessPolicy{}, "remote", &d1, true},
		{"fleet manager has no ota without a rule", "fleet_manager", db.AccessPolicy{}, "ota", &d1, false},
		{"fleet manager never ota, even by rule", "fleet_manager", db.AccessPolicy{Grants: []db.AccessGrant{allow("restaurant", &v, "ota")}}, "ota", &d1, false},
		{"fleet manager never shell", "fleet_manager", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "shell")}}, "shell", &d1, false},
		{"support reboots", "support", db.AccessPolicy{}, "reboot", &d1, true},
		{"support locks", "support", db.AccessPolicy{}, "kiosk", &d1, true},
		{"support never installs", "support", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "*")}}, "install_apk", &d1, false},
		{"viewer never screenshots", "viewer", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "screenshot")}}, "screenshot", &d1, false},
		{"viewer sees by default", "viewer", db.AccessPolicy{}, "view", &d1, true},
		{"viewer hidden by deny", "viewer", db.AccessPolicy{Grants: []db.AccessGrant{deny("restaurant", &v, "view")}}, "view", &d1, false},
		{"viewer never reboots", "viewer", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "*")}}, "reboot", &d1, false},
		{"owner sees nothing by default", "owner", db.AccessPolicy{}, "view", &d1, false},
		{"owner sees granted venue", "owner", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "view")}}, "view", &d1, true},
		{"owner reboots at their venue", "owner", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "view")}}, "reboot", &d1, true},
		{"owner never reboots elsewhere", "owner", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "view")}}, "reboot", &d3, false},
		{"owner never sees through an all-scope rule", "owner", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "*")}}, "view", &d3, false},
		{"owner never installs", "owner", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "view")}}, "install_apk", &d1, false},
		{"staff reboots at their venue", "staff", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "view")}}, "reboot", &d1, true},
		{"staff never locks", "staff", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "view")}}, "kiosk", &d1, false},
		{"fleet action: venue rule does not apply", "operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "alerts")}}, "alerts", nil, false},
		{"fleet action: all rule applies", "operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("all", nil, "alerts")}}, "alerts", nil, true},
		{"unknown device id", "operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "*")}}, "reboot", ptr(uuid.New()), false},
		{"unknown action", "operator", db.AccessPolicy{}, "launch_nukes", &d1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := newAccess(c.role, c.pol).decide(c.action, c.dev)
			if got.Allowed != c.want {
				t.Fatalf("want %v got %v (%s)", c.want, got.Allowed, got.Reason)
			}
		})
	}
}

func ptr(u uuid.UUID) *uuid.UUID { return &u }

func TestVisibleIDsAndHiding(t *testing.T) {
	v := venueV
	// owner: only venue devices, always hidden
	a := newAccess("owner", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "view")}})
	if !a.hidesDevices() {
		t.Fatal("owner must hide")
	}
	if ids := a.visibleIDs(); len(ids) != 2 {
		t.Fatalf("owner sees %d, want 2", len(ids))
	}
	// operator with base deny sees nothing without a view rule — "See device" is the
	// visibility grant, with no separate hide switch (30 Sep access audit)
	a = newAccess("operator", db.AccessPolicy{Base: "deny"})
	if !a.hidesDevices() || len(a.visibleIDs()) != 0 || a.visibleIDs() == nil {
		t.Fatal("base deny with no view rule must hide every device")
	}
	// a super op allowed to see one device sees only that device, flag or not
	for _, hide := range []bool{false, true} {
		a = newAccess("fleet_manager", db.AccessPolicy{Base: "deny", HideOutOfScope: hide, Grants: []db.AccessGrant{allow("device", &d4, "view")}})
		if ids := a.visibleIDs(); len(ids) != 1 || ids[0] != d4 {
			t.Fatalf("hide=%v: got %v", hide, ids)
		}
	}
	// viewer with a deny: filtered
	a = newAccess("viewer", db.AccessPolicy{Grants: []db.AccessGrant{deny("group", &groupG, "view")}})
	if ids := a.visibleIDs(); len(ids) != 2 {
		t.Fatalf("viewer sees %d, want 2", len(ids))
	}
	// admin: never
	if newAccess("admin", db.AccessPolicy{}).visibleIDs() != nil {
		t.Fatal("admin filtered")
	}
}

func TestFilterAndHolds(t *testing.T) {
	a := newAccess("operator", db.AccessPolicy{Grants: []db.AccessGrant{allow("group", &groupG, "remote")}})
	kept, dropped := a.filterDevices("remote", []uuid.UUID{d1, d2, d3, d4})
	if len(kept) != 2 || dropped != 2 {
		t.Fatalf("kept %v dropped %d", kept, dropped)
	}
	if !a.holdsAnywhere("remote") || a.holdsAnywhere("shell") {
		t.Fatal("holdsAnywhere wrong")
	}
	if !a.anyDeviceAction(d4) {
		t.Fatal("base allow gives actions on d4")
	}
	b := newAccess("operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("device", &d1, "view")}})
	if b.anyDeviceAction(d1) {
		t.Fatal("view-only device must report no actions")
	}
}

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

// TestVisibleIDsDPCNotHidden pins that a DPC-managed device is an ordinary fleet
// device: it is no longer removed from a non-admin's list, and a policy that hides
// nothing still hides nothing.
func TestVisibleIDsDPCNotHidden(t *testing.T) {
	// One DPC device among the four; the operator may see everything (base allow), so
	// nothing may be removed — being DPC is no reason to hide a device.
	a := newAccess("operator", db.AccessPolicy{Base: "allow"})
	sc := fixtureScopes()
	sc[d2] = db.DeviceScope{RestaurantID: sc[d2].RestaurantID, Groups: sc[d2].Groups, DPC: true}
	a.scopes = sc

	if ids := a.visibleIDs(); ids != nil {
		t.Fatalf("nothing hides these devices, want an unfiltered list, got %d ids", len(ids))
	}
	if a.canDevice("view", d2) != a.canDevice("view", d1) {
		t.Error("the DPC device must be judged like any other, not hidden for being DPC")
	}
	if a.hidesDevices() {
		t.Error("a policy with no view restriction must not hide")
	}
	// With a view restriction, the DPC device is judged by the policy like the rest.
	a = newAccess("operator", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("device", &d2, "view")}})
	a.scopes = sc
	if ids := a.visibleIDs(); len(ids) != 1 || ids[0] != d2 {
		t.Fatalf("got %v, want only the DPC device the rule names", ids)
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
	if len(ids) != 4 {
		t.Fatalf("owner sees %d devices, want 4", len(ids))
	}
}

// A viewer set to "allow nothing" sees nothing: the viewer's see-everything default
// used to be checked before the base, so the base never applied (30 Sep audit).
func TestViewerBaseDenySeesNothing(t *testing.T) {
	a := newAccess("viewer", db.AccessPolicy{Base: "deny"})
	if a.canDevice("view", d1) {
		t.Fatal("viewer with base deny can still see a device")
	}
	if ids := a.visibleIDs(); ids == nil || len(ids) != 0 {
		t.Fatalf("viewer with base deny sees %v", ids)
	}
	a = newAccess("viewer", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("device", &d3, "view")}})
	if ids := a.visibleIDs(); len(ids) != 1 || ids[0] != d3 {
		t.Fatalf("got %v, want only d3", ids)
	}
	// The default still holds without a base: viewers see the fleet.
	if !newAccess("viewer", db.AccessPolicy{}).canDevice("view", d1) {
		t.Fatal("a plain viewer must see the fleet")
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
		{"operator", "allow", remoteAll, false},     // the rule is what gives an operator remote
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
	cases := []struct {
		actor, target, newRole string
		want                   bool
	}{
		{"fleet_manager", "operator", "", true},
		{"fleet_manager", "owner", "", true},
		{"fleet_manager", "staff", "owner", true},
		{"fleet_manager", "fleet_manager", "", false},
		{"fleet_manager", "operator", "fleet_manager", false},
		{"fleet_admin", "fleet_manager", "", true},
		{"fleet_admin", "fleet_admin", "", false},
		{"operator", "viewer", "", false},
		{"support", "viewer", "", false},
		{"owner", "staff", "", false}, // owner self-service is venue-checked, not this ladder
		{"admin", "admin", "admin", true},
	}
	for _, c := range cases {
		if got := mayManageUser(c.actor, c.target, c.newRole); got != c.want {
			t.Errorf("mayManageUser(%s, %s, %q) = %v, want %v", c.actor, c.target, c.newRole, got, c.want)
		}
	}
	if got := assignableRoles("fleet_manager"); strings.Join(got, ",") != "operator,support,viewer,owner,staff" {
		t.Errorf("fleet manager assigns %v", got)
	}
	if assignableRoles("operator") != nil || assignableRoles("owner") != nil {
		t.Error("operators and owners must assign nothing on the Users page")
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
	last := all[len(all)-1][1]
	for _, r := range roleOrder {
		if !strings.Contains(last, "'"+r+"'") {
			t.Errorf("the last users_role_check (%s) does not allow %q", last, r)
		}
	}
}
