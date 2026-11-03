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

// The Ops lead's home (Q1, picked 3 Nov 2026; user: "for ops they should see, by default, yet
// to assign devices and deployed … a queue for enrolled but not deployed, then they can deploy
// them to venues assign roles create venues", "the queue page should be their main page").
//
// GET /visits ("Setup"), Fleet's layout. The left list: To place · Setting up · In service, then
// the venues and the open setup visits. To place is the queue (db/placement.go): a row per
// device with its role, a venue picker and Deploy; tick several to deploy them together or send
// them to the Lab; "+ New venue…" in the picker creates one and deploys the ticked devices
// there. Setting up holds the old Setup page's to-dos and each device's checklist and Hand over.
// Field's own queue (K2) is the same table on My visits (placeRowsFor).

// placeRow is one device in a To place table.
type placeRow struct {
	Device  db.Device
	Online  bool
	Model   string
	By      string // who enrolled it, the name part of the address
	Role    deviceRole
	Choices []string // the role picker's classes (none: the role comes from the model)
}

// placeRowsFor builds the queue's rows, newest enrolment first.
func (h *Handler) placeRowsFor(ctx context.Context, devs []db.Device) []placeRow {
	var out []placeRow
	for i := range devs {
		d := &devs[i]
		if d.Hidden || d.RestaurantID != nil {
			continue
		}
		row := placeRow{Device: *d, Online: h.hub.IsConnectedForDisplay(d.ID), Model: productLabel(d.Product)}
		row.By, _, _ = strings.Cut(d.EnrolledBy, "@")
		row.Role = h.deviceRoleOf(ctx, d)
		if row.Role.Picker() {
			row.Choices = row.Role.Choices()
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Device.EnrolledAt.After(out[j].Device.EnrolledAt) })
	return out
}

// setupTodo is one line of the Setting up pane's to-do strip.
type setupTodo struct {
	Title, Sub, Action, Href string
}

// SetupHome: GET /visits, the Ops lead's home ("Setup"; /setup is an old redirect to Apps).
// ?view= place (default) | setup | service.
func (h *Handler) SetupHome(w http.ResponseWriter, r *http.Request) {
	if !mayManageVisits(h.role(r)) {
		http.Error(w, "Setup is the Ops lead's home.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	view := r.URL.Query().Get("view")
	if view != "setup" && view != "service" {
		view = "place"
	}
	queue, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateToPlace}, 0, 2000, "serial", "asc")
	setting, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateSettingUp}, 0, 2000, "serial", "asc")
	service, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateInService}, 0, 5000, "serial", "asc")
	visits, _ := h.db.OpenVisits(ctx)
	venues, _ := h.db.ListRestaurants(ctx)
	rows := h.placeRowsFor(ctx, queue)

	onVisit := map[uuid.UUID]bool{}
	for _, v := range visits {
		onVisit[v.RestaurantID] = true
	}
	// Setting up: each device's checklist; the to-dos per venue.
	type venueAgg struct {
		ID           uuid.UUID
		Name         string
		Total, Ready int
	}
	agg := map[uuid.UUID]*venueAgg{}
	var order []uuid.UUID
	var setRows []visitDeviceRow
	ready := 0
	for i := range setting {
		d := &setting[i]
		if d.RestaurantID == nil || d.Hidden {
			continue
		}
		a := agg[*d.RestaurantID]
		if a == nil {
			a = &venueAgg{ID: *d.RestaurantID, Name: d.RestaurantName}
			agg[a.ID] = a
			order = append(order, a.ID)
		}
		a.Total++
		row := visitDeviceRow{Device: *d, Visit: d.RestaurantName, Online: h.hub.IsConnectedForDisplay(d.ID), State: db.StateSettingUp}
		if view == "setup" {
			cfg, _ := h.db.GetOrCreateDeviceConfig(ctx, d.ID)
			row.Checks, row.Ready = h.handOverChecks(ctx, d, cfg != nil && cfg.KioskEnabled)
			row.CanHand = h.mayHandOver(r, d)
			if row.Ready {
				a.Ready++
				ready++
			}
		}
		setRows = append(setRows, row)
	}
	sort.SliceStable(setRows, func(i, j int) bool {
		if setRows[i].Ready != setRows[j].Ready {
			return setRows[i].Ready
		}
		if setRows[i].Visit != setRows[j].Visit {
			return setRows[i].Visit < setRows[j].Visit
		}
		return setRows[i].Device.SerialNumber < setRows[j].Device.SerialNumber
	})
	var todos []setupTodo
	if view == "setup" {
		for _, id := range order {
			if a := agg[id]; !onVisit[id] {
				todos = append(todos, setupTodo{Title: fmt.Sprintf("%s: %d device%s setting up, no visit open", a.Name, a.Total, plural(a.Total)),
					Sub: "nobody on site can finish them: open a visit or hand them over", Action: "Open a visit", Href: visitBack(id)})
			}
		}
		for _, v := range visits {
			if time.Until(v.EndsAt) < 24*time.Hour {
				todos = append(todos, setupTodo{Title: v.Restaurant + " visit ends within a day",
					Sub: joinAnd(v.People) + " · extend it if the work isn't done", Action: "Extend", Href: visitBack(v.RestaurantID)})
			}
		}
	}
	var svc []visitDeviceRow
	for i := range service {
		d := &service[i]
		if d.Hidden {
			continue
		}
		svc = append(svc, visitDeviceRow{Device: *d, Visit: d.RestaurantName, Online: h.hub.IsConnectedForDisplay(d.ID), State: db.StateInService})
	}
	// The rail's venues: the ones with devices, biggest first.
	var railVenues []db.Restaurant
	for _, v := range venues {
		if v.DeviceCount > 0 {
			railVenues = append(railVenues, v)
		}
	}
	sort.SliceStable(railVenues, func(i, j int) bool { return railVenues[i].DeviceCount > railVenues[j].DeviceCount })
	sort.SliceStable(venues, func(i, j int) bool { return strings.ToLower(venues[i].Name) < strings.ToLower(venues[j].Name) })
	h.render(w, r, "ops_home.html", map[string]any{
		"Title": "Setup", "ActivePage": "setup", "View": view,
		"Rows": rows, "SetRows": setRows, "SvcRows": svc, "Todos": todos, "Ready": ready,
		"SettingN": len(setRows), "ServiceN": len(svc), "Visits": visits,
		"Venues": venues, "RailVenues": railVenues, "Back": "/visits",
	})
}

// DevicePlacementPost: POST /devices/{serial}/placement {placement: queue|lab}. "Send to Lab"
// from the queue, "Move to queue" from a Lab device: only while it is at no venue.
func (h *Handler) DevicePlacementPost(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	back := r.FormValue("back")
	if back == "" || !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/devices/" + serial
	}
	d, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || d == nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if d.RestaurantID != nil {
		h.hxDoneToast(w, r, back, "It is at a venue: take it off the venue first", "error")
		return
	}
	p := r.FormValue("placement")
	if err := h.db.SetDevicePlacement(r.Context(), d.ID, p); err != nil {
		h.hxDoneToast(w, r, back, "Placement must be queue or lab", "error")
		return
	}
	msg := "Sent to the Lab"
	if p == db.PlacementQueue {
		msg = "In the queue to place"
	}
	h.audit(r, "device.placement", serial, p)
	h.hub.PublishDeviceUpdate(d.ID)
	h.hxDoneToast(w, r, back, msg, "success")
}
