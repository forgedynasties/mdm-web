package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"strings"

	"mdm/internal/db"
	"mdm/internal/product"
)

// cmdAuthz is the outcome of a command-authorization check. It distinguishes an
// unknown command type (a client error -> 400) from a known type the caller's
// role may not issue (an authorization error -> 403).
type cmdAuthz int

const (
	cmdAuthzOK cmdAuthz = iota
	cmdAuthzUnknownType
	cmdAuthzForbidden
)

// policyActionForCommand maps a command type to the access-policy action key it is
// evaluated against, from the command catalogue (product.Command.Access). Types
// outside the operator set (shell, ota, …) name an action that is never granted to
// operators by their role ceiling anyway.
func policyActionForCommand(cmdType string) string {
	if c, ok := product.CommandFor(cmdType); ok && c.Access != "" {
		return c.Access
	}
	return cmdType
}

// authorizeCommand reports whether role may issue a command of cmdType, per the
// role allowlist in commandRoles.
func (h *Handler) authorizeCommand(role, cmdType string) cmdAuthz {
	roles, known := commandRoles[cmdType]
	if !known {
		return cmdAuthzUnknownType
	}
	allowed := false
	for _, r := range roles {
		if r == role {
			allowed = true
			break
		}
	}
	if !allowed {
		return cmdAuthzForbidden
	}
	return cmdAuthzOK
}

// writeCommandAuthzError writes the HTTP response for a non-OK command
// authorization result and reports whether it wrote anything. Callers return
// early when it returns true.
func writeCommandAuthzError(w http.ResponseWriter, authz cmdAuthz) bool {
	switch authz {
	case cmdAuthzUnknownType:
		http.Error(w, "Unknown command type", http.StatusBadRequest)
		return true
	case cmdAuthzForbidden:
		http.Error(w, "Forbidden", http.StatusForbidden)
		return true
	}
	return false
}

// commandTypeAllowed is a boolean convenience for read/template paths (e.g.
// whether to offer a Resend button). Handlers that create or resend commands
// use authorizeCommand directly so they can distinguish 400 from 403.
func (h *Handler) commandTypeAllowed(role, cmdType string) bool {
	return h.authorizeCommand(role, cmdType) == cmdAuthzOK
}

// canSeeCommandURLs reports whether a role may be shown internal APK/OTA URLs,
// which embed the object-store bucket name. Only operational roles need them;
// viewers must not see them, so the bucket name is not disclosed via command
// reads (GB-05).
func canSeeCommandURLs(role string) bool {
	return roleCanOperate(role)
}

// redactDeviceCommandURLs blanks the APK/OTA URL on a command-history slice for
// roles that may not see it. The template falls back to the command label.
func redactDeviceCommandURLs(role string, cmds []db.DeviceCommand) {
	if canSeeCommandURLs(role) {
		return
	}
	for i := range cmds {
		cmds[i].ApkURL = ""
	}
}

// hideShellForRole reports whether a role must not see raw shell commands in the
// history. Operators cannot run shell (commandRoles limits it to admin/dev) and must
// not browse the shell history either — its command text and captured output can
// expose sensitive operational detail. admin/dev/viewer see history unchanged.
func hideShellForRole(role string) bool { return role != "admin" }

// filterShellDeviceCommands drops raw shell entries from a per-device command
// history when the viewer must not see them (hideShellForRole).
func filterShellDeviceCommands(role string, cmds []db.DeviceCommand) []db.DeviceCommand {
	if !hideShellForRole(role) {
		return cmds
	}
	out := make([]db.DeviceCommand, 0, len(cmds))
	for _, c := range cmds {
		if c.Type == "shell" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// filterShellCommands drops raw shell entries from an Actions/history command
// slice when the viewer must not see them (hideShellForRole).
func filterShellCommands(role string, cmds []db.Command) []db.Command {
	if !hideShellForRole(role) {
		return cmds
	}
	out := make([]db.Command, 0, len(cmds))
	for _, c := range cmds {
		if c.Type == "shell" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// hideAdminCommandsForRole reports whether a role must not see commands the
// old built-in admin login (actor "admin", now gone) created — operators shouldn't see
// admin-issued actions (e.g. an internal test install) in their Actions view.
func hideAdminCommandsForRole(role string) bool { return roleIsOperatorLike(role) }

// filterAdminCommands drops commands created by the admin account from an
// Actions/history slice when the viewer must not see them
// (hideAdminCommandsForRole). "admin" is the old built-in login (never a DB
// user), so what is left of it is history stamped legacyAdminActor.
func (h *Handler) filterAdminCommands(r *http.Request, cmds []db.Command) []db.Command {
	if !h.hideAdminActions(r) {
		return cmds
	}
	out := make([]db.Command, 0, len(cmds))
	for _, c := range cmds {
		if h.isAdminAction(c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// isAdminAction: sent by an admin account, of a type only admins can send (shell,
// boot logo), sent with the admin API key, or still unattributed after the
// startup backfill (BackfillCommandAuthors) — nothing in the audit log names a
// person for those, so they are treated as admin history. The OTA flow's own
// update reboots are the exception and stay visible, labelled automatic.
func (h *Handler) isAdminAction(c db.Command) bool {
	if isSystemReboot(c) {
		return false
	}
	// Unattributed or API-key commands count as admin actions; a shell or boot
	// logo command sent by a named dev is that person's own action.
	if c.CreatedBy == "" || c.CreatedBy == "API key" {
		return true
	}
	return h.isAdminAuthor(c.CreatedBy)
}

// showAdminActionsCookie is set by the "Show admin actions" toggle (admins only).
const showAdminActionsCookie = "mdm_show_admin_actions"

// hideAdminActions: operators/viewers never see actions sent by admin accounts;
// admins don't either unless they switched the toggle on. Server-side so the
// counts and pagination on the Actions/History pages stay right.
func (h *Handler) hideAdminActions(r *http.Request) bool {
	if hideAdminCommandsForRole(h.role(r)) {
		return true
	}
	c, err := r.Cookie(showAdminActionsCookie)
	return err != nil || c.Value != "1"
}

// legacyAdminActor is the name the old built-in dashboard login (DASHBOARD_USER, removed)
// stamped on its history. It has no users row; its commands and audit rows are all that is left.
const legacyAdminActor = "admin"

// isAdminAuthor: only that old built-in login. Team
// accounts, whatever their role, are people whose actions always show.
func (h *Handler) isAdminAuthor(name string) bool {
	if name == "" {
		return false // system-sent (e.g. OTA reboots) — shown, labelled automatic
	}
	return strings.EqualFold(name, legacyAdminActor)
}
