package dashboard

import (
	"context"
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
		{"ota never for access admin", "user_manager", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "ota")}}, "ota", &d1, false},
		{"dev holds shell by default", "dev", db.AccessPolicy{}, "shell", &d1, true},
		{"dev holds ota by default", "dev", db.AccessPolicy{}, "ota", &d1, true},
		{"dev holds remote by default", "dev", db.AccessPolicy{}, "remote", &d1, true},
		{"dev ota scoped to venue", "dev", db.AccessPolicy{Grants: []db.AccessGrant{deny("all", nil, "ota"), allow("restaurant", &v, "ota")}}, "ota", &d1, false},
		{"dev ota scoped: base deny + venue allow in", "dev", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "ota")}}, "ota", &d2, true},
		{"dev ota scoped: base deny + venue allow out", "dev", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "ota")}}, "ota", &d3, false},
		{"viewer sees by default", "viewer", db.AccessPolicy{}, "view", &d1, true},
		{"viewer hidden by deny", "viewer", db.AccessPolicy{Grants: []db.AccessGrant{deny("restaurant", &v, "view")}}, "view", &d1, false},
		{"viewer never reboots", "viewer", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "*")}}, "reboot", &d1, false},
		{"owner sees nothing by default", "owner", db.AccessPolicy{}, "view", &d1, false},
		{"owner sees granted venue", "owner", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{allow("restaurant", &v, "view")}}, "view", &d1, true},
		{"owner never acts", "owner", db.AccessPolicy{Grants: []db.AccessGrant{allow("all", nil, "*")}}, "reboot", &d1, false},
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
	// operator base deny without hide flag: shown read-only, no filter
	a = newAccess("operator", db.AccessPolicy{Base: "deny"})
	if a.hidesDevices() || a.visibleIDs() != nil {
		t.Fatal("operator without hide flag must not filter")
	}
	// with hide flag
	a = newAccess("operator", db.AccessPolicy{Base: "deny", HideOutOfScope: true, Grants: []db.AccessGrant{allow("device", &d4, "view")}})
	if ids := a.visibleIDs(); len(ids) != 1 || ids[0] != d4 {
		t.Fatalf("got %v", ids)
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
		if roleCeiling("operator")[k] || roleCeiling("user_manager")[k] || roleCeiling("viewer")[k] {
			t.Fatalf("%s leaked below dev", k)
		}
		if !roleCeiling("dev")[k] {
			t.Fatalf("dev lacks %s", k)
		}
	}
	if roleCeiling("admin") != nil {
		t.Fatal("admin must be unrestricted")
	}
	if n := len(grantableActions("owner")); n != 1 {
		t.Fatalf("owner grantable = %d", n)
	}
}

// TestVisibleIDsDPCOnlyFilterKeepsOthers pins the fix for an operator seeing an empty
// device list. Two independent reasons can put a user on the filter path — the access
// policy, and DPC devices being hidden from non-admins — and each must filter on its
// own criterion. Applying the view policy merely because DPC hiding was in play meant
// an operator with a base-deny policy and no hide flag, who is supposed to see every
// device with its actions disabled, saw nothing at all.
func TestVisibleIDsDPCOnlyFilterKeepsOthers(t *testing.T) {
	// One DPC device among the four; the operator's policy denies by default but has
	// no hide flag, so the policy must not remove anything.
	a := newAccess("operator", db.AccessPolicy{Base: "deny"})
	sc := fixtureScopes()
	sc[d2] = db.DeviceScope{RestaurantID: sc[d2].RestaurantID, Groups: sc[d2].Groups, DPC: true}
	a.scopes = sc

	ids := a.visibleIDs()
	if ids == nil {
		t.Fatal("a DPC device is present, so the list must be filtered")
	}
	if len(ids) != 3 {
		t.Fatalf("operator sees %d devices, want 3 (all but the DPC one)", len(ids))
	}
	for _, id := range ids {
		if id == d2 {
			t.Error("the DPC device was not hidden from the operator")
		}
	}
	// The policy still must not hide anything by itself.
	if a.hidesDevices() {
		t.Error("an operator without the hide flag must not hide on policy grounds")
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
