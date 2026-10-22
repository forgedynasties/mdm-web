package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Wi-Fi card on the device page's Overview (W1, picked 22 Oct 2026; user: "add option to
// change wifi network correctly using mdm of devices save wifis"). Every saved network
// with its signal from the device's last scan, what it is on now, and per network Switch
// to, Edit password and Forget, plus Add network. All of it is a wifi_config command;
// "connect" is reversible on the device: it must reach the server on the new network
// within 90 s or it goes back to the one it was on and says why (client 1.18.0 / 0.7.0).
// Admin only, like the venue's Wi-Fi.

type wifiNetView struct {
	SSID, Security string
	Connected      bool
	Pending        bool // firmware client: a new password held until the network drops
	Signal         int  // dBm from the last scan, 0 = not seen
	InRange, Weak  bool
	Bars           int // 0–3
	Venue          bool
	CanForget      bool
}

type wifiOpView struct {
	Running bool
	Ok      bool
	Title   string
	Detail  string
	Elapsed string
}

type deviceWifiView struct {
	Serial    string
	Supported bool // the client takes wifi_config
	CanSwitch bool // the client reverts a failed switch (reports wifi_connect)
	CanManage bool
	Current   string // "on AIO-Guest · 5 GHz · 433 Mb/s · −58 dBm"
	Nets      []wifiNetView
	ScanKnown bool // the device sends its scan, so "in range" means something
	Op        *wifiOpView
	Error     string
}

func wifiBars(rssi int) int {
	switch {
	case rssi == 0:
		return 0
	case rssi >= -60:
		return 3
	case rssi >= -72:
		return 2
	}
	return 1
}

func unquoteSSID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "<unknown ssid>" || s == "—" {
		return ""
	}
	return s
}

func (h *Handler) deviceWifiView(ctx context.Context, d *db.Device, canManage bool) deviceWifiView {
	v := deviceWifiView{Serial: d.SerialNumber, Supported: d.Supports("wifi_config"), CanManage: canManage,
		CanSwitch: extraBoolField(d.LatestExtra, "wifi_connect")}
	// The strongest reading of each network in the last scan.
	var scan []struct {
		SSID string `json:"ssid"`
		RSSI int    `json:"rssi"`
	}
	var ex map[string]json.RawMessage
	if json.Unmarshal(d.LatestExtra, &ex) == nil {
		if raw, ok := ex["wifi_scan"]; ok && json.Unmarshal(raw, &scan) == nil && len(scan) > 0 {
			v.ScanKnown = true
		}
	}
	best := map[string]int{}
	for _, s := range scan {
		if s.SSID == "" {
			continue
		}
		if cur, ok := best[s.SSID]; !ok || s.RSSI > cur {
			best[s.SSID] = s.RSSI
		}
	}
	venue := map[string]bool{}
	if d.RestaurantID != nil {
		if nets, err := h.db.ListVenueWifi(ctx, *d.RestaurantID); err == nil {
			for _, n := range nets {
				venue[n.SSID] = true
			}
		}
	}
	current := unquoteSSID(extraString(d.LatestExtra, "wifi"))
	rssiNow := int(extraInt64(d.LatestExtra, "wifi_rssi"))
	for _, n := range savedNetworksView(d.LatestExtra) {
		nv := wifiNetView{SSID: n.SSID, Security: strings.ToUpper(n.Security), Connected: n.Connected || (current != "" && n.SSID == current),
			Pending: n.Pending, Venue: venue[n.SSID]}
		if r, ok := best[n.SSID]; ok {
			nv.Signal, nv.InRange = r, true
		}
		if nv.Connected && rssiNow < 0 {
			nv.Signal, nv.InRange = rssiNow, true
		}
		nv.Weak = nv.InRange && nv.Signal < -80
		nv.Bars = wifiBars(nv.Signal)
		nv.CanForget = !nv.Connected
		v.Nets = append(v.Nets, nv)
	}
	sort.SliceStable(v.Nets, func(i, j int) bool {
		a, b := v.Nets[i], v.Nets[j]
		if a.Connected != b.Connected {
			return a.Connected
		}
		if a.InRange != b.InRange {
			return a.InRange
		}
		return a.Signal > b.Signal
	})
	if current != "" {
		parts := []string{"on " + current}
		if band := extraString(d.LatestExtra, "wifi_band"); band != "" && band != "—" {
			parts = append(parts, band+" GHz")
		}
		if l := extraInt64(d.LatestExtra, "wifi_link_mbps"); l > 0 {
			parts = append(parts, fmt.Sprintf("%d Mb/s", l))
		}
		if rssiNow < 0 {
			parts = append(parts, fmt.Sprintf("%d dBm", rssiNow))
		}
		v.Current = strings.Join(parts, " · ")
	}
	if op, err := h.db.LatestWifiOp(ctx, d.ID, 30*time.Minute, venueWifiActor); err == nil && op != nil {
		v.Op = wifiOpOf(op, time.Now())
	}
	return v
}

// wifiOpOf words the latest change for the strip under the card.
func wifiOpOf(op *db.WifiOp, now time.Time) *wifiOpView {
	var out struct {
		OK         bool   `json:"ok"`
		Error      string `json:"error"`
		Reason     string `json:"reason"`
		RevertedTo string `json:"reverted_to"`
		AfterS     int    `json:"after_s"`
	}
	_ = json.Unmarshal([]byte(op.Output), &out)
	v := &wifiOpView{}
	switch op.Status {
	case "completed":
		v.Ok = true
		switch op.Action {
		case "connect":
			v.Title = "On " + op.SSID
			if out.AfterS > 0 {
				v.Detail = fmt.Sprintf("Reached the MDM after %d s.", out.AfterS)
			}
		case "remove":
			v.Title = "Forgot " + op.SSID
		default:
			v.Title = "Saved " + op.SSID
			v.Detail = "The device keeps its current network; it uses this one when it is the best in range."
		}
	case "failed", "expired":
		why := out.Reason
		if why == "" {
			why = out.Error
		}
		if why == "" && op.Status == "expired" {
			why = "the device did not take the command in time"
		}
		if op.Action == "connect" {
			back := out.RevertedTo
			if back == "" {
				back = "its previous network"
			}
			v.Title = "Back on " + back
			v.Detail = op.SSID + ": " + wifiReason(why) + ". Nothing else changed."
		} else {
			v.Title = "Could not change " + op.SSID
			v.Detail = wifiReason(why)
		}
	default:
		v.Running = true
		el := now.Sub(op.CreatedAt).Round(time.Second)
		v.Elapsed = fmt.Sprintf("%d:%02d", int(el.Minutes()), int(el.Seconds())%60)
		switch op.Action {
		case "connect":
			v.Title = "Switching to " + op.SSID + "…"
			v.Detail = "Waiting for the device to reach the MDM on the new network · " + v.Elapsed + " of 1:30"
		case "remove":
			v.Title = "Forgetting " + op.SSID + "…"
		default:
			v.Title = "Saving " + op.SSID + "…"
		}
	}
	return v
}

// wifiReason turns a client's error code into words.
func wifiReason(code string) string {
	switch code {
	case "auth_failed", "authentication":
		return "the password was refused"
	case "not_found", "not_in_range":
		return "the network was not in range"
	case "no_internet", "unreachable":
		return "it joined, but could not reach the MDM from there"
	case "connected":
		return "it is the network the device is on"
	case "unknown_network":
		return "the device has no such saved network"
	case "":
		return "no reason given"
	}
	return code
}

// DeviceWifi is GET /devices/{serial}/wifi: the card, polled while a change runs.
func (h *Handler) DeviceWifi(w http.ResponseWriter, r *http.Request) {
	d, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil || d == nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	h.renderWifiCard(w, r, h.deviceWifiView(r.Context(), d, h.role(r) == "admin"))
}

func (h *Handler) renderWifiCard(w http.ResponseWriter, r *http.Request, v deviceWifiView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "dd-wifi-card", v); err != nil {
		log.Printf("[device-wifi] render: %v", err)
	}
}

// DeviceWifiSave is POST /devices/{serial}/wifi: action = add (also Edit password) |
// remove | connect, with ssid, security, password, hidden. Admin only.
func (h *Handler) DeviceWifiSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := h.db.GetDevice(ctx, r.PathValue("serial"))
	if err != nil || d == nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	action := r.FormValue("action")
	payload := map[string]any{"action": action, "ssid": r.FormValue("ssid")}
	if action == "add" {
		payload["security"], payload["password"], payload["hidden"] = r.FormValue("security"), r.FormValue("password"), r.FormValue("hidden") == "on"
	}
	raw, _ := json.Marshal(payload)
	v := h.deviceWifiView(ctx, d, true)
	if !d.Supports("wifi_config") {
		v.Error = "This device's client cannot change Wi-Fi."
		h.renderWifiCard(w, r, v)
		return
	}
	if action == "connect" && !v.CanSwitch {
		v.Error = "Switching needs Firmware MDM 1.18.0 or Standard client 0.7.0, which go back by themselves if the new network fails."
		h.renderWifiCard(w, r, v)
		return
	}
	if _, msg := product.NormalizeCommandPayload("wifi_config", raw); msg != "" {
		v.Error = msg
		h.renderWifiCard(w, r, v)
		return
	}
	cmd, err := h.db.CreateCommandBy(ctx, "wifi_config", "", raw, "devices", []uuid.UUID{d.ID}, h.currentUsername(r))
	if err != nil {
		v.Error = "Could not queue the change."
		h.renderWifiCard(w, r, v)
		return
	}
	h.pushCommand(ctx, cmd, "devices", []uuid.UUID{d.ID})
	h.audit(r, "device.wifi", d.SerialNumber, action+" "+r.FormValue("ssid"))
	h.renderWifiCard(w, r, h.deviceWifiView(ctx, d, true))
}
