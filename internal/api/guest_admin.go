package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Admin API for venues and guest info, so scripts (onboarding, test benches) can do what
// the dashboard's restaurant and Placement edits do: create a restaurant, place devices
// in it, set a device's table and the venue's guest Wi-Fi. Every change is pushed to the
// connected firmware devices it affects, as the dashboard does.

// CreateRestaurant is POST /api/v1/restaurants {"name", "address"?, "timezone"?, "notes"?}.
func (h *Handler) CreateRestaurant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Address  string `json:"address"`
		Timezone string `json:"timezone"`
		Notes    string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	rest, err := h.db.CreateRestaurant(r.Context(), db.Restaurant{
		Name:     strings.TrimSpace(body.Name),
		Address:  strings.TrimSpace(body.Address),
		Timezone: strings.TrimSpace(body.Timezone),
		Notes:    strings.TrimSpace(body.Notes),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, rest)
}

// AssignRestaurantDevices is POST /api/v1/restaurants/{id}/devices {"serials": [...]}:
// places the devices at the venue (moving them from any other) and pushes them its
// guest info.
func (h *Handler) AssignRestaurantDevices(w http.ResponseWriter, r *http.Request) {
	id, ok := h.restaurantFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Serials []string `json:"serials"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Serials) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serials is required"})
		return
	}
	var ids []uuid.UUID
	for _, s := range body.Serials {
		d, err := h.db.GetDevice(r.Context(), strings.TrimSpace(s))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found: " + s})
			return
		}
		ids = append(ids, d.ID)
	}
	if err := h.db.AssignDevicesToRestaurant(r.Context(), body.Serials, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	for _, did := range ids {
		h.PushGuest(r.Context(), did)
	}
	writeJSON(w, http.StatusOK, map[string]any{"restaurant_id": id, "assigned": len(ids)})
}

// SetRestaurantGuestWifi is POST /api/v1/restaurants/{id}/guest-wifi
// {"ssid", "password"?, "security"?, "app_package"?}; an empty ssid clears the network.
func (h *Handler) SetRestaurantGuestWifi(w http.ResponseWriter, r *http.Request) {
	id, ok := h.restaurantFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		SSID       string `json:"ssid"`
		Password   string `json:"password"`
		Security   string `json:"security"`
		AppPackage string `json:"app_package"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	wifi, err := db.NormalizeGuestWifi(db.RestaurantGuestWifi{
		SSID: body.SSID, Password: body.Password, Security: body.Security, AppPackage: body.AppPackage,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.db.SetRestaurantGuestWifi(r.Context(), id, wifi); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.pushGuestToRestaurant(r.Context(), id)
	// The password stays out of the response, as it stays out of the audit log.
	writeJSON(w, http.StatusOK, map[string]any{"restaurant_id": id, "ssid": wifi.SSID,
		"security": wifi.Security, "app_package": wifi.AppPackage})
}

// SetDeviceTable is POST /api/v1/devices/{serial}/table {"table_label"}; "" clears it.
func (h *Handler) SetDeviceTable(w http.ResponseWriter, r *http.Request) {
	device, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	var body struct {
		TableLabel string `json:"table_label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := h.db.SetTableLabel(r.Context(), device.ID, body.TableLabel); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.PushGuest(r.Context(), device.ID)
	writeJSON(w, http.StatusOK, map[string]string{"serial": device.SerialNumber,
		"table_label": db.NormalizeTableLabel(body.TableLabel)})
}

func (h *Handler) restaurantFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid restaurant id"})
		return uuid.Nil, false
	}
	if _, err := h.db.GetRestaurant(r.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "restaurant not found"})
		return uuid.Nil, false
	}
	return id, true
}

// pushGuestToRestaurant pushes a venue's guest info to its connected firmware devices.
func (h *Handler) pushGuestToRestaurant(ctx context.Context, restaurantID uuid.UUID) {
	gs, err := h.db.GuestForRestaurant(ctx, restaurantID)
	if err != nil {
		return
	}
	for _, g := range gs {
		if g.AgentKind == product.KindFirmware {
			h.hub.Push(g.DeviceID, g.Guest.Frame())
		}
	}
}
