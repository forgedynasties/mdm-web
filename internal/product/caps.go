package product

import "strings"

// Agent kinds: how a device is managed. Stored on devices.agent_kind.
const (
	// KindFirmware is our own hardware running the com.aioapp.mdm system-app client:
	// shared device key, auto-enrolled on first check-in, full platform privileges.
	KindFirmware = "firmware"
	// KindDPC is a stock Android device managed by the com.skorra.agent Device-Owner
	// agent: explicitly enrolled (profile token → per-device key), capabilities
	// advertised on check-in. Matches the agent's extra.agent_type value.
	KindDPC = "dpc"
	// KindAndroid is the catalog's name for "not our firmware": a product outside the
	// catalog is stock Android, and any agent on it is the DPC agent.
	KindAndroid = "android"
	// AgentTypeMDMLite is the agent_type of AIO MDM-lite, the library embedded in an
	// app (e.g. the menu board) that reports vitals and crashes with no Device Owner.
	// It is stored as KindDPC: like the DPC agent it runs on stock hardware and
	// advertises what it can do, and every non-firmware rule (capability gating, no
	// OTA) applies. latest_extra.agent_type keeps "mdm-lite" so the dashboard can tell
	// the two apart. It only checks in over HTTP: it has no live connection.
	AgentTypeMDMLite = "mdm-lite"
)

// IsStockAgent reports whether a check-in's agent_type is one of the stock-device
// agents (the DPC agent or the embedded app library) rather than our firmware.
func IsStockAgent(agentType string) bool {
	return agentType == KindDPC || agentType == AgentTypeMDMLite
}

// Device roles — what a device does in the restaurant, following the AIO lineup
// (aioapp.com): POS terminal, self-order kiosk, kitchen display, menu board,
// Tableside AI (our T7), handheld, payment terminal. Keys are the historical class
// keys ("dongle" is the menu board, "mpos" the handheld) so stored rows need no
// migration; only the labels changed. Stored on devices.device_class; empty means
// "derive from product" (firmware) or "not set yet" (DPC devices enrolled without a
// class on the profile). Classes are display + filtering axes only: nothing is gated on
// them, capabilities do that.
const (
	ClassT7      = "t7"      // our T7 — the model IS the category, not a generic tablet
	ClassKiosk   = "kiosk"   // self-service kiosk, ours (Kiosk 18/22/27) or outsourced
	ClassTablet  = "tablet"  // stock Android tablet (DPC-managed)
	ClassMPOS    = "mpos"    // mobile point of sale
	ClassPOS     = "pos"     // counter point of sale
	ClassDongle  = "dongle"  // Android TV stick / box on an HDMI screen (leanback, remote-driven)
	ClassKDS     = "kds"     // kitchen display
	ClassPayment = "payment" // payment terminal running our agent
	ClassOther   = "other"
	// ClassPanel is retired: the wall kiosks are Kiosk now. The const and its label
	// stay so a row written before the retag still renders; Classes() no longer
	// offers it, so IsClass rejects it on new admin input.
	ClassPanel = "panel"
)

// Classes lists every device class in display order (dashboard filters and forms).
func Classes() []string {
	return []string{ClassDongle, ClassPOS, ClassKiosk, ClassKDS, ClassT7, ClassMPOS, ClassPayment, ClassTablet, ClassOther}
}

// ClassLabel is the human label for a class key; unknown keys are shown as-is and an
// empty class is "—" so templates never print a blank.
func ClassLabel(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case ClassT7:
		return "Tableside AI"
	case ClassTablet:
		return "Tablet"
	case ClassPanel, ClassKiosk:
		return "Self-order kiosk"
	case ClassMPOS:
		return "Handheld"
	case ClassPOS:
		return "POS terminal"
	case ClassDongle:
		return "Menu board"
	case ClassKDS:
		return "Kitchen display"
	case ClassPayment:
		return "Payment terminal"
	case ClassOther:
		return "Other"
	case "":
		return "—"
	}
	return strings.TrimSpace(class)
}

// IsClass reports whether the key names a known class.
func IsClass(class string) bool {
	for _, c := range Classes() {
		if c == class {
			return true
		}
	}
	return false
}

// Capability names. These are the strings the DPC agent advertises in
// extra.capabilities (see mdm-dpc-agent Capabilities.kt) and the names the server
// assumes for firmware products (DefaultCaps). Keep the two in sync: a command is only
// offered to a device when its capability set contains the command's requirement.
const (
	CapKiosk         = "kiosk" // lock-task / kiosk config
	CapInstallAPK    = "install_apk"
	CapUninstall     = "uninstall"
	CapReboot        = "reboot"
	CapWipe          = "wipe"   // factory reset (Device Owner only)
	CapConfig        = "config" // managed app configurations
	CapTelemetry     = "telemetry"
	CapScreenCapture = "screen_capture" // screenshot + remote screen
	CapInput         = "input"          // remote taps/keys
	CapLogcat        = "logcat"
	CapShell         = "shell"
	CapOTA           = "ota"           // firmware OTA (system partition)
	CapUpdateSplash  = "update_splash" // boot splash write
	CapMicGain       = "mic_gain"      // T7 codec gain (TX_DEC)
	CapWLC           = "wlc"           // wireless-charging guest pad control
	// CapAppControl: the MDM-lite host app's own controls (reload the page, restart the
	// app, clear its web cache, check for an app update). No Device Owner needed.
	CapAppControl = "app_control"
	// CapSelfUpdate: the MDM-lite host app can install a newer version of itself from a
	// URL (silently when the device allows it to install apps, else via Android's prompt).
	CapSelfUpdate = "self_update"
)

// AppControlCommands are the MDM-lite app-control command types, all gated by
// CapAppControl and delivered in the check-in response (MDM-lite polls, no socket).
var AppControlCommands = []string{"app_reload", "app_restart", "app_clear_cache", "app_update_check"}

// commandNeeds maps a command type (the "type" a dashboard form or the admin API
// sends) to the capability it requires. Types absent from the map need nothing
// (e.g. "query"). Names line up with the DPC agent's list so its advertised
// capabilities are used verbatim.
var commandNeeds = map[string]string{
	"screenshot":       CapScreenCapture,
	"install_apk":      CapInstallAPK,
	"uninstall":        CapUninstall,
	"reboot":           CapReboot,
	"shell":            CapShell,
	"logcat":           CapLogcat,
	"ota":              CapOTA,
	"update_splash":    CapUpdateSplash,
	"app_reload":       CapAppControl,
	"app_restart":      CapAppControl,
	"app_clear_cache":  CapAppControl,
	"app_update_check": CapAppControl,
	"app_update":       CapSelfUpdate,
	"wipe":             CapWipe,
	"kiosk_set":        CapKiosk,
	"managed_config":   CapConfig,
	"mic_gain_read":    CapMicGain,
	"mic_gain_set":     CapMicGain,
	"wlc_set":          CapWLC,
	"remote":           CapScreenCapture,
	"query":            CapShell, // runs as a shell command on the device
	"set_kiosk":        CapKiosk, // the Actions page's kiosk card (applyKioskForTargets)
}

// CommandNeeds returns the capability a command type requires ("" = none).
func CommandNeeds(cmdType string) string { return commandNeeds[cmdType] }

// firmwareBaseCaps is everything the system-app client can do on any of our products.
var firmwareBaseCaps = []string{
	CapKiosk, CapInstallAPK, CapUninstall, CapReboot, CapTelemetry,
	CapScreenCapture, CapInput, CapLogcat, CapShell, CapOTA, CapUpdateSplash,
}

// DefaultCaps is the capability set assumed for a firmware device that has not
// reported one (today's system-app client never does). Product-specific hardware
// (the T7's codec gain and wireless-charging pad) is added from the catalog so the
// same gating path serves both device kinds. Returns nil for non-firmware products:
// a stock device must advertise what it can do.
func DefaultCaps(productKey string) []string {
	p, _ := Resolve(productKey)
	if p.Kind != KindFirmware {
		return nil
	}
	out := append([]string(nil), firmwareBaseCaps...)
	if p.Caps.HasWLC {
		out = append(out, CapWLC)
	}
	if p.Key == KeyT7 {
		out = append(out, CapMicGain)
	}
	return out
}

// CapSet turns a capability list into a lookup set (template-friendly: map index
// on a missing key is false).
func CapSet(caps []string) map[string]bool {
	m := make(map[string]bool, len(caps))
	for _, c := range caps {
		if c = strings.TrimSpace(c); c != "" {
			m[c] = true
		}
	}
	return m
}
