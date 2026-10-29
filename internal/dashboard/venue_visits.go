package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Setup visits on the venue page (roles replan, decided 30 Oct 2026): Venue settings ›
// Setup visits. The Ops lead opens a visit (what it is for, until when, which Field
// people), extends or closes it; the row also lists the venue's devices that are setting
// up, each with its checklist and Hand over. db/setup_visits.go holds the model.

type venueSetupRow struct {
	Device db.Device
	Checks []handOverCheck
	Ready  bool
}

type venueVisitsView struct {
	Open       []db.SetupVisit
	Past       []db.SetupVisit
	SettingUp  []venueSetupRow
	Ready      int
	CanManage  bool     // open, extend, close visits
	CanHand    bool     // hand over here
	Field      []string // Field accounts, for the people picker
	DefaultEnd string   // yyyy-mm-dd, DefaultVisitDays from today
}

// Summary is the row's one line.
func (v venueVisitsView) Summary() string {
	switch {
	case len(v.Open) == 1:
		return "1 open · ends " + v.Open[0].EndsAt.Format("Mon 2 Jan")
	case len(v.Open) > 1:
		return fmt.Sprintf("%d open", len(v.Open))
	}
	return "None open"
}

// mayManageVisits: the super admin and the Ops lead.
func mayManageVisits(role string) bool { return role == "admin" || role == "fleet_manager" }

func (h *Handler) venueVisitsView(r *http.Request, id uuid.UUID, devices []db.Device) venueVisitsView {
	ctx := r.Context()
	role := h.role(r)
	v := venueVisitsView{CanManage: mayManageVisits(role),
		DefaultEnd: time.Now().AddDate(0, 0, db.DefaultVisitDays).Format("2006-01-02")}
	if vs, err := h.db.VenueVisits(ctx, id); err == nil {
		for _, x := range vs {
			if x.Open() {
				v.Open = append(v.Open, x)
			} else {
				v.Past = append(v.Past, x)
			}
		}
	}
	scopes, _ := h.db.DeviceScopes(ctx)
	for _, d := range devices {
		if sc, ok := scopes[d.ID]; !ok || sc.InService || d.Hidden {
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
	if v.CanManage {
		v.Field = h.fieldAccounts(ctx)
	}
	return v
}

// fieldAccounts lists the usernames with the Field role.
func (h *Handler) fieldAccounts(ctx context.Context) []string {
	us, err := h.db.ListUsers(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, u := range us {
		if u.Role == "field" {
			out = append(out, u.Username)
		}
	}
	sort.Strings(out)
	return out
}

func visitBack(id uuid.UUID) string { return fmt.Sprintf("/restaurants/%s?open=visits#vn-visits", id) }

// RestaurantVisitOpen: POST /restaurants/{id}/visits {purpose, ends (yyyy-mm-dd), people[]}.
func (h *Handler) RestaurantVisitOpen(w http.ResponseWriter, r *http.Request) {
	if !mayManageVisits(h.role(r)) {
		http.Error(w, "Only an Ops lead opens setup visits.", http.StatusForbidden)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !h.access(r).venueInScope(id) {
		http.Error(w, "Venue not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	ends, err := visitEnd(r.FormValue("ends"))
	if err != nil {
		h.hxDoneToast(w, r, visitBack(id), err.Error(), "error")
		return
	}
	people := cleanPeople(r.Form["people"], h.fieldAccounts(r.Context()))
	if len(people) == 0 {
		h.hxDoneToast(w, r, visitBack(id), "Pick at least one Field person", "error")
		return
	}
	purpose := strings.TrimSpace(r.FormValue("purpose"))
	if len(purpose) > 120 {
		purpose = purpose[:120]
	}
	vid, err := h.db.OpenSetupVisit(r.Context(), id, purpose, ends, people, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "visit.open", vid.String(), fmt.Sprintf("%s · %s · until %s", id, strings.Join(people, ", "), ends.Format("2 Jan")))
	h.hxDoneToast(w, r, visitBack(id), "Setup visit open until "+ends.Format("Mon 2 Jan"), "success")
}

// RestaurantVisitChange: POST /restaurants/{id}/visits/{vid} {action: extend|close|people, ends, people[]}.
func (h *Handler) RestaurantVisitChange(w http.ResponseWriter, r *http.Request) {
	if !mayManageVisits(h.role(r)) {
		http.Error(w, "Only an Ops lead changes setup visits.", http.StatusForbidden)
		return
	}
	id, err1 := uuid.Parse(r.PathValue("id"))
	vid, err2 := uuid.Parse(r.PathValue("vid"))
	if err1 != nil || err2 != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	v, err := h.db.GetSetupVisit(r.Context(), vid)
	if err != nil || v.RestaurantID != id {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	back := visitBack(id)
	switch r.FormValue("action") {
	case "extend":
		ends, err := visitEnd(r.FormValue("ends"))
		if err != nil {
			h.hxDoneToast(w, r, back, err.Error(), "error")
			return
		}
		if err := h.db.ExtendSetupVisit(r.Context(), vid, ends); err != nil {
			h.hxDoneToast(w, r, back, "That visit is closed", "error")
			return
		}
		h.audit(r, "visit.extend", vid.String(), ends.Format("2 Jan"))
		h.hxDoneToast(w, r, back, "Visit now ends "+ends.Format("Mon 2 Jan"), "success")
	case "close":
		if err := h.db.CloseSetupVisit(r.Context(), vid, h.currentUsername(r)); err != nil {
			h.hxDoneToast(w, r, back, "That visit is closed already", "error")
			return
		}
		h.audit(r, "visit.close", vid.String(), "")
		h.hxDoneToast(w, r, back, "Visit closed: its Field people no longer see this venue", "success")
	case "people":
		people := cleanPeople(r.Form["people"], h.fieldAccounts(r.Context()))
		if len(people) == 0 {
			h.hxDoneToast(w, r, back, "Keep at least one Field person, or close the visit", "error")
			return
		}
		if err := h.db.SetVisitPeople(r.Context(), vid, people); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "visit.people", vid.String(), strings.Join(people, ", "))
		h.hxDoneToast(w, r, back, "People updated", "success")
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
	}
}

// RestaurantHandOverReady: POST /restaurants/{id}/handover hands over every ready device here.
func (h *Handler) RestaurantHandOverReady(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListRestaurantDevices(r.Context(), id)
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

// visitEnd parses an end date: end of that day in UTC, not in the past, at most 60 days out.
func visitEnd(s string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("Pick an end date")
	}
	end := d.Add(24*time.Hour - time.Second)
	switch {
	case end.Before(time.Now()):
		return time.Time{}, fmt.Errorf("The end date has passed")
	case end.After(time.Now().AddDate(0, 0, 60)):
		return time.Time{}, fmt.Errorf("A visit can last 60 days at most")
	}
	return end, nil
}

// cleanPeople keeps the picked usernames that are Field accounts.
func cleanPeople(picked, field []string) []string {
	ok := map[string]bool{}
	for _, f := range field {
		ok[f] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range picked {
		if ok[p] && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
