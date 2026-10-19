package dashboard

import (
	"context"
	"log"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Guest info: what a guest at the table sees on a T7 — the venue's name (see
// db.GuestInfo; guest Wi-Fi was removed on 19 Oct). Tablets get it on every HTTP check-in
// and when their socket connects; these push it again the moment it changes here, so a
// renamed venue reaches the tables that are online without waiting for a check-in.

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
