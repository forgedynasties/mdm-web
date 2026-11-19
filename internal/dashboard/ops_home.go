package dashboard

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"mdm/internal/db"
)

// The Ops lead's Setup (Q1, picked 3 Nov 2026; simplified 19 Nov 2026, user: "we dont need this
// complexity", picked option A "Fleet's twin with an assign bar").
//
// GET /setup. A device is To place or In service; placing it at a venue puts it in service
// at once, so there is nothing between. The left list: To place, In service, the venues (each
// a link to Fleet on it; the rail's own search finds one among hundreds), then Enrollment
// (Enrolled; the super admin's Profiles, Found on Wi-Fi, From the AIO backend). How to enroll,
// the Android app and New profile are in the island. To place is the queue (db/placement.go): a
// row per device with its role, a venue picker and Deploy; tick several to deploy them together;
// "+ New venue…" in the picker creates one and deploys the ticked
// devices there. In service is a read-only list that opens Fleet. Field's own queue (K2) is the
// same table on /setup/mine (placeRowsFor).

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

// svcRow is one In service row (read-only).
type svcRow struct {
	Device db.Device
	Online bool
}

// SetupHome: GET /setup, the Ops lead's home. ?view= place (default) | service | the
// Enrollment views.
func (h *Handler) SetupHome(w http.ResponseWriter, r *http.Request) {
	if !mayManageVisits(h.role(r)) {
		http.Error(w, "Setup is the Ops lead's home.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	view := r.URL.Query().Get("view")
	switch {
	case view == "service":
	case enrollmentViews[view] && (!superAdminViews[view] || h.role(r) == "admin"):
	default:
		view = "place"
	}
	queue, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateToPlace}, 0, 2000, "serial", "asc")
	venues, _ := h.db.ListRestaurants(ctx)
	rows := h.placeRowsFor(ctx, queue)
	serviceN, _ := h.db.CountDevices(ctx, db.DeviceFilter{State: db.StateInService})

	var svc []svcRow
	if view == "service" {
		service, _ := h.db.ListDevices(ctx, db.DeviceFilter{State: db.StateInService}, 0, 5000, "serial", "asc")
		for i := range service {
			d := &service[i]
			if d.Hidden {
				continue
			}
			svc = append(svc, svcRow{Device: *d, Online: h.hub.IsConnectedForDisplay(d.ID)})
		}
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
	data := map[string]any{
		"Title": "Setup", "ActivePage": "setup", "View": view, "EnView": enrollmentViews[view],
		"Rows": rows, "SvcRows": svc, "ServiceN": serviceN,
		"Venues": venues, "RailVenues": railVenues, "Back": "/setup",
		"Restaurants": venues,
	}
	// Enrollment moved in (M1, 3 Nov 2026): its views and the left list's counts.
	h.enrollmentData(r, view, data)
	h.render(w, r, "ops_home.html", data)
}
