package dashboard

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// The venue page's Setup row (no setup visits since 3 Nov 2026; user: "we can remove visit
// logic, ops lead can set devices to setup phase, then all the people in field can see them").
// Placing a device at a venue deploys it; the Ops lead puts devices back into the Setting up
// bucket (Set up again), Field people work on them, and anyone allowed hands them over once
// the checklist is green. The row lists the devices setting up here with their checklist.

type venueSetupRow struct {
	Device db.Device
	Checks []handOverCheck
	Ready  bool
}

type venueVisitsView struct {
	SettingUp []venueSetupRow
	Ready     int
	InService int  // devices here in service (what Set up again would move)
	CanManage bool // Set up again (Ops lead, super admin)
	CanHand   bool // hand over here
}

// Summary is the row's one line.
func (v venueVisitsView) Summary() string {
	if len(v.SettingUp) == 0 {
		return "Nothing being set up"
	}
	s := fmt.Sprintf("%d being set up", len(v.SettingUp))
	if v.Ready > 0 {
		s += fmt.Sprintf(" · %d ready to hand over", v.Ready)
	}
	return s
}

// mayManageVisits: who decides what is being set up (the Ops lead and super admins).
func mayManageVisits(role string) bool { return role == "admin" || role == "fleet_admin" || role == "fleet_manager" }

func (h *Handler) venueVisitsView(r *http.Request, id uuid.UUID, devices []db.Device) venueVisitsView {
	ctx := r.Context()
	v := venueVisitsView{CanManage: mayManageVisits(h.role(r))}
	scopes, _ := h.db.DeviceScopes(ctx)
	for _, d := range devices {
		sc, ok := scopes[d.ID]
		if !ok || d.Hidden {
			continue
		}
		if sc.InService {
			v.InService++
			continue
		}
		cfg, _ := h.db.GetOrCreateDeviceConfig(ctx, d.ID)
		checks, ready := h.handOverChecks(ctx, &d, cfg != nil && cfg.KioskEnabled)
		v.SettingUp = append(v.SettingUp, venueSetupRow{Device: d, Checks: checks, Ready: ready})
		if ready {
			v.Ready++
		}
	}
	if len(v.SettingUp) > 0 {
		v.CanHand = h.mayHandOver(r, &v.SettingUp[0].Device)
	}
	return v
}

func visitBack(id uuid.UUID) string { return fmt.Sprintf("/restaurants/%s?open=setup#vn-setup", id) }

// RestaurantHandOverReady: POST /restaurants/{id}/handover hands over every ready device here.
func (h *Handler) RestaurantHandOverReady(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{RestaurantID: id}, 0, 2000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	v := h.venueVisitsView(r, id, devices)
	back := visitBack(id)
	if strings.HasPrefix(r.FormValue("back"), "/") && !strings.HasPrefix(r.FormValue("back"), "//") {
		back = r.FormValue("back")
	}
	if !v.CanHand {
		http.Error(w, "You can't hand devices over here.", http.StatusForbidden)
		return
	}
	var ids []uuid.UUID
	var serials []string
	for _, s := range v.SettingUp {
		if s.Ready {
			ids = append(ids, s.Device.ID)
			serials = append(serials, s.Device.SerialNumber)
		}
	}
	if len(ids) == 0 {
		h.hxDoneToast(w, r, back, "Nothing is ready to hand over yet", "error")
		return
	}
	n, err := h.db.HandOver(r.Context(), ids, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.handover", strings.Join(serials, ","), id.String())
	h.hxDoneToast(w, r, back, fmt.Sprintf("%d device%s handed over · in service", n, map[bool]string{true: "", false: "s"}[n == 1]), "success")
}

