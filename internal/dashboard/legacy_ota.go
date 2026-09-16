package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/ota"
)

// Updates › Legacy OTA: the otautil devices and their groups, kept apart from the
// fleet. Replaces the old ota-server dashboard. See internal/api/legacy_ota.go.

func (h *Handler) legacyOTAData(r *http.Request) map[string]any {
	ctx := r.Context()
	devices, _ := h.db.ListLegacyOTADevices(ctx)
	deployments, _ := h.db.ListLegacyDeployments(ctx)
	releases, _ := h.db.ListReleasesWithPackages(ctx)
	releases = visibleReleases(h.role(r), releases)

	// Which of these serials also run the MDM client, so the list can link to the
	// device page for the ones that have one.
	inFleet := map[string]bool{}
	// legacyOnly: this serial cannot take an MDM OTA, so the legacy path is the only
	// way to update it. A serial that never joined the fleet is legacy-only by
	// definition; one that did is judged by the OTA gate.
	legacyOnly := map[string]bool{}
	if fleet, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 2000, "serial", "asc"); err == nil {
		for _, d := range fleet {
			inFleet[d.SerialNumber] = true
			legacyOnly[d.SerialNumber] = !h.otaGate.Device(ctx, d).OK
		}
	}
	for i := range devices {
		if _, known := legacyOnly[devices[i].Serial]; !known {
			legacyOnly[devices[i].Serial] = true
		}
	}

	kpi := map[string]int{"seen": len(devices), "recent": 0, "updating": 0, "updated": 0, "failed": 0, "active": 0}
	for i := range devices {
		d := &devices[i]
		if st, ok := ota.Legacy.Get(d.Serial); ok {
			d.Percent = st.Percent
			if st.Phase != "" && (d.Status == "downloading" || d.Status == "installing" || d.Status == "offered") {
				if st.Phase == "installed" {
					d.Status = "installing"
				} else {
					d.Status = st.Phase
				}
			}
		}
		switch d.Status {
		case "offered", "downloading", "installing", "verifying", "finalizing":
			kpi["updating"]++
		case "updated":
			kpi["updated"]++
		case "failed":
			kpi["failed"]++
		}
		if time.Since(d.LastSeen) < 30*time.Minute {
			kpi["recent"]++
		}
	}
	for i := range deployments {
		if deployments[i].Status == "active" {
			kpi["active"]++
		}
		for j := range deployments[i].Devices {
			dv := &deployments[i].Devices[j]
			if st, ok := ota.Legacy.Get(dv.Serial); ok && (dv.Status == "downloading" || dv.Status == "installing" || dv.Status == "offered") {
				dv.Percent = st.Percent
				if st.Phase != "" && st.Phase != "installed" {
					dv.Status = st.Phase
				}
			}
		}
	}
	return map[string]any{
		"Title":             "Legacy OTA",
		"ActivePage":        "updates",
		"Devices":           devices,
		"Deployments":       deployments,
		"Releases":          releases,
		"InFleet":           inFleet,
		"LegacyOnly":        legacyOnly,
		"KPI":               kpi,
		"LegacyOTAMode":     h.cfg.LegacyOTAMode(),
		"LegacyOTAPort":     os.Getenv("LEGACY_OTA_PORT"),
		"LegacyOTAUpstream": os.Getenv("LEGACY_OTA_UPSTREAM"),
		"CanEdit":           roleCanOTA(h.role(r)),
	}
}

// LegacyOTAPage renders the page; ?partial=devices returns just the device table
// (polled while anything is downloading).
func (h *Handler) LegacyOTAPage(w http.ResponseWriter, r *http.Request) {
	data := h.legacyOTAData(r)
	if r.URL.Query().Get("partial") == "devices" {
		_ = h.tmpl.ExecuteTemplate(w, "legacy-devices", h.withRole(r, data))
		return
	}
	h.render(w, r, "legacy_ota.html", data)
}

// LegacyOTAPush creates a legacy deployment: one release offered to the chosen
// otautil serials, tracked per device like a fleet rollout.
func (h *Handler) LegacyOTAPush(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	relID, err := strconv.Atoi(strings.TrimSpace(r.FormValue("release_id")))
	if err != nil || relID <= 0 {
		h.hxDoneToast(w, r, "/updates/legacy", "Choose a release first", "error")
		return
	}
	serials := parseSerialsField(r.Form["serials"])
	if len(serials) == 0 {
		h.hxDoneToast(w, r, "/updates/legacy", "Pick at least one device", "error")
		return
	}
	id, err := h.db.CreateLegacyDeployment(r.Context(), relID, serials, h.currentUsername(r))
	if err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not create the deployment", "error")
		return
	}
	h.audit(r, "legacy_ota.push", strconv.Itoa(relID), strings.Join(serials, ","))
	h.hxDoneToast(w, r, "/updates/legacy",
		fmt.Sprintf("Deployment #%d created · %d device%s get it on their next poll", id, len(serials), plural(len(serials))), "success")
}

// LegacyDeploymentPage is one legacy rollout: every target, where it got to, and
// the two things an operator can still do — retry a failure, reboot a device that
// has the update on its inactive slot. The legacy client never reboots itself.
func (h *Handler) LegacyDeploymentPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	dep, err := h.db.GetLegacyDeployment(r.Context(), id)
	if err != nil || dep == nil {
		http.Error(w, "Deployment not found", http.StatusNotFound)
		return
	}
	// Live percentages for the rows still moving.
	awaiting := 0
	for i := range dep.Devices {
		dv := &dep.Devices[i]
		if st, ok := ota.Legacy.Get(dv.Serial); ok && (dv.Status == "downloading" || dv.Status == "installing" || dv.Status == "offered") {
			dv.Percent = st.Percent
			if st.Phase != "" {
				dv.Status = st.Phase
			}
		}
		if dv.Status == "awaiting_reboot" || (dv.Status == "installing" && dv.Percent >= 100) {
			awaiting++
		}
	}
	data := map[string]any{
		"Title":      "Legacy rollout #" + strconv.Itoa(dep.ID),
		"ActivePage": "updates",
		"Dep":        dep,
		"Awaiting":   awaiting,
		"CanEdit":    roleCanOTA(h.role(r)),
	}
	if r.URL.Query().Get("partial") == "rows" {
		_ = h.tmpl.ExecuteTemplate(w, "legacy-dep-rows", h.withRole(r, data))
		return
	}
	h.render(w, r, "legacy_deployment.html", data)
}

// LegacyOTAReboot reboots a device that has a legacy update applied to its inactive
// slot. Only possible when the device also runs the MDM client — otherwise nothing
// here can reach it and the reboot has to happen on site.
func (h *Handler) LegacyOTAReboot(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	serial := strings.TrimSpace(r.PathValue("serial"))
	back := fmt.Sprintf("/updates/legacy/deployments/%d", id)
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || device == nil {
		h.hxDoneToast(w, r, back, serial+" isn't in the fleet — reboot it on site", "error")
		return
	}
	// Same guard as the device page: never reboot a device that is still taking an OTA.
	if blocked, why, err := h.db.RebootBlockedFor(r.Context(), device.ID); err == nil && blocked {
		h.hxDoneToast(w, r, "/updates/legacy", "Reboot refused: "+why, "error")
		return
	}
	cmd, err := h.db.CreateCommandBy(r.Context(), "reboot", "", nil, "devices", []uuid.UUID{device.ID}, h.currentUsername(r))
	if err != nil {
		h.hxDoneToast(w, r, back, "Could not send the reboot", "error")
		return
	}
	h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{device.ID})
	// Record it on the row: a button that vanishes is not feedback. The row now says
	// when the reboot went out and what the delivery queue did with it.
	_ = h.db.SetLegacyDeploymentReboot(r.Context(), id, serial, cmd.ID)
	h.audit(r, "legacy_ota.reboot", strconv.Itoa(id), serial)
	msg := "Reboot queued for " + serial + " · it boots into the new build"
	if h.hub.IsConnected(device.ID) {
		msg = "Reboot sent to " + serial + " · it boots into the new build"
	}
	h.hxDoneToastEvents(w, r, back, msg, "success", "legacy-refresh")
}

func (h *Handler) LegacyOTACancel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	if err := h.db.CancelLegacyDeployment(r.Context(), id); err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not cancel", "error")
		return
	}
	h.audit(r, "legacy_ota.cancel", strconv.Itoa(id), "")
	h.hxDoneToast(w, r, "/updates/legacy", "Deployment canceled", "success")
}

func (h *Handler) LegacyOTARetry(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	serial := strings.TrimSpace(r.PathValue("serial"))
	if err := h.db.RetryLegacyDeploymentDevice(r.Context(), id, serial); err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not retry", "error")
		return
	}
	_ = h.db.SetLegacyOTADeviceStatus(r.Context(), serial, "idle", "", "")
	h.audit(r, "legacy_ota.retry", strconv.Itoa(id), serial)
	h.hxDoneToast(w, r, "/updates/legacy", "Retrying "+serial+" on its next poll", "success")
}

// legacyPickerRows are the otautil devices that are NOT in the fleet, annotated for a
// release push. A fleet device that happens to poll the legacy listener is already in
// the main list with its own verdict; these are the ones that would otherwise be
// invisible to a push even though the legacy path can reach them.
func (h *Handler) legacyPickerRows(r *http.Request, rel *db.Release, blocked, artifact map[string]string, q, status string) []legacyPickerRow {
	devices, err := h.db.ListLegacyOTADevices(r.Context())
	if err != nil {
		return nil
	}
	hasFull := false
	for _, p := range func() []db.OTAPackage { ps, _ := h.db.ListPackagesByRelease(r.Context(), rel.ID); return ps }() {
		if p.Status == "active" && p.Type == "full" {
			hasFull = true
		}
	}
	q = strings.ToLower(strings.TrimSpace(q))
	const liveWindow = 30 * time.Minute
	var out []legacyPickerRow
	for _, d := range devices {
		if dev, err := h.db.GetDevice(r.Context(), d.Serial); err == nil && dev != nil {
			continue // in the fleet: already listed
		}
		if q != "" && !strings.Contains(strings.ToLower(d.Serial+" "+d.BuildID), q) {
			continue
		}
		online := time.Since(d.LastSeen) < liveWindow
		if (status == "online" && !online) || (status == "offline" && online) {
			continue
		}
		switch {
		case d.BuildID == rel.Version:
			blocked[d.Serial] = "up to date"
		case !hasFull:
			blocked[d.Serial] = "legacy OTA needs a full image"
		case d.Status == "offered" || d.Status == "downloading" || d.Status == "installing" || d.Status == "awaiting_reboot":
			blocked[d.Serial] = "already updating"
		default:
			artifact[d.Serial] = "legacy"
		}
		out = append(out, legacyPickerRow{Device: d, Online: online, LegacyOnly: true})
	}
	return out
}

// browseLegacyDevices feeds the shared picker on the Legacy OTA page: the same
// row markup as the fleet browser, built from legacy_ota_devices. With ?release=
// each row says whether that release can be offered to it.
func (h *Handler) browseLegacyDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := h.db.ListLegacyOTADevices(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	status := r.URL.Query().Get("status")
	blocked, artifact := map[string]string{}, map[string]string{}

	var rel *db.Release
	hasFull := false
	sourceBuilds := map[string]bool{}
	if raw := strings.TrimSpace(r.URL.Query().Get("release")); raw != "" {
		if id, err := strconv.Atoi(raw); err == nil {
			if got, err := h.db.GetRelease(r.Context(), id); err == nil && got != nil {
				rel = got
				pkgs, _ := h.db.ListPackagesByRelease(r.Context(), id)
				for _, p := range pkgs {
					if p.Status != "active" {
						continue
					}
					if p.Type == "full" {
						hasFull = true
					} else if p.SourceBuildID != "" {
						sourceBuilds[p.SourceBuildID] = true
					}
				}
			}
		}
	}

	// A legacy device counts as reachable while it is polling; the app's floor is
	// 15 minutes, so anything quiet for half an hour is treated as offline.
	const liveWindow = 30 * time.Minute
	// Same question the page's toggle asks: is the legacy path the only way to update
	// this serial? Fleet members that can take an MDM OTA are the exception.
	mdmCapable := map[string]bool{}
	if fleet, err := h.db.ListDevices(r.Context(), db.DeviceFilter{}, 0, 2000, "serial", "asc"); err == nil {
		for _, d := range fleet {
			if h.otaGate.Device(r.Context(), d).OK {
				mdmCapable[d.SerialNumber] = true
			}
		}
	}
	rows := make([]legacyPickerRow, 0, len(devices))
	for _, d := range devices {
		if q != "" && !strings.Contains(strings.ToLower(d.Serial+" "+d.BuildID), q) {
			continue
		}
		online := time.Since(d.LastSeen) < liveWindow
		if (status == "online" && !online) || (status == "offline" && online) {
			continue
		}
		if rel != nil {
			switch {
			case d.BuildID == rel.Version:
				blocked[d.Serial] = "up to date"
			case !hasFull && !sourceBuilds[d.BuildID]:
				blocked[d.Serial] = "no update for this build"
			case d.Status == "offered" || d.Status == "downloading" || d.Status == "installing":
				blocked[d.Serial] = "already updating"
			case sourceBuilds[d.BuildID]:
				artifact[d.Serial] = "incremental"
			default:
				artifact[d.Serial] = "full"
			}
		}
		rows = append(rows, legacyPickerRow{Device: d, Online: online, LegacyOnly: !mdmCapable[d.Serial]})
	}
	_ = h.tmpl.ExecuteTemplate(w, "legacy-picker-rows", map[string]any{
		"Rows": rows, "Blocked": blocked, "Artifact": artifact,
	})
}

type legacyPickerRow struct {
	Device     db.LegacyOTADevice
	Online     bool
	LegacyOnly bool // the legacy path is the only way to update this one
}
