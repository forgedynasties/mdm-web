package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"

	"mdm/internal/product"
)

// commandRoles is the dashboard's view of the command vocabulary: which roles may
// issue (or resend) each type, and which types it cannot send at all. It is derived
// from the command catalogue in internal/product/commands.go, which describes a type
// once — its roles, the capability a device must advertise, what the admin API
// accepts, and the access-policy action it is judged by. Those facts used to be four
// lists that drifted apart; commands_test.go fails when what comes out of the
// catalogue cannot be enforced here.
//
// A type absent from this map is unknown, and unknown is rejected outright — this is
// what closes the previous default-allow gap where any arbitrary string (including
// destructive verbs) was accepted, persisted, and dispatched from the lowest-
// privilege role (GB-01/GB-02).
var commandRoles = func() map[string][]string {
	m := map[string][]string{}
	for _, c := range product.Commands() {
		if c.Roles != nil {
			m[c.Type] = c.Roles
		}
	}
	return m
}()

// ── Roles (ladder picked 20 Oct 2026) ───────────────────────────────────────
//
// Two ladders that never mix. The AIO team, highest first:
//   admin         Super admin: everything — shell, Settings, Releases, Lineup
//   fleet_admin   Admin: + firmware pushes and deployments
//   fleet_manager Fleet manager: + remote control, app library, client updates, people
//   operator      Operator: + installs, logs, queries, venues, groups, lock rules
//   support       Support: + screenshot, reboot, lock/unlock, lost mode, alerts, notes
//   viewer        Viewer: sees the whole fleet, read-only (no screenshots)
// Each level can do everything the one below can. A user manages (creates, edits,
// deletes) only accounts strictly below their own level; the super admin also makes
// super admins. Per-person access rules narrow anyone down.
//
// Customers see only their own venues, whatever rules say:
//   owner         Owner: their venues' devices — reboot, lock/unlock, lost mode, alerts
//   staff         Staff: their venue's devices — reboot, find a tablet, alerts
// The AIO team manages customers from Fleet manager up.
//
// The keys are the stored users.role values. "admin" is the super admin (the key
// predates the ladder) and the level labelled "Admin" is fleet_admin. The old
// super_op and user_manager keys were folded into fleet_manager by migration.

type roleDef struct {
	Key, Label, Article string
	Level               int    // within its ladder
	Customer            bool   // venue-walled customer account
	Desc                string // one line for the role pickers
	Ceiling             string // what the role can ever do, for the access editor
}

var roleDefs = []roleDef{
	{Key: "admin", Label: "Super admin", Article: "a super admin", Level: 5,
		Desc: "Everything", Ceiling: "Super admin. Nothing to configure."},
	{Key: "fleet_admin", Label: "Admin", Article: "an admin", Level: 4,
		Desc: "Fleet manager + firmware pushes", Ceiling: "An admin can do everything a fleet manager can, and push firmware updates. Rules say where."},
	{Key: "fleet_manager", Label: "Fleet manager", Article: "a fleet manager", Level: 3,
		Desc: "Operator + remote control, app library, people", Ceiling: "A fleet manager can do everything an operator can, use remote control, manage the app library and the people below them. Firmware pushes need a rule that names them. Rules say where."},
	{Key: "operator", Label: "Operator", Article: "an operator", Level: 2,
		Desc: "Support + installs, logs, venues, lock rules", Ceiling: "An operator can act on devices: commands, lock, notes, alerts, venues and groups, and remote control when a rule names it. Rules say where."},
	{Key: "support", Label: "Support", Article: "a support agent", Level: 1,
		Desc: "Viewer + screenshots, reboot, lock, lost mode, alerts", Ceiling: "Support can take screenshots, reboot, lock and unlock, use lost mode, handle alerts and write notes. Rules say where."},
	{Key: "viewer", Label: "Viewer", Article: "a viewer", Level: 0,
		Desc: "Sees everything, read-only", Ceiling: "A viewer only looks. Rules say which devices they see."},
	{Key: "owner", Label: "Owner", Article: "an owner", Level: 2, Customer: true,
		Desc: "Their venues: reboot, lock, lost mode, alerts", Ceiling: "An owner sees only their venues, and can reboot, lock and unlock, use lost mode and handle alerts there."},
	{Key: "staff", Label: "Staff", Article: "a staff member", Level: 1, Customer: true,
		Desc: "Their venue: reboot, find a tablet", Ceiling: "Staff see only their venue, and can reboot and find a missing tablet there."},
}

var roleByKey = func() map[string]roleDef {
	m := map[string]roleDef{}
	for _, d := range roleDefs {
		m[d.Key] = d
	}
	return m
}()

// roleOrder is every role, highest first, AIO team then customers.
var roleOrder = func() []string {
	var out []string
	for _, d := range roleDefs {
		out = append(out, d.Key)
	}
	return out
}()

func roleKnown(role string) bool { _, ok := roleByKey[role]; return ok }

// roleIsCustomer: an owner or staff account, walled to its venues.
func roleIsCustomer(role string) bool { return roleByKey[role].Customer }

// roleLevel is the level on the AIO ladder; customers and unknown roles are -1, below
// every AIO role, so no level comparison ever lets a customer reach a team page.
func roleLevel(role string) int {
	d, ok := roleByKey[role]
	if !ok || d.Customer {
		return -1
	}
	return d.Level
}

// roleAtLeast: an AIO role at or above min's level.
func roleAtLeast(role, min string) bool {
	return roleLevel(role) >= roleByKey[min].Level && roleLevel(role) >= 0
}

func roleLabel(role string) string {
	if d, ok := roleByKey[role]; ok {
		return d.Label
	}
	return role
}

func roleArticle(role string) string {
	if d, ok := roleByKey[role]; ok {
		return d.Article
	}
	return "an operator"
}

func roleDesc(role string) string { return roleByKey[role].Desc }

// rolePill is the colour of a role's pill in people lists.
func rolePill(role string) string {
	switch role {
	case "admin":
		return "danger"
	case "fleet_admin", "fleet_manager":
		return "warn"
	case "operator", "support":
		return "info"
	case "owner":
		return "accent"
	}
	return ""
}

// roleCanSupport: the safe device actions — screenshot, reboot, lock/unlock, lost
// mode, alerts, notes. Support and up.
func roleCanSupport(role string) bool { return roleAtLeast(role, "support") }

// roleCanOperate: operator powers (device actions, venues, groups, lock rules…).
func roleCanOperate(role string) bool { return roleAtLeast(role, "operator") }

// roleCanOTA: may push firmware (create deployments, add targets, retry, cancel).
// Admin and up by default; a fleet manager reaches the routes too, and the access
// policy then lets them push only where a rule names "OTA updates". Release packages
// themselves stay with the super admin (requireReleaseAdmin).
func roleCanOTA(role string) bool { return roleAtLeast(role, "fleet_manager") }

// roleIsOperatorLike: the "test team" roles that mark QA results — operator up to
// admin, not the super admin.
func roleIsOperatorLike(role string) bool {
	l := roleLevel(role)
	return l >= roleByKey["operator"].Level && l < roleByKey["admin"].Level
}

// roleManagesUsers: who reaches Users, Activity and access control. Fleet manager up;
// like everyone they manage only accounts below their own level.
func roleManagesUsers(role string) bool { return roleAtLeast(role, "fleet_manager") }

// roleCreatesUsers: may create, delete or merge accounts. Same people as above.
func roleCreatesUsers(role string) bool { return roleManagesUsers(role) }

// roleCanAppLibrary: the app library (browse, upload, register). Fleet manager up;
// removing an APK stays with the super admin.
func roleCanAppLibrary(role string) bool { return roleAtLeast(role, "fleet_manager") }

// rank orders any role for "who may manage whom": customers sit below the whole AIO
// team, so a fleet manager (3) manages owners and staff and nobody on a customer
// ladder ever outranks a team member.
func rank(role string) int {
	d, ok := roleByKey[role]
	switch {
	case !ok:
		return -10
	case d.Customer:
		return d.Level - 5 // staff -4, owner -3
	}
	return d.Level
}

// assignableRoles lists the roles an actor may grant: strictly below their own rank,
// except the super admin who may also make super admins. Customers grant nothing here.
func assignableRoles(actorRole string) []string {
	if roleIsCustomer(actorRole) || !roleManagesUsers(actorRole) {
		return nil
	}
	var out []string
	for _, r := range roleOrder {
		if rank(r) < rank(actorRole) || actorRole == "admin" {
			out = append(out, r)
		}
	}
	return out
}

// mayManageUser: actor may act on a target of role targetRole (and, for role
// changes, grant newRole). The super admin manages anyone, other super admins too.
func mayManageUser(actorRole, targetRole, newRole string) bool {
	if actorRole == "admin" {
		return true
	}
	if !roleManagesUsers(actorRole) || rank(targetRole) >= rank(actorRole) {
		return false
	}
	return newRole == "" || rank(newRole) < rank(actorRole)
}
