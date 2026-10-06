package dashboard

import (
	"context"
	"log"
	"net/http"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Guest info: what a guest at the table sees on a T7 — the venue's name, the table the
// tablet sits on, and the venue's guest Wi-Fi (see db.GuestInfo). Tablets get it on
// every HTTP check-in and when their socket connects; these push it again the moment
// any of it changes here, so a renamed venue or a new Wi-Fi password reaches the
// tables that are online without waiting for a check-in.

// pushGuest sends each connected firmware device its current guest info. Devices
// that are offline pick it up when they next connect or check in.
func (h *Handler) pushGuest(ctx context.Context, gs []db.DeviceGuest) {
	for _, g := range gs {
		if g.AgentKind != product.KindFirmware || !h.hub.IsConnected(g.DeviceID) {
			continue
		}
		h.hub.Push(g.DeviceID, g.Guest.Frame())
	}
}

// pushGuestToDevices re-sends guest info to the given devices (a venue or table change).
func (h *Handler) pushGuestToDevices(ctx context.Context, ids []uuid.UUID) {
	if len(ids) == 0 {
		return
	}
	gs, err := h.db.GuestForDevices(ctx, ids)
	if err != nil {
		log.Printf("[guest] lookup for %d devices: %v", len(ids), err)
		return
	}
	h.pushGuest(ctx, gs)
}

// pushGuestToSerials is pushGuestToDevices for handlers that hold serials.
func (h *Handler) pushGuestToSerials(ctx context.Context, serials []string) {
	if len(serials) == 0 {
		return
	}
	ids, err := h.db.GetDeviceIDsBySerials(ctx, serials)
	if err != nil {
		log.Printf("[guest] device ids for %d serials: %v", len(serials), err)
		return
	}
	h.pushGuestToDevices(ctx, ids)
}

// pushGuestToRestaurant re-sends guest info to every device placed at a venue.
func (h *Handler) pushGuestToRestaurant(ctx context.Context, restaurantID uuid.UUID) {
	gs, err := h.db.GuestForRestaurant(ctx, restaurantID)
	if err != nil {
		log.Printf("[guest] lookup for restaurant %s: %v", restaurantID, err)
		return
	}
	h.pushGuest(ctx, gs)
}

// DeviceSetTableLabel is POST /devices/{serial}/table: the table this tablet sits on,
// shown to guests on its home and lock screens ("" clears it).
func (h *Handler) DeviceSetTableLabel(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "notes", device.ID) {
		return
	}
	label := db.NormalizeTableLabel(r.FormValue("table_label"))
	if r.FormValue("clear") != "" {
		label = ""
	}
	if err := h.db.SetTableLabel(r.Context(), device.ID, label); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	h.auditDev(r, "device.table_label", device.ID, device.SerialNumber, label)
	h.pushGuestToDevices(r.Context(), []uuid.UUID{device.ID})
	h.hub.PublishDeviceUpdate(device.ID)
	h.hxRedirect(w, r, "/devices/"+device.SerialNumber)
}

// RestaurantSetGuestWifi is POST /restaurants/{id}/guest-wifi: the venue's guest
// Wi-Fi (and its guest ordering app, when not the default), pushed to every tablet
// there that is online.
func (h *Handler) RestaurantSetGuestWifi(w http.ResponseWriter, r *http.Request) {
	if !h.requireFleetAction(w, r, "groups") { // "Manage groups & venues", like service hours
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	back := "/restaurants/" + id.String()
	r.ParseForm()
	in := db.RestaurantGuestWifi{
		SSID:       r.FormValue("guest_wifi_ssid"),
		Password:   r.FormValue("guest_wifi_password"),
		Security:   r.FormValue("guest_wifi_security"),
		AppPackage: r.FormValue("guest_app_package"),
	}
	if r.FormValue("action") == "clear" {
		in = db.RestaurantGuestWifi{AppPackage: in.AppPackage}
	}
	wifi, err := db.NormalizeGuestWifi(in)
	if err != nil {
		if hxReq(r) {
			hxToast(w, err.Error(), "error")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.db.SetRestaurantGuestWifi(r.Context(), id, wifi); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// The network name only: the audit log is read by more people than the venue.
	detail := "cleared"
	if wifi.SSID != "" {
		detail = wifi.SSID + " (" + wifi.Security + ")"
	}
	h.audit(r, "restaurant.guest_wifi", id.String(), detail)
	h.pushGuestToRestaurant(r.Context(), id)
	h.hxRedirect(w, r, back)
}
