package dashboard

import (
	"sort"
	"strings"
	"testing"

	"mdm/internal/db"
	"mdm/internal/product"
)

// wantCommandRoles is the role allowlist as it stood before it was derived from the
// command catalogue, written out so a change to a role set has to be deliberate.
// This map is a security gate: widening it silently is how a low-privilege account
// ends up able to wipe a device, and narrowing it is how a button that is offered
// stops working.
var wantCommandRoles = map[string][]string{
	"screenshot":       {"admin", "dev", "operator", "user_manager", "super_op", "viewer"},
	"install_apk":      {"admin", "dev", "operator", "user_manager", "super_op"},
	"uninstall":        {"admin", "dev", "operator", "user_manager", "super_op"},
	"reboot":           {"admin", "dev", "operator", "user_manager", "super_op"},
	"app_reload":       {"admin", "dev", "operator", "user_manager", "super_op"},
	"app_restart":      {"admin", "dev", "operator", "user_manager", "super_op"},
	"app_clear_cache":  {"admin", "dev", "operator", "user_manager", "super_op"},
	"app_update_check": {"admin", "dev", "operator", "user_manager", "super_op"},
	"app_update":       {"admin", "dev", "operator", "user_manager", "super_op"},
	"shell":            {"admin", "dev"},
	"query":            {"admin", "dev", "operator", "user_manager", "super_op"},
	"ota":              {"admin", "dev", "super_op"},
	"update_splash":    {"admin", "dev"},
	"logcat":           {"admin", "dev"},
	"wipe":             {"admin"},
	"mic_gain_read":    {"admin"},
}

func TestCommandRolesMatchTheAllowlist(t *testing.T) {
	if len(commandRoles) != len(wantCommandRoles) {
		t.Errorf("commandRoles has %d types, want %d: %s", len(commandRoles), len(wantCommandRoles), strings.Join(sortedKeys(commandRoles), ", "))
	}
	for typ, want := range wantCommandRoles {
		got, ok := commandRoles[typ]
		if !ok {
			t.Errorf("commandRoles is missing %q", typ)
			continue
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("commandRoles[%q] = %v, want %v", typ, got, want)
		}
	}
	for typ := range commandRoles {
		if _, ok := wantCommandRoles[typ]; !ok {
			t.Errorf("commandRoles gained %q (%v) — a new sendable type is a policy change", typ, commandRoles[typ])
		}
	}
}

// TestEveryCommandIsEnforceable is the guard the four drifting lists were missing.
//
// A command is only reachable if BOTH gates open: the role allowlist here, and — for
// anyone but an admin — the access policy, which is asked about the action key
// policyActionForCommand returns. decide() answers "Unknown action" for a key that is
// not in accessActions, and that is a denial, so a type can be offered by every
// surface in the dashboard and refused at the last step. app_update (operators, on
// the device page, the Clients page and "Update all behind") and update_splash (devs,
// on the boot-logo form) were exactly that. Either every sendable type names a real
// action, or it is admin-only and appears in the catalogue's documented residue.
func TestEveryCommandIsEnforceable(t *testing.T) {
	residue := product.AdminOnlyCommandTypes()
	for _, c := range product.Commands() {
		if c.Roles == nil {
			continue // not a dashboard command: a probe, a session, or closed
		}
		action := policyActionForCommand(c.Type)
		if residue[c.Type] {
			// Admin-only by allowlist, and an admin never reaches decide().
			if isAccessAction(action) {
				t.Errorf("%q now names the real access action %q — remove it from product.accessActionsAdminOnly", c.Type, action)
			}
			continue
		}
		if !isAccessAction(action) {
			t.Errorf("command %q is sendable by %v but judged by access action %q, which does not exist: decide() denies it as \"Unknown action\", so no non-admin can ever send it",
				c.Type, c.Roles, action)
		}
	}
}

// TestCommandPolicyActions pins the two mappings that were added to make the reachable
// commands above reachable, since both are a judgement about which grant covers them.
func TestCommandPolicyActions(t *testing.T) {
	cases := map[string]string{
		// The agent updating itself is an install from the app library; operators
		// already hold install_apk. This is what unblocks the Clients page.
		"app_update": "install_apk",
		// Writing the boot logo is a system-partition write, judged like the other
		// firmware push: dev ceiling, sensitive, never granted to operators.
		"update_splash": "ota",
		"app_reload":    "app_control",
		"set_kiosk":     "kiosk",
		"reboot":        "reboot",
	}
	for typ, want := range cases {
		if got := policyActionForCommand(typ); got != want {
			t.Errorf("policyActionForCommand(%q) = %q, want %q", typ, got, want)
		}
	}
}

// TestNonAdminCanReachTheOfferedCommands is the end-to-end version of the guard
// above: for the roles each type is offered to, the command must actually be issuable
// on a device inside the role's own ceiling and default policy. It fails loudly if a
// future change reintroduces a button that answers 403.
func TestNonAdminCanReachTheOfferedCommands(t *testing.T) {
	// An operator with the default policy (base allow, no grants) on a device.
	a := newAccess("operator", db.AccessPolicy{})
	for _, c := range product.Commands() {
		if c.Roles == nil || !contains(c.Roles, "operator") {
			continue
		}
		if residue := product.AdminOnlyCommandTypes()[c.Type]; residue {
			continue
		}
		d := a.decide(policyActionForCommand(c.Type), &d1)
		if !d.Allowed {
			t.Errorf("an operator is offered %q (%v) but the policy refuses it: %s", c.Type, c.Roles, d.Reason)
		}
	}
	// A dev, likewise, for the dev-level types.
	dv := newAccess("dev", db.AccessPolicy{})
	for _, typ := range []string{"update_splash", "shell", "ota"} {
		if d := dv.decide(policyActionForCommand(typ), &d1); !d.Allowed {
			t.Errorf("a dev is offered %q but the policy refuses it: %s", typ, d.Reason)
		}
	}
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
