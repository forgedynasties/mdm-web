package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"mdm/internal/adbtunnel"
	"mdm/internal/db"
	"mdm/internal/ratelimit"
)

// The Wireless adb page (/adb). Admin only, like the adb_tcp command itself. The connect
// flow both it and the device page use lives in adb_flow.go.

// SetAdbTunnels wires the tunnel manager.
func (h *Handler) SetAdbTunnels(m *adbtunnel.Manager) { h.tunnels = m }

// Minimum agent builds that answer adb_tunnel_open.
const (
	adbTunnelMinFirmwareCode = 72 // client 1.8.9
	adbTunnelMinDPCCode      = 17 // standard client 0.2.9
)

// adbRow is one device on the page.
type adbRow struct {
	Device   *db.Device
	Online   bool
	Group    string // "open" | "off" | "cant"
	State    string // short state text under the serial
	Why      string // for "cant": the reason
	Port     int
	OffIn    string // "23 h" / "" (stays on)
	StaysOn  bool
	IP       string
	LastBy   string
	LastAt   time.Time
	LastWord string // "turned on" / "turned off" / "failed" / "pending"
	// What the device can take.
	CanSwitch bool // adb_tcp (firmware client 1.4.9+)
	CanTunnel bool // agent new enough for adb_tunnel_open
	Tunnel    *adbtunnel.Session
}

// adbRowFor classifies one device. hist is its last adb_tcp command, if any.
func (h *Handler) adbRowFor(d *db.Device, online bool, hist *db.AdbTCPFleetRow) adbRow {
	var p struct {
		AdbTCP *bool  `json:"adb_tcp"`
		IP     string `json:"ip_address"`
	}
	if len(d.LatestExtra) > 0 {
		_ = json.Unmarshal(d.LatestExtra, &p)
	}
	code := extraInt64(d.LatestExtra, "agent_version_code")
	r := adbRow{Device: d, Online: online, IP: p.IP}
	r.CanSwitch = d.Supports("adb_tcp") && code >= adbTCPMinClientCode
	switch d.AgentKind {
	case "firmware":
		r.CanTunnel = code >= adbTunnelMinFirmwareCode
	case "dpc":
		r.CanTunnel = code >= adbTunnelMinDPCCode
	}
	if s, ok := h.tunnels.ForDevice(d.ID); ok {
		r.Tunnel = s
	}
	if hist != nil {
		r.LastBy, r.LastAt = hist.LastBy, hist.LastAt
		switch hist.LastStatus {
		case "completed":
			if hist.Port > 0 {
				r.LastWord = "turned on"
			} else {
				r.LastWord = "turned off"
			}
		case "failed":
			r.LastWord = "failed"
		default:
			r.LastWord = "pending"
		}
	}
	on := p.AdbTCP != nil && *p.AdbTCP
	switch {
	case on:
		r.Group, r.State, r.Port = "open", "On", 5555
		if hist != nil && hist.LastStatus == "completed" && hist.Port > 0 {
			r.Port = hist.Port
			if hist.Hours > 0 {
				if left := time.Until(hist.LastAt.Add(time.Duration(hist.Hours) * time.Hour)); left > 0 {
					r.OffIn = durShort(left)
				}
			} else {
				r.StaysOn = true
			}
		} else if d.AgentKind != "firmware" {
			r.StaysOn = true // the image has it on; nothing of ours switches it off
		}
	case d.AgentKind == "dpc":
		// The standard client has no root, so it can only use an adbd that already listens.
		r.Group, r.State, r.Why = "cant", "Can't", "Standard client: adbd is not listening on 5555 (no root to open it)"
	case !d.Supports("adb_tcp"):
		r.Group, r.State, r.Why = "cant", "Can't", "This agent doesn't take adb_tcp"
	case code > 0 && code < adbTCPMinClientCode:
		r.Group, r.State, r.Why = "cant", "Can't", "Firmware client "+extraString(d.LatestExtra, "agent_version")+" — needs 1.4.9+"
	case hist != nil && hist.LastStatus == "failed":
		r.Group, r.State, r.Why = "cant", "Can't", hist.LastOutput
	default:
		r.Group, r.State = "off", "Off"
	}
	return r
}

// AdbPage renders /adb (A1, picked 17 Oct): "Connect devices" — one search over every device
// that can take wireless adb, the duration, Connect — then the live progress for the devices
// being connected (their serials ride the URL, so a reload keeps watching), the `adb connect`
// lines for all of them, and "Open now" below with copy / turn off / end tunnel, in batch too.
func (h *Handler) AdbPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hist, err := h.db.AdbTCPFleet(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	devices, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 10000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	connected := h.hub.ConnectedIDsForDisplay()
	type pickable struct {
		Serial string `json:"s"`
		Venue  string `json:"v,omitempty"`
		Kind   string `json:"k"`
		Online bool   `json:"o"`
		Open   bool   `json:"a,omitempty"`
	}
	var picks []pickable
	var open []adbRow
	for i := range devices {
		d := &devices[i]
		var hp *db.AdbTCPFleetRow
		if hr, ok := hist[d.ID]; ok {
			hp = &hr
		}
		_, online := connected[d.ID]
		row := h.adbRowFor(d, online, hp)
		if row.CanSwitch || (d.AgentKind == "dpc" && row.Group == "open" && row.CanTunnel) {
			picks = append(picks, pickable{Serial: d.SerialNumber, Venue: d.RestaurantName, Kind: d.KindLabel(), Online: online, Open: row.Group == "open"})
		}
		if row.Group == "open" || row.Tunnel != nil {
			open = append(open, row)
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		if open[i].Device.RestaurantName != open[j].Device.RestaurantName {
			return open[i].Device.RestaurantName < open[j].Device.RestaurantName
		}
		return open[i].Device.SerialNumber < open[j].Device.SerialNumber
	})
	picksJSON, _ := json.Marshal(picks)
	var watching []string
	for _, s := range strings.Split(r.URL.Query().Get("connect"), ",") {
		if s = strings.TrimSpace(s); s != "" && len(watching) < 100 {
			watching = append(watching, s)
		}
	}
	h.render(w, r, "adb.html", map[string]any{
		"Title":      "Wireless adb",
		"Open":       open,
		"Capable":    len(picks),
		"PicksJSON":  template.JS(picksJSON),
		"Watching":   strings.Join(watching, ","),
		"Sessions":   h.tunnels.List(),
		"TunnelsOn":  h.tunnels.Enabled(),
		"TunnelHost": h.adbTunnelHost(r),
		"MyIP":       ratelimit.ClientIP(r),
		"Now":        time.Now(),
	})
}

// AdbBatch turns wireless adb on or off for the ticked devices. The server fans one
// adb_tcp per named device — never a group, never "all" — so the one-device rule of
// product.ValidateAdbTcp still holds: you tick serials, not a venue.
func (h *Handler) AdbBatch(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := r.Form["serial"]
	if len(serials) == 0 {
		h.hxRedirect(w, r, "/adb?flash="+url.QueryEscape("Tick at least one device.")+"&flash_type=error")
		return
	}
	if len(serials) > 100 {
		h.hxRedirect(w, r, "/adb?flash="+url.QueryEscape("At most 100 devices at a time.")+"&flash_type=error")
		return
	}
	port, hours := 0, 0
	if r.FormValue("action") == "on" {
		port = 5555
		hours, _ = strconv.Atoi(r.FormValue("hours"))
		if hours < 0 || hours > 24*30 {
			hours = 24
		}
	}
	payload, _ := json.Marshal(map[string]int{"port": port, "hours": hours})
	// One batch id across the fan-out: the server still sends one adb_tcp per device,
	// but the history shows the run as one row, like a multi-app install.
	if len(serials) > 1 {
		payload = withBatch(payload, uuid.NewString())
	}
	var sent, skipped []string
	for _, serial := range serials {
		d, err := h.db.GetDevice(r.Context(), serial)
		if err != nil || !d.Supports("adb_tcp") {
			skipped = append(skipped, serial)
			continue
		}
		cmd, err := h.db.CreateCommandBy(r.Context(), "adb_tcp", "", payload, "devices", []uuid.UUID{d.ID}, h.currentUsername(r))
		if err != nil {
			skipped = append(skipped, serial)
			continue
		}
		h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{d.ID})
		detail := "off"
		if port > 0 {
			detail = fmt.Sprintf("on port %d for %d h", port, hours)
			if hours == 0 {
				detail = fmt.Sprintf("on port %d, stays on", port)
			}
		}
		h.auditDev(r, "device.adb_tcp", d.ID, serial, detail)
		sent = append(sent, serial)
	}
	msg := fmt.Sprintf("Wireless adb %s sent to %d device(s).", map[bool]string{true: "on", false: "off"}[port > 0], len(sent))
	kind := "success"
	if len(skipped) > 0 {
		msg += " Skipped (can't take it): " + strings.Join(skipped, ", ")
		kind = "info"
	}
	h.hxRedirect(w, r, "/adb?flash="+url.QueryEscape(msg)+"&flash_type="+kind)
}

// AdbTunnelOpen starts a tunnel session for one device and lands on its panel.
func (h *Handler) AdbTunnelOpen(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serial := strings.TrimSpace(r.FormValue("serial"))
	d, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	allow := strings.TrimSpace(r.FormValue("allow_from"))
	if allow == "" {
		allow = ratelimit.ClientIP(r)
	}
	s, err := h.tunnels.Open(d.ID, d.SerialNumber, h.currentUsername(r), allow)
	if err != nil {
		h.hxRedirect(w, r, "/adb?sel="+url.QueryEscape(serial)+"&flash="+url.QueryEscape("Couldn't open the tunnel: "+err.Error())+"&flash_type=error")
		return
	}
	h.auditDev(r, "device.adb_tunnel", d.ID, serial, fmt.Sprintf("opened port %d for %s", s.Port, s.AllowFrom))
	h.hxRedirect(w, r, "/adb?sel="+url.QueryEscape(serial))
}

// AdbTunnelAllow adds an IP or CIDR to a live session — the "Allow it" next to a refused
// connection, for offices whose adb host leaves through a different WAN address than
// the browser did.
func (h *Handler) AdbTunnelAllow(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id := r.PathValue("id")
	s, ok := h.tunnels.Get(id)
	if !ok {
		h.hxRedirect(w, r, "/adb?flash="+url.QueryEscape("That tunnel is no longer open.")+"&flash_type=error")
		return
	}
	ip := strings.TrimSpace(r.FormValue("allow_from"))
	if err := h.tunnels.AddAllow(id, ip); err != nil {
		h.hxRedirect(w, r, "/adb?sel="+url.QueryEscape(s.Serial)+"&flash="+url.QueryEscape(err.Error())+"&flash_type=error")
		return
	}
	h.auditDev(r, "device.adb_tunnel", s.DeviceID, s.Serial, fmt.Sprintf("port %d now also allows %s", s.Port, ip))
	h.hxRedirect(w, r, "/adb?sel="+url.QueryEscape(s.Serial))
}

// AdbTunnelEnd closes a tunnel session.
func (h *Handler) AdbTunnelEnd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	back := "/adb"
	if s, ok := h.tunnels.Get(id); ok {
		back += "?sel=" + url.QueryEscape(s.Serial)
		h.auditDev(r, "device.adb_tunnel", s.DeviceID, s.Serial, fmt.Sprintf("ended port %d", s.Port))
	}
	h.tunnels.Close(id, "ended by "+h.currentUsername(r))
	h.hxRedirect(w, r, back)
}
