package dashboard

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// ── Venue Wi-Fi (W1, picked 17 Oct) ───────────────────────────────────────────
//
// A venue's Wi-Fi card lists the networks every device there should have. Adding one
// (or changing its password) queues a wifi_config for each device at the venue, now and
// for any device assigned there later; removing one tells them to forget it. The point is
// a router or password change prepared from the dashboard before it happens: "14 of 14"
// says when it is safe to change the router. Admins only — it writes a credential to every
// device at the venue.
//
// Delivery is a reconciler rather than a fan-out at save time, so every way a device can
// arrive at a venue (the venue page, bulk assign, enrollment, a client update that adds
// the capability) is covered by the same rule: a device at the venue that has not finished
// the network's current version gets a command.
//
// The password is sealed (AES-GCM) before it is stored and opened only to build the
// command, whose own copy lives in command_secrets until the device has it. The key is
// derived from SESSION_SECRET: rotating that secret makes stored venue passwords
// unreadable, and they have to be entered again (the card says so).

const (
	venueWifiSeenFactor = 3                   // queue only for devices seen within 3× the check-in interval
	venueWifiPerTick    = 40                  // commands per reconciler run, fleet-wide
	venueWifiRetryAfter = 6 * time.Hour       // a failed or expired push is retried after this
	venueWifiRemoveKeep = 14 * 24 * time.Hour // removed networks are still forgotten for this long
	venueWifiActor      = "Venue Wi-Fi"
)

var errVenueWifiSealed = errors.New("venue Wi-Fi password can no longer be opened (SESSION_SECRET changed)")

func (h *Handler) venueWifiKey() []byte {
	mac := hmac.New(sha256.New, []byte(h.reportSecret))
	mac.Write([]byte("venue-wifi-v1"))
	return mac.Sum(nil)
}

func (h *Handler) sealWifiPassword(pw string) ([]byte, error) {
	if pw == "" {
		return nil, nil
	}
	block, err := aes.NewCipher(h.venueWifiKey())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(pw), nil), nil
}

func (h *Handler) openWifiPassword(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	block, err := aes.NewCipher(h.venueWifiKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errVenueWifiSealed
	}
	pt, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", errVenueWifiSealed
	}
	return string(pt), nil
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// RestaurantWifiSave adds a network to the venue, or changes the password of the one with
// that name.
//
//	POST /restaurants/{id}/wifi  ssid, security (open|wpa2|wpa3), password, hidden
func (h *Handler) RestaurantWifiSave(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	ssid := strings.TrimSpace(r.FormValue("ssid"))
	security := strings.ToLower(strings.TrimSpace(r.FormValue("security")))
	pw := r.FormValue("password")
	if security == "open" {
		pw = ""
	}
	payload, _ := json.Marshal(map[string]any{"action": "add", "ssid": ssid, "security": security, "password": pw})
	if _, msg := product.NormalizeCommandPayload("wifi_config", payload); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	sealed, err := h.sealWifiPassword(pw)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	n, err := h.db.SaveVenueWifi(r.Context(), db.VenueWifiNetwork{RestaurantID: id, SSID: ssid, Security: security,
		Hidden: r.FormValue("hidden") == "on" || r.FormValue("hidden") == "1", Secret: sealed, CreatedBy: h.currentUsername(r)})
	if err != nil {
		log.Printf("[venue-wifi] save %s: %v", id, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	verb := "added"
	if n.Version > 1 {
		verb = "changed"
	}
	h.audit(r, "restaurant.wifi", id.String(), verb+" "+ssid+" ("+security+")")
	// Start delivering now rather than at the next tick, so "N of M" starts moving while
	// the person who added it is still looking.
	go h.RunVenueWifi(context.WithoutCancel(r.Context()))
	http.Redirect(w, r, "/restaurants/"+id.String()+"#wifi", http.StatusFound)
}

// RestaurantWifiRemove removes a network from the venue; its devices forget it, except one
// still connected to it (the client refuses, so a device is never cut off by the dashboard).
//
//	POST /restaurants/{id}/wifi/{nid}/remove
func (h *Handler) RestaurantWifiRemove(w http.ResponseWriter, r *http.Request) {
	id, err1 := uuid.Parse(r.PathValue("id"))
	nid, err2 := uuid.Parse(r.PathValue("nid"))
	if err1 != nil || err2 != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	n, err := h.db.GetVenueWifi(r.Context(), id, nid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.db.RemoveVenueWifi(r.Context(), id, nid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "restaurant.wifi", id.String(), "removed "+n.SSID)
	go h.RunVenueWifi(context.WithoutCancel(r.Context()))
	http.Redirect(w, r, "/restaurants/"+id.String()+"#wifi", http.StatusFound)
}

// ── What the venue page shows ─────────────────────────────────────────────────

// venueWifiRow is one network on the venue's Wi-Fi card.
type venueWifiRow struct {
	ID          uuid.UUID
	SSID        string
	Security    string
	Hidden      bool
	Total       int // devices at the venue
	Done        int // finished the current version (or report it saved, waiting for the router)
	Pending     int // queued or on its way
	Failed      int
	Offline     int  // not done and not seen lately, so nothing is queued yet
	Unsupported int  // their client can't take a network (firmware client < 1.12.0, Standard client < 0.3.0)
	Sealed      bool // the stored password can no longer be opened
	New         bool // added in the last day
}

// Complete is "safe to change the router": every device that can take it, has it.
func (v venueWifiRow) Complete() bool { return v.Total > 0 && v.Done == v.Total-v.Unsupported }

// Pct is the share of devices done, for the delivery bar.
func (v venueWifiRow) Pct() int {
	if v.Total == 0 {
		return 0
	}
	return v.Done * 100 / v.Total
}

// venueWifiState is where one device stands with one network version.
func venueWifiState(p *db.VenueWifiPush) string {
	if p == nil {
		return ""
	}
	switch p.Status {
	case "completed", "installed":
		return "done"
	case "failed", "unsupported":
		return "failed"
	case "expired", "cancelled":
		return "expired"
	}
	return "pending"
}

// venueWifiRows builds the venue's card from its networks, its devices and their pushes.
func (h *Handler) venueWifiRows(ctx context.Context, restaurantID uuid.UUID, devices []db.Device) []venueWifiRow {
	nets, err := h.db.ListVenueWifi(ctx, restaurantID)
	if err != nil || len(nets) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(nets))
	for i, n := range nets {
		ids[i] = n.ID
	}
	pushes, _ := h.db.VenueWifiPushes(ctx, ids)
	latest := latestVenueWifiPushes(pushes)
	seenWithin := time.Duration(h.cfg.CheckinInterval()*venueWifiSeenFactor) * time.Second
	var rows []venueWifiRow
	for _, n := range nets {
		row := venueWifiRow{ID: n.ID, SSID: n.SSID, Security: n.Security, Hidden: n.Hidden, Total: len(devices),
			New: time.Since(n.CreatedAt) < 24*time.Hour}
		if _, err := h.openWifiPassword(n.Secret); err != nil {
			row.Sealed = true
		}
		for _, d := range devices {
			if !d.Supports("wifi_config") {
				row.Unsupported++
				continue
			}
			switch venueWifiState(latest[venueWifiKey{n.ID, d.ID, n.Version, "add"}]) {
			case "done":
				row.Done++
			case "pending":
				row.Pending++
			case "failed":
				row.Failed++
			default:
				if time.Since(d.LastSeenAt) > seenWithin {
					row.Offline++
				} else {
					row.Pending++
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

type venueWifiKey struct {
	net, dev uuid.UUID
	version  int
	action   string
}

func latestVenueWifiPushes(ps []db.VenueWifiPush) map[venueWifiKey]*db.VenueWifiPush {
	m := make(map[venueWifiKey]*db.VenueWifiPush, len(ps))
	for i := range ps {
		p := &ps[i]
		m[venueWifiKey{p.NetworkID, p.DeviceID, p.Version, p.Action}] = p
	}
	return m
}

// savedNetworksView is a device's "Saved networks" (extra.wifi_networks, client 1.12.0 /
// 0.3.0): the names only — clients never report a password.
type savedNetwork struct {
	SSID      string `json:"ssid"`
	Security  string `json:"security"`
	Connected bool   `json:"connected"`
	Pending   bool   `json:"pending"` // a new password held until the router changes (firmware client)
}

func savedNetworksView(extra json.RawMessage) []savedNetwork {
	var m struct {
		Nets []savedNetwork `json:"wifi_networks"`
	}
	if json.Unmarshal(extra, &m) != nil {
		return nil
	}
	// A transition-mode network is saved as two configs (WPA2 and WPA3) under one name, and
	// the firmware client lists both: show the name once, connected if either is.
	var out []savedNetwork
	at := map[string]int{}
	for _, n := range m.Nets {
		k := strings.ToLower(n.SSID)
		if i, ok := at[k]; ok {
			out[i].Connected = out[i].Connected || n.Connected
			out[i].Pending = out[i].Pending || n.Pending
			continue
		}
		at[k] = len(out)
		out = append(out, n)
	}
	return out
}

// ── The reconciler ────────────────────────────────────────────────────────────

// RunVenueWifi queues the wifi_config commands the venues' networks call for: an add for
// each device at a venue that has not finished a live network's current version, a remove
// for each device that still reports a removed one. Runs every minute and after every
// change on a venue card.
func (h *Handler) RunVenueWifi(ctx context.Context) {
	venueWifiMu.Lock()
	defer venueWifiMu.Unlock()
	nets, err := h.db.VenueWifiForReconcile(ctx, venueWifiRemoveKeep)
	if err != nil || len(nets) == 0 {
		return
	}
	ids := make([]uuid.UUID, len(nets))
	for i, n := range nets {
		ids[i] = n.ID
	}
	pushes, err := h.db.VenueWifiPushes(ctx, ids)
	if err != nil {
		log.Printf("[venue-wifi] pushes: %v", err)
		return
	}
	latest := latestVenueWifiPushes(pushes)
	seenWithin := time.Duration(h.cfg.CheckinInterval()*venueWifiSeenFactor) * time.Second
	byVenue := map[uuid.UUID][]db.Device{}
	budget := venueWifiPerTick
	for _, n := range nets {
		devices, ok := byVenue[n.RestaurantID]
		if !ok {
			devices, _ = h.db.ListDevices(ctx, db.DeviceFilter{RestaurantID: n.RestaurantID}, 0, 5000, "serial", "asc")
			byVenue[n.RestaurantID] = devices
		}
		removed := n.RemovedAt != nil
		var pw string
		if !removed {
			if pw, err = h.openWifiPassword(n.Secret); err != nil {
				continue // the card asks for the password again
			}
		}
		for _, d := range devices {
			if budget == 0 {
				return
			}
			if d.Hidden || !d.Supports("wifi_config") || time.Since(d.LastSeenAt) > seenWithin {
				continue
			}
			action := "add"
			if removed {
				action = "remove"
				if !deviceHasNetwork(d.LatestExtra, n.SSID) {
					continue
				}
			}
			p := latest[venueWifiKey{n.ID, d.ID, n.Version, action}]
			switch venueWifiState(p) {
			case "done", "pending":
				continue
			case "failed", "expired":
				if time.Since(p.At) < venueWifiRetryAfter {
					continue
				}
			}
			payload := map[string]any{"action": action, "ssid": n.SSID}
			if !removed {
				payload["security"], payload["hidden"] = n.Security, n.Hidden
				if pw != "" {
					payload["password"] = pw
				}
			}
			b, _ := json.Marshal(payload)
			cmd, err := h.db.CreateCommandBy(ctx, "wifi_config", "", b, "devices", []uuid.UUID{d.ID}, venueWifiActor)
			if err != nil {
				log.Printf("[venue-wifi] %s → %s: %v", n.SSID, d.SerialNumber, err)
				continue
			}
			if err := h.db.RecordVenueWifiPush(ctx, n.ID, d.ID, n.Version, action, cmd.ID); err != nil {
				log.Printf("[venue-wifi] record %s → %s: %v", n.SSID, d.SerialNumber, err)
			}
			h.pushCommand(ctx, cmd, "devices", []uuid.UUID{d.ID})
			budget--
		}
	}
}

// venueWifiMu keeps the minute job and a save-triggered run from queuing the same push twice.
var venueWifiMu sync.Mutex

// deviceHasNetwork reports whether a device says it has the network saved.
func deviceHasNetwork(extra json.RawMessage, ssid string) bool {
	for _, n := range savedNetworksView(extra) {
		if strings.EqualFold(n.SSID, ssid) {
			return true
		}
	}
	return false
}
