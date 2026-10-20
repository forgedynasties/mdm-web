package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ── Guest info ────────────────────────────────────────────────────────────────
//
// What a guest sees on a T7: the venue's name. The firmware client keeps it and hands it
// to the launcher and lock screen through its status provider (com.aioapp.status).
//
// Guest Wi-Fi was removed on 19 Oct 2026 (server, dashboard and firmware client 1.13.3).
// The restaurants.guest_wifi_* and guest_app_package columns are left in place, unread,
// so an older image still runs after a rollback. The frame no longer carries them, and a
// client reads a missing field as cleared, so a tablet that still showed a network stops.

// GuestInfo is the "guest" object the server sends to the firmware client. Every field
// is always present (empty when unset) so a value cleared on the dashboard clears on the
// device too.
type GuestInfo struct {
	RestaurantName string `json:"restaurant_name"`
	// ServiceHours are the venue's hours, for the welcome screens' closed-hours screen
	// (20 Oct). null for a device with no venue. A venue without hours of its own has the
	// fleet default (Settings). open == close means that window never applies.
	ServiceHours *GuestHours `json:"service_hours"`
}

// GuestHours is a venue's service window as the client shows it: local "HH:MM" times and
// the venue's timezone ("" = the device's own).
type GuestHours struct {
	Open       string `json:"open"`
	Close      string `json:"close"`
	NightOpen  string `json:"night_open"`
	NightClose string `json:"night_close"`
	Timezone   string `json:"timezone"`
}

func hhmm(min int) string {
	min = ((min % 1440) + 1440) % 1440
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

// Frame is the WebSocket message that pushes guest info to a connected device. It is
// its own message type rather than a partial "config": the client applies a config
// frame's kiosk fields as a whole, so a config carrying only "guest" would read as
// kiosk-off. Clients that predate it log the unknown type and ignore it.
func (g GuestInfo) Frame() []byte {
	b, _ := json.Marshal(map[string]any{"type": "guest", "guest": g})
	return b
}

// DeviceGuest is one device's guest info and what it needs to decide whether to send it.
type DeviceGuest struct {
	DeviceID  uuid.UUID
	AgentKind string
	Guest     GuestInfo
}

// One query shape for every caller: the device row, its venue (if any) and the venue's
// service window (its own, else the fleet default).
const guestSelect = `
	SELECT d.id, d.agent_kind, COALESCE(r.name, ''), d.restaurant_id IS NOT NULL,
	       COALESCE(sw.open_min, fw.open_min, 420), COALESCE(sw.close_min, fw.close_min, 1380),
	       COALESCE(sw.night_open_min, fw.night_open_min, 1410), COALESCE(sw.night_close_min, fw.night_close_min, 360),
	       COALESCE(NULLIF(sw.timezone, ''), NULLIF(r.timezone, ''), NULLIF(fw.timezone, ''), '')
	FROM devices d
	LEFT JOIN restaurants r ON r.id = d.restaurant_id
	LEFT JOIN service_windows sw ON sw.restaurant_id = d.restaurant_id
	LEFT JOIN LATERAL (SELECT * FROM service_windows WHERE restaurant_id IS NULL AND group_id IS NULL LIMIT 1) fw ON true`

func (d *DB) queryGuests(ctx context.Context, where string, arg any) ([]DeviceGuest, error) {
	rows, err := d.pool.Query(ctx, guestSelect+" WHERE "+where, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceGuest
	for rows.Next() {
		var g DeviceGuest
		var placed bool
		var open, close, nOpen, nClose int
		var tz string
		if err := rows.Scan(&g.DeviceID, &g.AgentKind, &g.Guest.RestaurantName, &placed, &open, &close, &nOpen, &nClose, &tz); err != nil {
			return nil, err
		}
		if placed {
			g.Guest.ServiceHours = &GuestHours{Open: hhmm(open), Close: hhmm(close), NightOpen: hhmm(nOpen), NightClose: hhmm(nClose), Timezone: tz}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GuestForDevice returns one device's guest info (PK lookup plus its venue's row).
func (d *DB) GuestForDevice(ctx context.Context, deviceID uuid.UUID) (DeviceGuest, error) {
	gs, err := d.queryGuests(ctx, "d.id = $1", deviceID)
	if err != nil {
		return DeviceGuest{}, err
	}
	if len(gs) == 0 {
		return DeviceGuest{}, errors.New("device not found")
	}
	return gs[0], nil
}

// GuestForDevices returns the guest info of each of the given devices.
func (d *DB) GuestForDevices(ctx context.Context, ids []uuid.UUID) ([]DeviceGuest, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return d.queryGuests(ctx, "d.id = ANY($1)", ids)
}

// GuestForRestaurant returns the guest info of every device placed at a venue.
func (d *DB) GuestForRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]DeviceGuest, error) {
	return d.queryGuests(ctx, "d.restaurant_id = $1 AND NOT d.hidden", restaurantID)
}
