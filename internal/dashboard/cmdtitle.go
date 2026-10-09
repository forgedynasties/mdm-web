package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"

	"mdm/internal/db"
)

// One naming for a command on every surface (Actions history, the device page's
// timeline and queue, the command page): the title comes from the type AND the
// payload, so a client self-update is a "Client update · Firmware client 1.8.9" and
// not an "Install app" that happens to carry an APK, and a reboot the OTA flow sent
// is an "Update reboot" whoever pressed the button. 2026-10-07, from
// static/actions-labels-audit.html.

// cmdShape is what the title needs; db.Command and db.DeviceCommand both carry it.
type cmdShape struct {
	Type    string
	ApkURL  string
	Payload json.RawMessage
}

func shapeOf(v any) cmdShape {
	switch c := v.(type) {
	case db.Command:
		return cmdShape{c.Type, c.ApkURL, c.Payload}
	case *db.Command:
		if c != nil {
			return cmdShape{c.Type, c.ApkURL, c.Payload}
		}
	case db.DeviceCommand:
		return cmdShape{c.Type, c.ApkURL, c.Payload}
	case *db.DeviceCommand:
		if c != nil {
			return cmdShape{c.Type, c.ApkURL, c.Payload}
		}
	}
	return cmdShape{}
}

// clientNameForPackage names the MDM client an app_update targets.
func clientNameForPackage(pkg string) string {
	switch pkg {
	case "com.aioapp.mdm":
		return "Firmware client"
	case "aio.app.mdmclient.dpc":
		return "Standard client"
	}
	return pkg
}

func baseName(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

// rebootReason is the payload's reason on a reboot ("ota" for the update flow).
func rebootReason(payload json.RawMessage) string {
	var p struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.Reason
}

// isUpdateReboot: a reboot the firmware-update flow asked for, whoever sent it.
func isUpdateReboot(c db.Command) bool { return c.Type == "reboot" && rebootReason(c.Payload) == "ota" }

// cmdTitleOf returns the title and a short hint for a command.
func cmdTitleOf(s cmdShape) (title, hint string) {
	switch s.Type {
	case "install_apk":
		return "Install app", baseName(s.ApkURL)
	case "uninstall":
		var p struct {
			Package string `json:"package"`
		}
		_ = json.Unmarshal(s.Payload, &p)
		return "Uninstall", p.Package
	case "app_update":
		var p struct {
			Package string `json:"package"`
			Version string `json:"version"`
		}
		_ = json.Unmarshal(s.Payload, &p)
		if p.Package != "" {
			return "Client update", strings.TrimSpace(clientNameForPackage(p.Package) + " " + p.Version)
		}
		return "App update", baseName(s.ApkURL)
	case "reboot":
		if rebootReason(s.Payload) == "ota" {
			return "Update reboot", "after a firmware update"
		}
		return "Reboot", ""
	case "adb_tcp":
		var p struct {
			Port  int `json:"port"`
			Hours int `json:"hours"`
		}
		_ = json.Unmarshal(s.Payload, &p)
		if p.Port == 0 {
			return "Wireless adb", "switch off"
		}
		if p.Hours > 0 {
			return "Wireless adb", fmt.Sprintf("switch on for %d h", p.Hours)
		}
		return "Wireless adb", "switch on until told"
	case "ota":
		var p struct {
			BuildID string `json:"build_id"`
		}
		_ = json.Unmarshal(s.Payload, &p)
		return "OTA update", p.BuildID
	case "shell":
		var p struct {
			Cmd string `json:"cmd"`
		}
		_ = json.Unmarshal(s.Payload, &p)
		return "Shell", p.Cmd
	case "query":
		var p struct {
			Query string `json:"query"`
			Cmd   string `json:"cmd"`
		}
		_ = json.Unmarshal(s.Payload, &p)
		if p.Query != "" {
			return "Query", p.Query
		}
		return "Query", p.Cmd
	case "collect_logs":
		return "Collect logs", "logcat, dumpsys, getprop, OTA log"
	case "logcat":
		var p struct {
			Level string `json:"level"`
		}
		_ = json.Unmarshal(s.Payload, &p)
		if p.Level != "" {
			return "Log capture", "level " + p.Level
		}
		return "Log capture", ""
	case "update_splash":
		return "Boot logo", ""
	case "set_kiosk", "kiosk_set":
		return "Kiosk", ""
	case "remote":
		return "Remote control", ""
	case "wipe":
		return "Wipe", "factory reset"
	case "unenroll":
		return "Unenroll", "handed back, data kept"
	}
	return cmdTypeLabel(s.Type), ""
}

// cmdVerbOf is the past-tense word the history row uses for a finished delivery.
func cmdVerbOf(s cmdShape) string {
	switch s.Type {
	case "install_apk":
		return "installed"
	case "uninstall":
		return "uninstalled"
	case "app_update":
		return "updated"
	case "reboot":
		return "rebooted"
	case "adb_tcp":
		return "switched"
	case "wipe":
		return "wiped"
	case "unenroll":
		return "unenrolled"
	}
	return "delivered"
}
