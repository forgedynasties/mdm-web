package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
	"mdm/internal/venueicon"
)

// Admin API for venues, so scripts (onboarding, test benches) can do what the
// dashboard's restaurant and Placement edits do: create a restaurant, place devices in it,
// set its icon. Every change is pushed to the connected firmware devices it affects, as
// the dashboard does.

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

// SetRestaurantIcon is POST /api/v1/restaurants/{id}/icon: {"website": "momsbiryanica.com"}
// fetches the site's icon (venueicon.Fetch), {"clear": true} removes it, and a request
// whose Content-Type is image/* stores that image. The icon is kept as a 128px PNG.
func (h *Handler) SetRestaurantIcon(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid restaurant id"})
		return
	}
	ctx := r.Context()
	if _, err := h.db.GetRestaurant(ctx, id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "restaurant not found"})
		return
	}
	var icon []byte
	source := "uploaded"
	if strings.HasPrefix(r.Header.Get("Content-Type"), "image/") {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 2<<20+1))
		if err == nil {
			icon, err = venueicon.Normalize(raw)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else {
		var body struct {
			Website string `json:"website"`
			Clear   bool   `json:"clear"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send {\"website\": …}, {\"clear\": true} or an image"})
			return
		}
		if !body.Clear {
			fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			icon, source, err = venueicon.Fetch(fctx, body.Website)
			cancel()
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
				return
			}
		}
	}
	if err := h.db.SetRestaurantIcon(ctx, id, icon, source); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "has_icon": icon != nil, "bytes": len(icon), "source": source})
}

// UnassignRestaurantDevice takes a device off a venue (DELETE
// /api/v1/restaurants/{id}/devices/{serial}, admin key): the same as removing it on the venue
// page. 404 when the device is not at that venue. Its guest info is pushed so the tablet
// stops showing the venue.
func (h *Handler) UnassignRestaurantDevice(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid restaurant id"})
		return
	}
	serial := r.PathValue("serial")
	d, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || d == nil || d.RestaurantID == nil || *d.RestaurantID != id {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not at this venue"})
		return
	}
	if err := h.db.AssignDeviceToRestaurant(r.Context(), serial, nil); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = h.db.InsertAudit(r.Context(), "API key", "restaurant.unassign", id.String(), serial)
	h.hub.PublishDeviceUpdate(d.ID)
	h.PushGuest(r.Context(), d.ID)
	writeJSON(w, http.StatusOK, map[string]any{"restaurant_id": id, "unassigned": serial})
}
