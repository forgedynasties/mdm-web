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

// ── Roles ────────────────────────────────────────────────────────────────────
//
// Levels, highest first: admin ("Super admin", 4) → dev (3) → user_manager (2)
// → operator (1) → viewer (0). A user may only create, edit or delete accounts
// strictly below their own level, and may only assign roles below their own.
// The env dashboard login is always an admin.
//   admin         everything
//   dev           releases / OTA / deployments / productions + every operator action
//   user_manager  users, access policies, activity + every operator action
//   operator      device actions, per access policy
//   viewer        read-only, per visibility policy

var roleLevels = map[string]int{"owner": 0, "viewer": 0, "operator": 1, "user_manager": 2, "super_op": 2, "admin": 4}

var roleLabels = map[string]string{"admin": "Super Admin", "user_manager": "Access admin", "super_op": "Super op", "operator": "Operator", "viewer": "Viewer", "owner": "Restaurant owner"}

// roleOrder is every assignable role, highest first.
var roleOrder = []string{"admin", "user_manager", "super_op", "operator", "viewer", "owner"}

func roleLevel(role string) int { return roleLevels[role] }

func roleLabel(role string) string {
	if l, ok := roleLabels[role]; ok {
		return l
	}
	return role
}

// roleCanOperate: roles with operator powers (device actions, QA, groups…).
func roleCanOperate(role string) bool {
	return role == "admin" || role == "user_manager" || role == "super_op" || role == "operator"
}

// roleCanOTA: may push firmware updates (create deployments, add targets, retry,
// cancel). Release packages themselves (create / upload / publish / delete) stay
// with canRelease (admin + dev).
func roleCanOTA(role string) bool { return role == "admin" || role == "super_op" }

// roleIsOperatorLike: the "test team" roles — operator, or user manager acting
// as one. Used where operators specifically (not admins) get a behaviour.
func roleIsOperatorLike(role string) bool {
	return role == "operator" || role == "user_manager" || role == "super_op"
}

// roleManagesUsers: may open the Users pages and edit accounts below their level.
// roleManagesUsers: who reaches Users, Activity and access control. Devs were added on
// 30 Sep; like the others they manage only accounts below their own level (dev: access
// admins, super ops, operators, viewers, owners), and delegation limits what they grant.
func roleManagesUsers(role string) bool {
	return role == "admin" || role == "user_manager" || role == "super_op"
}

// roleCreatesUsers: may create, delete or merge accounts. The super op manages
// access rules, roles and passwords of existing accounts but does not add or
// remove them.
func roleCreatesUsers(role string) bool { return role == "admin" || role == "user_manager" }

// assignableRoles lists the roles an actor may grant: strictly below their level,
// except an admin who may also make admins.
func assignableRoles(actorRole string) []string {
	lvl := roleLevel(actorRole)
	var out []string
	for _, r := range roleOrder {
		if roleLevel(r) < lvl || (actorRole == "admin" && r == "admin") {
			out = append(out, r)
		}
	}
	return out
}

// mayManageUser: actor may act on a target of role targetRole (and, for role
// changes, grant newRole). Admins may manage anyone, including other admins.
func mayManageUser(actorRole, targetRole, newRole string) bool {
	if actorRole == "admin" {
		return true
	}
	if roleLevel(targetRole) >= roleLevel(actorRole) {
		return false
	}
	return newRole == "" || roleLevel(newRole) < roleLevel(actorRole)
}
