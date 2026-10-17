package dashboard

import (
	"encoding/json"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"strconv"
	"strings"

	"mdm/internal/db"
)

// launchableOnly filters a device's package list down to app-drawer apps
// (launchable = true) — the full dump is mostly RRO overlays and framework
// plumbing nobody manages. Devices whose client predates the launchable flag
// (no row carries it) keep the full list rather than rendering an empty page.
// Display and kiosk pickers only: pending-install reconciliation and
// InstalledSet must keep working off the FULL list.
func launchableOnly(pkgs []db.DevicePackage) []db.DevicePackage {
	reported := false
	out := pkgs[:0:0]
	for _, p := range pkgs {
		if p.Launchable != nil {
			reported = true
			if *p.Launchable {
				out = append(out, p)
			}
		}
	}
	if !reported {
		return pkgs
	}
	return out
}

// pendingInstallRows returns "installing" rows (resolved app name + apk url) for
// install_apk commands not yet completed/failed — shown in the device Applications list.
// Commands are deduped by APK URL: re-sending the same install (or a group target
// re-firing it) yields one row with a Count, not a stack of identical rows.
// installedPkgs = the packages the device currently reports; apkPkg = learned
// apk_url→package map. A pending install whose APK maps to a package the device
// already has is treated as done and not shown (covers a lost terminal ack until the
// next check-in reconciles it in the DB).
func pendingInstallRows(commands []db.DeviceCommand, apps []db.App, installedPkgs []db.DevicePackage, apkPkg map[string]string) []map[string]string {
	name := make(map[string]string, len(apps))
	for _, a := range apps {
		name[a.ApkURL] = a.Name
	}
	have := make(map[string]bool, len(installedPkgs))
	for _, p := range installedPkgs {
		have[p.PackageName] = true
	}
	alreadyInstalled := func(apkURL string) bool {
		pkg, ok := apkPkg[apkURL]
		return ok && have[pkg]
	}
	// A human phase label for the row's spinner tag, plus the download percent.
	label := func(status string, progress *int) (string, string) {
		switch status {
		case "downloading":
			if progress != nil {
				return "downloading", strconv.Itoa(*progress)
			}
			return "downloading", ""
		case "installing":
			return "installing", ""
		default: // pending / delivered — queued, not yet picked up
			return "installing", ""
		}
	}
	inFlight := func(s string) bool {
		return s == "pending" || s == "delivered" || s == "downloading" || s == "installing"
	}
	var out []map[string]string
	idx := make(map[string]int) // ApkURL -> position in out
	for _, c := range commands {
		if c.Type == "install_apk" && inFlight(c.Status) && !alreadyInstalled(c.ApkURL) {
			if i, ok := idx[c.ApkURL]; ok {
				out[i]["Count"] = strconv.Itoa(atoi(out[i]["Count"]) + 1)
				continue
			}
			n := name[c.ApkURL]
			if n == "" {
				n = c.ApkURL
			}
			phase, pct := label(c.Status, c.Progress)
			idx[c.ApkURL] = len(out)
			out = append(out, map[string]string{"Name": n, "ApkURL": c.ApkURL, "Count": "1", "Phase": phase, "Progress": pct})
		}
	}
	return out
}

// pendingUninstallPkgs returns the set of package names with an in-flight uninstall
// command, so the Applications drawer can show them as "Uninstalling…" until the device
// confirms removal (mirrors pendingInstallRows for installs).
func pendingUninstallPkgs(commands []db.DeviceCommand) map[string]bool {
	inFlight := func(s string) bool {
		return s == "pending" || s == "delivered" || s == "downloading" || s == "installing"
	}
	out := map[string]bool{}
	for _, c := range commands {
		if c.Type != "uninstall" || !inFlight(c.Status) {
			continue
		}
		var p struct {
			Package string `json:"package"`
		}
		if json.Unmarshal(c.Payload, &p) == nil && p.Package != "" {
			out[p.Package] = true
		}
	}
	return out
}

// atoi is a forgiving strconv.Atoi: it returns 0 for unparseable input.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// commandDedupKey identifies "the same command" for duplicate suppression. Install keys
// on the APK URL only (its payload carries a volatile size/etag that must NOT defeat
// dedup); every other type keys on its payload, so an empty-payload one-shot like reboot
// or screenshot collapses to a single in-flight instance per type, while shell/uninstall/
// kiosk distinguish by their arguments.
func commandDedupKey(cmdType, apkURL string, payload json.RawMessage) string {
	if cmdType == "install_apk" {
		return "install_apk|" + apkURL
	}
	p := strings.TrimSpace(string(payload))
	if p == "{}" || p == "null" {
		p = ""
	}
	return cmdType + "|" + p
}

// hasPendingLikeCommand reports whether an identical command (same type + key params) is
// already in flight (pending/delivered/downloading/installing, i.e. not terminal) among
// the device's recent commands. Used to stop a repeat click — or a group target re-firing
// — from stacking a duplicate the device must process again.
func findPendingLikeCommand(commands []db.DeviceCommand, cmdType, apkURL string, payload json.RawMessage) (db.DeviceCommand, bool) {
	key := commandDedupKey(cmdType, apkURL, payload)
	for _, c := range commands {
		switch c.Status {
		case "pending", "delivered", "downloading", "installing":
			if commandDedupKey(c.Type, c.ApkURL, c.Payload) == key {
				return c, true
			}
		}
	}
	return db.DeviceCommand{}, false
}

// DeviceAppsList renders just the installed-apps list region for the device page,
// refreshed live via HTMX when the device checks in (so a freshly installed app
// shows up without a manual reload).
func (h *Handler) DeviceAppsList(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	installedPkgs, _ := h.db.GetDevicePackages(r.Context(), device.ID)
	commands, _ := h.db.GetDeviceCommands(r.Context(), device.ID, h.cfg.CommandExpiry())
	apps, _ := h.db.ListApps(r.Context())
	apkPkg, _ := h.db.GetApkPackageMap(r.Context(), nil)
	h.tmpl.ExecuteTemplate(w, "device-apps-list", map[string]any{
		"Device": device,
		"Role":   h.role(r),
		// Display only drawer apps; the pending-install reconciliation below still
		// checks against the FULL list so a non-launchable install isn't re-shown.
		"InstalledPackages": launchableOnly(installedPkgs),
		"NuggetIDByPackage": nuggetIDByPackage(device.LatestExtra),
		"PendingInstalls":   pendingInstallRows(commands, apps, installedPkgs, apkPkg),
		"Uninstalling":      pendingUninstallPkgs(commands),
	})
}
