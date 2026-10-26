package product

import "strings"

// Agent kinds: how a device is managed. Stored on devices.agent_kind.
const (
	// KindFirmware is our own hardware running the com.aioapp.mdm system-app client:
	// shared device key, auto-enrolled on first check-in, full platform privileges.
	KindFirmware = "firmware"
	// KindDPC is a stock Android device managed by the aio.app.mdmclient.dpc Device-Owner
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
	ClassKiosk   = "kiosk"   // self-service kiosk, ours (Kiosk 22) or outsourced
	ClassTablet  = "tablet"  // stock Android tablet (DPC-managed)
	ClassMPOS    = "mpos"    // mobile point of sale
	ClassPOS     = "pos"     // counter point of sale
	ClassDongle  = "dongle"  // Android TV stick / box on an HDMI screen (leanback, remote-driven)
	ClassKDS     = "kds"     // kitchen display
	ClassPayment = "payment" // payment terminal running our agent
	ClassOther   = "other"
	// ClassPanel is retired: the wall kiosks are Kiosk now. The const and its label
	// stay so a row written before the retag still renders; Classes() no longer
	// offers it, so IsClass rejects it on new admin input. Handheld (mpos), payment
	// terminal and tablet were retired the same way on 14 Oct 2026.
	ClassPanel = "panel"
)

// Classes lists every device class in display order (dashboard filters and forms).
func Classes() []string {
	return []string{ClassDongle, ClassPOS, ClassKiosk, ClassKDS, ClassT7, ClassOther}
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

// DefaultRoleApps is the app each role starts with: role_apps is filled from it once, and
// Lineup changes it from then on (roles got their app back on 19 Oct 2026, "A1"). The
// families are the ones live devices of each class have installed. A role's app is a base
// package; its build variants (aio.app.nugget.uatv2, .qa, .internal…) are the same app
// pointed at another backend.
func DefaultRoleApps() map[string]string {
	return map[string]string{
		ClassT7:     "aio.app.nugget",
		ClassKiosk:  "aio.app.kiosk",
		ClassKDS:    "aio.app.kds",
		ClassPOS:    "aio.app.pos",
		ClassDongle: "aio.app.menuboards",
	}
}

// HostAppPattern is the pattern for a role's app (its base package), for the firmware
// client's host_app report (HostApp.java) and for Lock: the base package and its variants.
// A trailing * is a prefix; patterns are comma-separated, the first naming the family.
// "" (no app) sends nothing, and a client told "" stops reporting.
func HostAppPattern(base string) string {
	if base == "" {
		return ""
	}
	return base + "," + base + ".*"
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
	CapWipe          = "wipe"     // factory reset (Device Owner only)
	CapUnenroll      = "unenroll" // hand the device back: drop Device Owner, keep the data
	CapConfig        = "config"   // managed app configurations
	CapTelemetry     = "telemetry"
	CapScreenCapture = "screen_capture" // screenshot + remote screen
	CapInput         = "input"          // remote taps/keys
	CapLogcat        = "logcat"
	CapShell         = "shell"
	CapOTA           = "ota"           // firmware OTA (system partition)
	CapUpdateSplash  = "update_splash" // boot splash write
	CapMicGain       = "mic_gain"      // T7 codec gain (TX_DEC)
	CapWLC           = "wlc"           // wireless-charging guest pad control
	// CapAdbTcp: switch wireless adb on or off (client 1.4.9+). It takes effect only on
	// firmware that ships aio-adb-tcp.rc (v2.1.022+); older images report that plainly.
	CapAdbTcp = "adb_tcp"
	// CapAppControl: the MDM-lite host app's own controls (reload the page, restart the
	// app, clear its web cache, check for an app update). No Device Owner needed.
	CapAppControl = "app_control"
	// CapSelfUpdate: the agent can install a newer version of itself from a URL. The DPC
	// agent does it silently (Device Owner); the MDM-lite host app does it silently only
	// when the device allows it to install apps, else via Android's prompt.
	CapSelfUpdate = "self_update"
	// CapLostMode: the full-screen lost message under lock task. CapWifiConfig: add or
	// forget a Wi-Fi network. Both from firmware client 1.12.0 and DPC agent 0.3.0.
	CapLostMode   = "lost_mode"
	CapWifiConfig = "wifi_config"
	// CapBLEListen: scans Bluetooth LE for the beacons of lost devices nearby. CapBLEBeacon:
	// advertises its own beacon while lost. lost_beacon.ok is the reliable signal for the
	// second: a DPC agent may list it with the radio off.
	CapBLEListen = "ble_listen"
	CapBLEBeacon = "ble_beacon"
	// CapApkPull: uploads an installed app's APK into the app library (pull_apk; DPC agent
	// 0.11.0 lists it, firmware client 1.21.0 sends the apk_pull_cmd flag; 26 Oct 2026).
	CapApkPull = "apk_pull"
)

// FirmwareCapFlags map the extra flags the firmware client sends (it reports no
// capability list) to the capability each one offers, as factory_reset does for wipe:
// client 1.12.0 (code 87) sends both. The DPC agent (0.3.0, code 23) lists lost_mode and
// wifi_config in its capabilities itself.
var FirmwareCapFlags = map[string]string{
	"lost_mode_cmd":   CapLostMode,
	"wifi_config_cmd": CapWifiConfig,
	// Lost-device reporting (18 Oct): firmware client 1.13.0 sends ble_listen / ble_beacon
	// when its radio can scan / advertise. The DPC agent lists both itself.
	"ble_listen": CapBLEListen,
	"ble_beacon": CapBLEBeacon,
	// Save to library (26 Oct): firmware client 1.21.0 runs pull_apk.
	"apk_pull_cmd": CapApkPull,
}

// The command types themselves, and the capability each one requires, live in
// commands.go — one table shared with the dashboard's role allowlist and the admin
// API's accepted set, so the three cannot drift apart again.

// firmwareBaseCaps is everything the system-app client can do on any of our products.
var firmwareBaseCaps = []string{
	CapKiosk, CapInstallAPK, CapUninstall, CapReboot, CapTelemetry,
	CapScreenCapture, CapInput, CapLogcat, CapShell, CapOTA, CapUpdateSplash, CapAdbTcp,
	// The client can install a newer build of itself (ClientUpdater): it holds
	// INSTALL_PACKAGES and runs as system uid, so no firmware OTA is needed for a
	// client change. Offered only when a matching build is hosted for the device's
	// signing variant — see agentUpdateTarget.
	CapSelfUpdate,
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
