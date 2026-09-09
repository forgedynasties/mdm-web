package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"mdm/internal/ota"
)

// Updates › Legacy OTA: the otautil devices and their groups, kept apart from the
// fleet. Replaces the old ota-server dashboard. See internal/api/legacy_ota.go.

func (h *Handler) legacyOTAData(r *http.Request) map[string]any {
	ctx := r.Context()
	devices, _ := h.db.ListLegacyOTADevices(ctx)
	groups, _ := h.db.ListLegacyOTAGroups(ctx)
	releases, _ := h.db.ListReleasesWithPackages(ctx)
	builds := map[string]string{}
	for _, d := range devices {
		builds[d.Serial] = d.BuildID
	}
	kpi := map[string]int{"seen": len(devices), "recent": 0, "updating": 0, "updated": 0, "failed": 0, "enabled": 0}
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
		if time.Since(d.LastSeen) < 20*time.Minute {
			kpi["recent"]++
		}
	}
	for _, g := range groups {
		if g.Enabled {
			kpi["enabled"]++
		}
	}
	return map[string]any{
		"Title":             "Legacy OTA",
		"ActivePage":        "updates",
		"Devices":           devices,
		"DeviceBuild":       builds,
		"Groups":            groups,
		"Releases":          releases,
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

func legacyReleaseID(v string) *int {
	id, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || id <= 0 {
		return nil
	}
	return &id
}

func (h *Handler) LegacyOTAGroupCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		h.hxDoneToast(w, r, "/updates/legacy", "Give the group a name", "error")
		return
	}
	id, err := h.db.CreateLegacyOTAGroup(r.Context(), name, legacyReleaseID(r.FormValue("release_id")), r.FormValue("enabled") == "on")
	if err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not create the group (name taken?)", "error")
		return
	}
	n, _ := h.db.AddLegacyOTAGroupSerials(r.Context(), id, parseSerialsField([]string{r.FormValue("serials")}))
	h.audit(r, "legacy_ota.group_create", name, fmt.Sprintf("%d serials", n))
	h.hxDoneToast(w, r, "/updates/legacy", "Group created", "success")
}

func (h *Handler) LegacyOTAGroupUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	name := strings.TrimSpace(r.FormValue("name"))
	if id <= 0 || name == "" {
		h.hxDoneToast(w, r, "/updates/legacy", "Give the group a name", "error")
		return
	}
	prio, _ := strconv.Atoi(r.FormValue("priority"))
	enabled := r.FormValue("enabled") == "on"
	if err := h.db.UpdateLegacyOTAGroup(r.Context(), id, name, legacyReleaseID(r.FormValue("release_id")), enabled, prio); err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not save the group", "error")
		return
	}
	h.audit(r, "legacy_ota.group_update", name, fmt.Sprintf("release=%s enabled=%v", r.FormValue("release_id"), enabled))
	msg := "Group saved"
	if enabled {
		msg = "Group saved · devices get the update on their next poll"
	}
	h.hxDoneToast(w, r, "/updates/legacy", msg, "success")
}

func (h *Handler) LegacyOTAGroupToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	groups, _ := h.db.ListLegacyOTAGroups(r.Context())
	for _, g := range groups {
		if g.ID == id {
			rid := g.ReleaseID
			if err := h.db.UpdateLegacyOTAGroup(r.Context(), id, g.Name, rid, !g.Enabled, g.Priority); err != nil {
				h.hxDoneToast(w, r, "/updates/legacy", "Could not save", "error")
				return
			}
			h.audit(r, "legacy_ota.group_toggle", g.Name, fmt.Sprintf("enabled=%v", !g.Enabled))
			if !g.Enabled {
				h.hxDoneToast(w, r, "/updates/legacy", "Rollout enabled · devices get it on their next poll", "success")
			} else {
				h.hxDoneToast(w, r, "/updates/legacy", "Rollout paused", "success")
			}
			return
		}
	}
	http.NotFound(w, r)
}

func (h *Handler) LegacyOTAGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	if err := h.db.DeleteLegacyOTAGroup(r.Context(), id); err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not delete", "error")
		return
	}
	h.audit(r, "legacy_ota.group_delete", strconv.Itoa(id), "")
	h.hxDoneToast(w, r, "/updates/legacy", "Group deleted", "success")
}

func (h *Handler) LegacyOTAGroupAddSerials(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	serials := parseSerialsField([]string{r.FormValue("serials")})
	n, err := h.db.AddLegacyOTAGroupSerials(r.Context(), id, serials)
	if err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not add", "error")
		return
	}
	h.audit(r, "legacy_ota.group_add", strconv.Itoa(id), strings.Join(serials, ","))
	h.hxDoneToast(w, r, "/updates/legacy", fmt.Sprintf("Added %d serial%s", n, plural(n)), "success")
}

func (h *Handler) LegacyOTAGroupRemoveSerial(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	serial := strings.TrimSpace(r.FormValue("serial"))
	if err := h.db.RemoveLegacyOTAGroupSerial(r.Context(), id, serial); err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not remove", "error")
		return
	}
	h.audit(r, "legacy_ota.group_remove", strconv.Itoa(id), serial)
	h.hxDoneToast(w, r, "/updates/legacy", "Removed "+serial, "success")
}

func (h *Handler) LegacyOTADeviceDelete(w http.ResponseWriter, r *http.Request) {
	serial := strings.TrimSpace(r.PathValue("serial"))
	if err := h.db.DeleteLegacyOTADevice(r.Context(), serial); err != nil {
		h.hxDoneToast(w, r, "/updates/legacy", "Could not forget the device", "error")
		return
	}
	ota.Legacy.Clear(serial)
	h.audit(r, "legacy_ota.device_forget", serial, "")
	h.hxDoneToast(w, r, "/updates/legacy", "Forgot "+serial+" · it comes back on its next poll", "success")
}
