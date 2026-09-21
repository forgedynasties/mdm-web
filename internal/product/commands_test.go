package product

import "testing"

// TestCommandCatalogueIsCoherent guards the table itself. It is deliberately about
// shape rather than policy: the point is that a new command type cannot arrive with
// a capability nobody declares, a duplicate entry, or a role list that makes no
// sense — the kinds of slip that made the four old lists disagree.
func TestCommandCatalogueIsCoherent(t *testing.T) {
	known := map[string]bool{
		CapKiosk: true, CapInstallAPK: true, CapUninstall: true, CapReboot: true,
		CapWipe: true, CapConfig: true, CapTelemetry: true, CapScreenCapture: true,
		CapInput: true, CapLogcat: true, CapShell: true, CapOTA: true,
		CapUpdateSplash: true, CapMicGain: true, CapWLC: true, CapAppControl: true,
		CapSelfUpdate: true,
	}
	seen := map[string]bool{}
	for _, c := range Commands() {
		if c.Type == "" {
			t.Error("a command entry has no type")
			continue
		}
		if seen[c.Type] {
			t.Errorf("command %q is described twice", c.Type)
		}
		seen[c.Type] = true
		if c.Cap != "" && !known[c.Cap] {
			t.Errorf("command %q requires capability %q, which is not one of the declared Caps", c.Type, c.Cap)
		}
		for _, r := range c.Roles {
			if !knownRole(r) {
				t.Errorf("command %q lists role %q, which is not a role", c.Type, r)
			}
		}
		if c.Roles != nil && len(c.Roles) == 0 {
			t.Errorf("command %q has an empty (non-nil) role list: sendable by nobody, or not sendable at all?", c.Type)
		}
	}
}

func knownRole(role string) bool {
	switch role {
	case "admin", "dev", "user_manager", "super_op", "operator", "viewer", "owner":
		return true
	}
	return false
}

// TestCatalogueKeepsTheClosedAndProbeTypesClosed pins the entries whose behaviour is
// a decision rather than a default: a type that must stay un-sendable has no role
// list, and the capability probes stay out of the send path entirely.
func TestCatalogueKeepsTheClosedAndProbeTypesClosed(t *testing.T) {
	for _, want := range []string{"mic_gain_set", "kiosk_set", "set_kiosk", "remote"} {
		c, ok := CommandFor(want)
		if !ok {
			t.Errorf("%q is no longer in the catalogue", want)
			continue
		}
		if c.Roles != nil {
			t.Errorf("%q is sendable from the dashboard (%v); mic gain is read-only and the rest are probes, not commands", want, c.Roles)
		}
	}
}

func TestAppControlCommands(t *testing.T) {
	got := AppControlCommands()
	want := []string{"app_reload", "app_restart", "app_clear_cache", "app_update_check"}
	if len(got) != len(want) {
		t.Fatalf("AppControlCommands() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AppControlCommands() = %v, want %v", got, want)
		}
	}
}

func TestCommandNeeds(t *testing.T) {
	// A device's capability gate reads this: it must keep answering for the types
	// the agents advertise, and stay additive (an unknown type needs nothing).
	cases := map[string]string{
		"screenshot":    CapScreenCapture,
		"query":         CapShell, // runs as a shell command on the device
		"app_update":    CapSelfUpdate,
		"mic_gain_read": CapMicGain,
		"reboot":        CapReboot,
		"install_apk":   CapInstallAPK,
		"nonexistent":   "",
	}
	for typ, want := range cases {
		if got := CommandNeeds(typ); got != want {
			t.Errorf("CommandNeeds(%q) = %q, want %q", typ, got, want)
		}
	}
}

// TestAPICommandTypes: the admin JSON API takes the types one request can specify
// completely. query and logcat are the two deliberate exclusions — query's payload
// must come from the vetted query catalog, and a logcat stream is a request row, not
// a command — so they must not quietly appear here.
func TestAPICommandTypes(t *testing.T) {
	api := APICommandTypes()
	for _, want := range []string{"install_apk", "uninstall", "reboot", "screenshot", "shell", "ota", "update_splash", "wipe", "app_update", "app_reload", "app_restart", "app_clear_cache", "app_update_check"} {
		if !api[want] {
			t.Errorf("the admin API no longer accepts %q", want)
		}
	}
	for _, unwanted := range []string{"query", "logcat", "mic_gain_read", "mic_gain_set", "set_kiosk", "remote", "kiosk_set"} {
		if api[unwanted] {
			t.Errorf("the admin API accepts %q; that is a policy change, not a catalogue tweak", unwanted)
		}
	}
	// Every accepted type must be a real command, and none may be a probe.
	for typ := range api {
		c, ok := CommandFor(typ)
		if !ok {
			t.Errorf("the admin API accepts %q, which is not in the catalogue", typ)
			continue
		}
		if c.Roles == nil {
			t.Errorf("the admin API accepts %q, which the dashboard cannot send at all", typ)
		}
	}
}

// TestAdminOnlyResidueIsExactlyTheExpectedSet: these two are the last types judged
// by an access action that does not exist. Both are admin-only, so nothing changes;
// the moment a reachable command joins them, this test says so.
func TestAdminOnlyResidueIsExactlyTheExpectedSet(t *testing.T) {
	want := map[string]bool{"wipe": true, "mic_gain_read": true}
	got := AdminOnlyCommandTypes()
	if len(got) != len(want) {
		t.Fatalf("AdminOnlyCommandTypes() = %v, want %v", got, want)
	}
	for typ := range want {
		if !got[typ] {
			t.Errorf("AdminOnlyCommandTypes() is missing %q", typ)
		}
		c, ok := CommandFor(typ)
		if !ok {
			t.Errorf("%q is not in the catalogue", typ)
			continue
		}
		if len(c.Roles) != 1 || c.Roles[0] != "admin" {
			t.Errorf("%q is no longer admin-only (%v), so an unknown access action would now bite", typ, c.Roles)
		}
	}
}

// TestInstallShapedTypes pins the install-shaped set. Every site that keys an install's
// lifecycle on the command type reads it from here, so widening it silently changes which
// commands get the longer delivery leash, the 'downloading'/'installing' progress acks,
// and the expiry keyed on last activity — and narrowing it is how a stalled app_update
// became un-expirable and wedged a device's whole command queue behind it.
func TestInstallShapedTypes(t *testing.T) {
	if got, want := InstallShapedSQL(), "('install_apk', 'app_update')"; got != want {
		t.Errorf("InstallShapedSQL() = %s, want %s", got, want)
	}
	for _, typ := range []string{"install_apk", "app_update"} {
		c, ok := CommandFor(typ)
		if !ok {
			t.Fatalf("%q is not in the catalogue", typ)
		}
		if !c.InstallShaped {
			t.Errorf("%q is no longer install-shaped — it downloads and installs an APK, so it follows an install's lifecycle", typ)
		}
	}
}
