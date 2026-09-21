package product

// ── Command catalogue ─────────────────────────────────────────────────────────
//
// Every command type the MDM can name, described once. Before this, a type lived
// in four lists that had no way to notice each other: the capability map here
// (commandNeeds), the dashboard's role allowlist (commandRoles), the admin API's
// accepted types (validTypes) and the access-policy action the command is
// evaluated against (policyActionForCommand). They drifted silently: `app_update`
// was offered to operators on three surfaces — the device page, the Clients page
// and "Update all behind" — and refused with a 403, because its policy key named
// no access action and an unknown action is a denial; a dev was refused the boot
// splash the same way. Nothing failed to build and no list looked wrong.
//
// One table now backs all four gates. A type added here without a home in them is
// a test failure, not a mystery in production:
//   - internal/product/commands_test.go     — the table's own invariants
//   - internal/dashboard/commands_test.go   — role and policy-action coverage
type Command struct {
	// Type is the string on the wire and in the commands table.
	Type string
	// Cap is the capability a target device must advertise to be offered this
	// command at all ("" = nothing needed). See CommandNeeds.
	Cap string
	// Roles are the dashboard roles that may issue or resend it. nil means the
	// dashboard cannot send this type: a capability probe, a session surface, or a
	// type deliberately closed to callers. An absent type is rejected outright by
	// the send path — that is what closes the old default-allow gap where any
	// arbitrary string (including destructive verbs) was accepted, persisted and
	// dispatched from the lowest-privilege role.
	Roles []string
	// API is whether the admin JSON API accepts the type. Deliberately narrower
	// than Roles: it exists for machine callers, which get the types a single
	// request can specify completely (see "query" below).
	API bool
	// Access is the access-policy action key the command is evaluated against when
	// a non-admin targets a device (accessActions in internal/dashboard). Empty
	// means the type's own name. A key that does not exist is a denial for every
	// non-admin, however generous Roles is, so the dashboard test asserts that every
	// sendable type names a real one.
	Access string
}

// Role sets, named so the table reads as policy rather than string soup. They are
// shared (and never written to) across entries.
var (
	rolesAdmin    = []string{"admin"}
	rolesAdminDev = []string{"admin", "dev"}
	// rolesOperator: the everyday action set. Operators, the access admin and the
	// super op carry it; viewers do not. Where it is allowed is the access policy's
	// business, not this table's.
	rolesOperator = []string{"admin", "dev", "operator", "user_manager", "super_op"}
	// rolesViewer: rolesOperator plus viewer — read-only actions that narrow the
	// fleet rather than change it (a viewer's screenshot is visibility).
	rolesViewer = []string{"admin", "dev", "operator", "user_manager", "super_op", "viewer"}
)

var commands = []Command{
	// ── Everyday device actions ──────────────────────────────────────────────
	// Raw shell is limited to admin/dev; operators reach vetted commands through
	// the device-query catalog instead (the "diagnostic" action on the Actions page).
	{Type: "screenshot", Cap: CapScreenCapture, Roles: rolesViewer, API: true},
	{Type: "install_apk", Cap: CapInstallAPK, Roles: rolesOperator, API: true},
	{Type: "uninstall", Cap: CapUninstall, Roles: rolesOperator, API: true},
	{Type: "reboot", Cap: CapReboot, Roles: rolesOperator, API: true},
	{Type: "shell", Cap: CapShell, Roles: rolesAdminDev, API: true},

	// ── MDM-lite app controls ────────────────────────────────────────────────
	// The menu board app's own controls, all gated by one capability and one grant:
	// operator-level like reboot, and far less disruptive.
	{Type: "app_reload", Cap: CapAppControl, Roles: rolesOperator, API: true, Access: "app_control"},
	{Type: "app_restart", Cap: CapAppControl, Roles: rolesOperator, API: true, Access: "app_control"},
	{Type: "app_clear_cache", Cap: CapAppControl, Roles: rolesOperator, API: true, Access: "app_control"},
	{Type: "app_update_check", Cap: CapAppControl, Roles: rolesOperator, API: true, Access: "app_control"},

	// The agent installing a newer build of itself: the DPC agent, the firmware
	// client (ClientUpdater) and the Lite host app. Evaluated as "install_apk" —
	// it is an install from the same app library, and that is the grant operators
	// already hold. Under its own name ("app_update") it matched no access action,
	// so the policy layer answered "Unknown action" and refused every non-admin a
	// command the role allowlist and all three device surfaces offer them.
	{Type: "app_update", Cap: CapSelfUpdate, Roles: rolesOperator, API: true, Access: "install_apk"},

	// ── Diagnostics ─────────────────────────────────────────────────────────
	// A read-only diagnostic whose command text comes only from the admin-curated
	// query catalog (chosen by query_id, never user-supplied) — which is why
	// operators may issue it without raw-shell rights. The admin API does not
	// accept it: it names no query_id, and taking the payload from the caller would
	// hand an admin key arbitrary shell text labelled "query".
	{Type: "query", Cap: CapShell, Roles: rolesOperator, Access: "query"},

	// Live logs are not a queued command at all — a log stream is a logcat_request
	// row and a stream frame pair. The entry carries the capability that decides
	// whether the device page offers the surface, and the role gate on it.
	{Type: "logcat", Cap: CapLogcat, Roles: rolesAdminDev},

	// ── Firmware-level writes ───────────────────────────────────────────────
	{Type: "ota", Cap: CapOTA, Roles: []string{"admin", "dev", "super_op"}, API: true},

	// Boot logo write. Evaluated as "ota" — a system-partition image write, dev
	// ceiling, sensitive — because as its own name it matched no access action and
	// refused a dev the one surface that offers it (the boot-logo form).
	{Type: "update_splash", Cap: CapUpdateSplash, Roles: rolesAdminDev, API: true, Access: "ota"},

	// Full-device factory reset — DPC-agent devices only, and the most destructive
	// action there is. Admin-only, so no policy key of its own; see
	// accessActionsAdminOnly below.
	{Type: "wipe", Cap: CapWipe, Roles: rolesAdmin, API: true},

	// ── T7 hardware ─────────────────────────────────────────────────────────
	// Mic capture gain (codec TX_DEC). Reading the mixer is admin-only: the field
	// it refreshes is rendered for admins alone (device.html Hardware card).
	{Type: "mic_gain_read", Cap: CapMicGain, Roles: rolesAdmin},
	// Writing the enforcement target is closed to every caller on purpose — mic
	// gain is read-only from the MDM. The entry stays so the capability gate and
	// its client handler keep working and re-opening the type is this role list,
	// not a re-implementation.
	{Type: "mic_gain_set", Cap: CapMicGain},

	// ── Capability probes (not commands) ────────────────────────────────────
	// A probe is asked of a device to decide whether a control is offered; nothing
	// ever creates a command row for it.
	//
	// kiosk_set: the device page's kiosk card ("can this device take kiosk config").
	{Type: "kiosk_set", Cap: CapKiosk},
	// set_kiosk: the Actions page's kiosk card, which writes kiosk config to its
	// targets directly (applyKioskForTargets) and returns — no command row.
	{Type: "set_kiosk", Cap: CapKiosk, Access: "kiosk"},
	// remote: the live screen session; same capability as a screenshot.
	{Type: "remote", Cap: CapScreenCapture},
}

// accessActionsAdminOnly are command types evaluated against an access-action key
// that does not exist in the catalogue. Both are admin-only in the table above, and
// an admin never reaches the policy check (it is unrestricted), so the unknown key
// is a second lock behind a locked door: it changes no outcome today. They are
// listed rather than mapped to someone else's grant so that a *reachable* command
// joining this set is a test failure — which is how app_update and update_splash
// were found.
var accessActionsAdminOnly = map[string]string{
	"wipe":          "wipe",
	"mic_gain_read": "mic_gain_read",
}

// AdminOnlyCommandTypes is the set in accessActionsAdminOnly, exported so the
// dashboard's coverage test can prove it holds exactly those types and no others.
func AdminOnlyCommandTypes() map[string]bool {
	m := make(map[string]bool, len(accessActionsAdminOnly))
	for t := range accessActionsAdminOnly {
		m[t] = true
	}
	return m
}

var commandByType = func() map[string]Command {
	m := make(map[string]Command, len(commands))
	for _, c := range commands {
		m[c.Type] = c
	}
	return m
}()

// Commands lists the whole command vocabulary, in display order.
func Commands() []Command { return commands }

// CommandFor returns the catalogue entry for a command type.
func CommandFor(cmdType string) (Command, bool) {
	c, ok := commandByType[cmdType]
	return c, ok
}

// CommandNeeds returns the capability a command type requires ("" = none). A type
// absent from the catalogue needs nothing — the capability gate is additive, so an
// unknown name cannot withdraw a command from a device.
func CommandNeeds(cmdType string) string { return commandByType[cmdType].Cap }

// AppControlCommands are the MDM-lite app-control command types, all gated by
// CapAppControl and delivered in the check-in response (MDM-lite polls, no socket).
func AppControlCommands() []string {
	var out []string
	for _, c := range commands {
		if c.Cap == CapAppControl {
			out = append(out, c.Type)
		}
	}
	return out
}

// APICommandTypes is the set the admin JSON API accepts (POST /api/v1/commands).
func APICommandTypes() map[string]bool {
	m := map[string]bool{}
	for _, c := range commands {
		if c.API {
			m[c.Type] = true
		}
	}
	return m
}
