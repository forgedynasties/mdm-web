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
	Plural string // "Ops leads", "Support agents": for lists of holders
	// Legacy: a role from before the 30 Oct 2026 replan. It still works for accounts
	// that hold it, but nobody can be given it any more.
	Legacy bool
}

// Roles replan (decided 30 Oct 2026): each role says what someone can do AND which
// devices they see, by the device's state (db/setup_visits.go): Lab (no venue), Setting
// up, In service. Stored keys stay where a role kept its job, so no account needs a
// migration for the label to change: fleet_manager is the Ops lead, operator is Support,
// viewer is Sales. engineer and field are new. fleet_admin ("Admin") and support are
// legacy: their holders move to a new role by hand.
var roleDefs = []roleDef{
	{Key: "admin", Label: "Super admin", Article: "a super admin", Plural: "Super admins", Level: 5,
		Desc: "Everything, firmware included", Ceiling: "Super admin. Nothing to configure."},
	{Key: "fleet_admin", Label: "Admin (old)", Article: "an admin", Plural: "Admins (old)", Level: 4, Legacy: true,
		Desc: "Old role: everything but settings, firmware pushes", Ceiling: "An old role from before 30 Oct 2026. Move this person to a new role."},
	{Key: "fleet_manager", Label: "Ops lead", Article: "an ops lead", Plural: "Ops leads", Level: 3,
		Desc: "Venues and setup visits; devices setting up and in service", Ceiling: "An ops lead sets venues up, opens setup visits, and acts on devices setting up and in service."},
	{Key: "engineer", Label: "Engineer", Article: "an engineer", Plural: "Engineers", Level: 2,
		Desc: "Lab devices; restaurant devices: look, logs, screenshots", Ceiling: "An engineer does everything on lab devices. On restaurant devices they look, collect logs and take screenshots."},
	{Key: "operator", Label: "Support", Article: "a support agent", Plural: "Support agents", Level: 2,
		Desc: "Devices in service: commands, remote control, installs", Ceiling: "Support acts on every device in service: commands, lock, remote control, installs."},
	{Key: "field", Label: "Field", Article: "a field person", Plural: "Field people", Level: 2,
		Desc: "Only the venue of an open setup visit", Ceiling: "Field acts on the devices of venues where they are on an open setup visit, and hands them over."},
	{Key: "support", Label: "Support (old)", Article: "a support agent", Plural: "Support (old)", Level: 1, Legacy: true,
		Desc: "Old role: screenshots, reboot, lock", Ceiling: "An old role from before 30 Oct 2026. Move this person to a new role."},
	{Key: "viewer", Label: "Sales", Article: "a sales person", Plural: "Sales people", Level: 0,
		Desc: "Every restaurant, read-only", Ceiling: "Sales see every device in service, read-only."},
	{Key: "owner", Label: "Owner", Article: "an owner", Plural: "Owners", Level: 2, Customer: true,
		Desc: "Their venues: reboot, lock, lost mode, alerts", Ceiling: "An owner sees only their venues, and can reboot, lock and unlock, use lost mode and handle alerts there."},
	{Key: "staff", Label: "Staff", Article: "a staff member", Plural: "Staff", Level: 1, Customer: true,
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

// rolePlural is the role's plural for lists ("Ops leads"); its label plus s when unset.
func rolePlural(role string) string {
	if d, ok := roleByKey[role]; ok && d.Plural != "" {
		return d.Plural
	}
	return roleLabel(role) + "s"
}

// rolePill is the colour of a role's pill in people lists.
func rolePill(role string) string {
	switch role {
	case "admin":
		return "danger"
	case "fleet_admin", "fleet_manager":
		return "warn"
	case "operator", "support", "engineer":
		return "info"
	case "field":
		return "ok"
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

// roleCanEnroll: may sign in to AIO Enroll and enroll devices with it. Every AIO team role,
// Viewer included (user, 28 Oct 2026: the onboarding team are viewers); customers can't. The
// fleet adb key the app can fetch stays with roleCanFetchFleetKey (Operator and up).
func roleCanEnroll(role string) bool { return !roleIsCustomer(role) && roleAtLeast(role, "viewer") }

// roleCanOTA: may push firmware (create deployments, add targets, retry, cancel, set a
// model's prod firmware). Admin and super admin only (user, 26 Oct 2026: "ota is for
// admin and super admin only"); no access rule gives it to anyone else. Release
// packages themselves stay with the super admin (requireReleaseAdmin).
func roleCanOTA(role string) bool { return roleAtLeast(role, "fleet_admin") }

// roleIsOperatorLike: operator up to admin, not the super admin (hides super-admin-only
// command types from their pickers).
func roleIsOperatorLike(role string) bool {
	l := roleLevel(role)
	return l >= roleByKey["operator"].Level && l < roleByKey["admin"].Level
}

// roleManagesUsers: who reaches Users, Activity and access control. The super admin only
// (roles replan, 30 Oct 2026: "Invite people: super admin"). Owners invite their own staff
// through /team, which does not ask this.
func roleManagesUsers(role string) bool { return role == "admin" }

// roleCreatesUsers: may create, delete or merge accounts. Same people as above.
func roleCreatesUsers(role string) bool { return roleManagesUsers(role) }

// roleCanAppLibrary: the app library (browse, upload, register). Fleet manager up;
// removing an APK stays with the super admin.
func roleCanAppLibrary(role string) bool { return role == "engineer" || roleAtLeast(role, "fleet_manager") }

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
		if roleByKey[r].Legacy {
			continue
		}
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
