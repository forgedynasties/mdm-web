package db

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ── Guest info ────────────────────────────────────────────────────────────────
//
// What a guest at the table sees on a T7: the venue's name, the table the tablet sits
// on, and the venue's guest Wi-Fi. The firmware client keeps it and hands it to the
// launcher and lock screen through its status provider (com.aioapp.status). The venue
// owns the Wi-Fi (restaurants.guest_wifi_*); the table label belongs to the device.

// Guest Wi-Fi security values, as the Wi-Fi QR format spells them (WIFI:T:…).
const (
	GuestWifiWPA    = "WPA" // WPA/WPA2 personal (also joins WPA3 transition networks)
	GuestWifiWEP    = "WEP"
	GuestWifiNoPass = "nopass"
)

// MaxTableLabel is the longest table label kept, in characters. It has to fit a
// one-line chip on the tablet's home screen.
const MaxTableLabel = 40

// GuestInfo is the "guest" object the server sends to the firmware client. Every
// field is always present (empty when unset) so a value cleared on the dashboard
// clears on the device too.
type GuestInfo struct {
	RestaurantName string `json:"restaurant_name"`
	TableLabel     string `json:"table_label"`
	WifiSSID       string `json:"wifi_ssid"`
	WifiPassword   string `json:"wifi_password"`
	WifiSecurity   string `json:"wifi_security"`
	// AppPackage is the venue's guest ordering app, when it is not the default one.
	AppPackage string `json:"guest_app_package"`
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

// RestaurantGuestWifi is a venue's guest Wi-Fi as stored.
type RestaurantGuestWifi struct {
	SSID       string
	Password   string
	Security   string
	AppPackage string
}

// One query shape for every caller: the device row, its venue (if any), nothing else.
const guestSelect = `
	SELECT d.id, d.agent_kind, COALESCE(r.name, ''), d.table_label,
	       COALESCE(r.guest_wifi_ssid, ''), COALESCE(r.guest_wifi_password, ''),
	       COALESCE(r.guest_wifi_security, 'WPA'), COALESCE(r.guest_app_package, '')
	FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id`

func (d *DB) queryGuests(ctx context.Context, where string, arg any) ([]DeviceGuest, error) {
	rows, err := d.pool.Query(ctx, guestSelect+" WHERE "+where, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceGuest
	for rows.Next() {
		var g DeviceGuest
		if err := rows.Scan(&g.DeviceID, &g.AgentKind, &g.Guest.RestaurantName, &g.Guest.TableLabel,
			&g.Guest.WifiSSID, &g.Guest.WifiPassword, &g.Guest.WifiSecurity, &g.Guest.AppPackage); err != nil {
			return nil, err
		}
		if g.Guest.WifiSSID == "" {
			// No network: nothing about one is sent, whatever is left in the columns.
			g.Guest.WifiPassword, g.Guest.WifiSecurity = "", ""
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

// SetTableLabel sets the table a device sits on ("" clears it). The label is
// normalised by NormalizeTableLabel first.
func (d *DB) SetTableLabel(ctx context.Context, deviceID uuid.UUID, label string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET table_label = $2 WHERE id = $1`, deviceID, NormalizeTableLabel(label))
	return err
}

// GetRestaurantGuestWifi returns a venue's guest Wi-Fi.
func (d *DB) GetRestaurantGuestWifi(ctx context.Context, restaurantID uuid.UUID) (RestaurantGuestWifi, error) {
	var w RestaurantGuestWifi
	err := d.pool.QueryRow(ctx, `
		SELECT guest_wifi_ssid, guest_wifi_password, guest_wifi_security, guest_app_package
		FROM restaurants WHERE id = $1`, restaurantID).Scan(&w.SSID, &w.Password, &w.Security, &w.AppPackage)
	return w, err
}

// SetRestaurantGuestWifi stores a venue's guest Wi-Fi. Pass it through
// NormalizeGuestWifi first; this writes what it is given.
func (d *DB) SetRestaurantGuestWifi(ctx context.Context, restaurantID uuid.UUID, w RestaurantGuestWifi) error {
	if w.Security == "" {
		w.Security = GuestWifiWPA
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE restaurants SET guest_wifi_ssid = $2, guest_wifi_password = $3,
			guest_wifi_security = $4, guest_app_package = $5
		WHERE id = $1`, restaurantID, w.SSID, w.Password, w.Security, w.AppPackage)
	return err
}

// NormalizeTableLabel trims a table label and cuts it to MaxTableLabel characters
// (never through the middle of one).
func NormalizeTableLabel(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxTableLabel {
		s = strings.TrimSpace(string([]rune(s)[:MaxTableLabel]))
	}
	return s
}

var (
	javaPackageRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	hex64Re       = regexp.MustCompile(`^[0-9A-Fa-f]{64}$`)
)

// NormalizeGuestWifi checks a guest Wi-Fi as typed on the dashboard and returns it the
// way it is stored and sent:
//   - no network name clears the network (password and security go with it);
//   - no password means an open network, and an open network keeps no password;
//   - security is one of WPA, WEP, nopass (case-insensitive; WPA2/WPA3 read as WPA,
//     "open"/"none" as nopass).
//
// The error says, in words for the person who typed it, what is wrong.
func NormalizeGuestWifi(in RestaurantGuestWifi) (RestaurantGuestWifi, error) {
	out := RestaurantGuestWifi{
		SSID:       strings.TrimSpace(in.SSID),
		Password:   strings.TrimSpace(in.Password),
		AppPackage: strings.TrimSpace(in.AppPackage),
	}
	if out.AppPackage != "" && (len(out.AppPackage) > 200 || !javaPackageRe.MatchString(out.AppPackage)) {
		return in, errors.New("The guest app must be an Android package name, like aio.app.nugget")
	}
	if out.SSID == "" {
		out.Password, out.Security = "", GuestWifiWPA
		return out, nil
	}
	if len(out.SSID) > 32 {
		return in, errors.New("A Wi-Fi network name is at most 32 bytes")
	}
	switch strings.ToUpper(strings.TrimSpace(in.Security)) {
	case "", "WPA", "WPA2", "WPA3", "WPA/WPA2", "SAE":
		out.Security = GuestWifiWPA
	case "WEP":
		out.Security = GuestWifiWEP
	case "NOPASS", "OPEN", "NONE":
		out.Security = GuestWifiNoPass
	default:
		return in, errors.New("Security must be WPA, WEP or none")
	}
	if out.Password == "" || out.Security == GuestWifiNoPass {
		out.Password, out.Security = "", GuestWifiNoPass
		return out, nil
	}
	switch out.Security {
	case GuestWifiWPA:
		if n := len(out.Password); (n < 8 || n > 63) && !hex64Re.MatchString(out.Password) {
			return in, errors.New("A WPA password is 8 to 63 characters")
		}
	case GuestWifiWEP:
		if len(out.Password) > 64 {
			return in, errors.New("That WEP key is too long")
		}
	}
	return out, nil
}
