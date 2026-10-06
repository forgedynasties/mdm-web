package dashboard

import (
	"testing"

	"mdm/internal/db"

	"github.com/google/uuid"
)

func TestDeriveSimple(t *testing.T) {
	look := db.AccessProfile{ID: uuid.New(), Name: "Look only", Actions: []string{"view"}, Builtin: true}
	profiles := []db.AccessProfile{look}
	rid := uuid.New()
	venue := db.AccessGrant{Effect: "allow", ScopeType: "restaurant", ScopeID: &rid, ScopeName: "Flights Vegas", Actions: []string{"view"}}
	all := func(acts ...string) db.AccessGrant { return db.AccessGrant{Effect: "allow", ScopeType: "all", Actions: acts} }
	cases := []struct {
		name, role string
		pol        db.AccessPolicy
		ok, every  bool
		do         string
	}{
		{"default", "operator", db.AccessPolicy{}, true, true, "role"},
		{"super op, redundant remote", "super_op", db.AccessPolicy{Grants: []db.AccessGrant{all("remote")}}, true, true, "role"},
		{"operator with remote on top", "operator", db.AccessPolicy{Grants: []db.AccessGrant{all("remote")}}, false, false, ""},
		{"venue look only", "super_op", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{venue}}, true, false, "profile:" + look.ID.String()},
		{"nothing", "dev", db.AccessPolicy{Base: "deny"}, true, false, "profile:" + look.ID.String()},
		{"deny rule", "operator", db.AccessPolicy{Grants: []db.AccessGrant{{Effect: "deny", ScopeType: "all", Actions: []string{"reboot"}}}}, false, false, ""},
		{"everything, place-limited", "dev", db.AccessPolicy{Base: "deny", Grants: []db.AccessGrant{{Effect: "allow", ScopeType: "restaurant", ScopeID: &rid, Actions: []string{"*", "remote", "shell", "ota"}}}}, true, false, "role"},
	}
	for _, c := range cases {
		s := deriveSimple(c.role, c.pol, profiles, nil)
		if s.OK != c.ok || (c.ok && (s.Every != c.every || s.Do != c.do)) {
			t.Errorf("%s: got ok=%v every=%v do=%q why=%q", c.name, s.OK, s.Every, s.Do, s.Why)
		}
	}
}
