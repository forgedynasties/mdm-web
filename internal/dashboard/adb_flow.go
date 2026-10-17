package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/ratelimit"
)

// ── Wireless adb: one connect flow (A1 + D2, picked 17 Oct) ───────────────────
//
// Both the /adb page and the device page's header pill start the same thing and watch the
// same progress: the adb_tcp command is sent, the device answers (or is offline and the
// step waits for it), adb is listening, and the tunnel is open — ending on the `adb connect`
// lines to copy. The tunnel opens automatically wherever the server has tunnels and the
// agent can take one, and 24 h is the default (the two questions on the demo were left to
// these defaults). The progress endpoint is polled; nothing on either page needs a reload.

// adbStep is one line of the stepper.
type adbStep struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	State  string `json:"state"` // pending | now | wait | ok | failed | skip
	Detail string `json:"detail,omitempty"`
}

// adbProgress is one device's connect progress.
type adbProgress struct {
	Serial   string    `json:"serial"`
	Name     string    `json:"name"`
	Venue    string    `json:"venue,omitempty"`
	Online   bool      `json:"online"`
	State    string    `json:"state"` // off | working | ready | failed | cant
	Word     string    `json:"word"`  // one line for the pill: "turning on…", "ready", …
	Steps    []adbStep `json:"steps"`
	Anywhere string    `json:"anywhere,omitempty"` // adb connect <tunnel host>:<port>
	LAN      string    `json:"lan,omitempty"`      // adb connect <device ip>:5555
	CanOn    bool      `json:"can_on"`             // this device can be connected from here
	Why      string    `json:"why,omitempty"`      // why not
	TunnelID string    `json:"tunnel_id,omitempty"`
	Refused  string    `json:"refused,omitempty"` // last address the tunnel turned away
}

// adbWants are the tunnels a Connect asked for that could not open yet: the tunnel manager
// only opens one while the device holds its live connection, and is pointless before adb
// listens. The progress poll opens them once both hold. In memory: a restart forgets them,
// and the next Connect asks again.
var adbWants = struct {
	sync.Mutex
	m map[uuid.UUID]adbWant
}{m: map[uuid.UUID]adbWant{}}

type adbWant struct {
	User, Allow string
	Until       time.Time
}

const adbWantFor = 2 * time.Hour

// adbTunnelHost is the host `adb connect` names for a tunnel: the configured one, else the
// host the dashboard was reached on.
func (h *Handler) adbTunnelHost(r *http.Request) string {
	host := h.tunnels.Host()
	if host == "" {
		host = r.Host
		if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
			host = host[:i]
		}
	}
	return host
}

// adbProgressFor works out each device's steps from its newest adb_tcp command, what it
// reports, whether it is connected, and its tunnel session.
func (h *Handler) adbProgressFor(r *http.Request, devices []*db.Device) []adbProgress {
	ids := make([]uuid.UUID, len(devices))
	for i, d := range devices {
		ids[i] = d.ID
	}
	latest, _ := h.db.LatestAdbTCPFor(r.Context(), ids)
	connected := h.hub.ConnectedIDsForDisplay()
	host := h.adbTunnelHost(r)
	out := make([]adbProgress, 0, len(devices))
	for _, d := range devices {
		_, online := connected[d.ID]
		row := h.adbRowFor(d, online, nil)
		p := adbProgress{Serial: d.SerialNumber, Name: d.SerialNumber, Venue: d.RestaurantName, Online: online,
			CanOn: row.CanSwitch || (d.AgentKind == "dpc" && row.Group == "open" && row.CanTunnel), Why: row.Why}
		l, has := latest[d.ID]
		listening := row.Group == "open"
		port := 5555

		// 1 · Sent.
		sent := adbStep{Key: "sent", Label: "Sent to the device", State: "pending"}
		// 2 · The device answered.
		ans := adbStep{Key: "answer", Label: "Device answered", State: "pending"}
		switch {
		case has && l.Port > 0:
			sent.State, sent.Detail = "ok", "by "+orDash(l.CreatedBy)+" · "+adbAgo(l.CreatedAt)
			switch l.Status {
			case "completed":
				ans.State, ans.Detail = "ok", "adb_tcp done · "+adbAgo(l.UpdatedAt)
			case "failed", "expired", "cancelled", "unsupported":
				ans.State, ans.Detail = "failed", firstNonEmpty(strings.TrimSpace(l.Output), l.Status)
			default:
				if !online {
					ans.State, ans.Detail = "wait", "offline — last seen "+adbAgo(d.LastSeenAt)+"; it runs when the device is back"
				} else {
					ans.State, ans.Detail = "now", "waiting for the device"
				}
			}
		case !row.CanSwitch && listening:
			// The image keeps adbd on the network (a Standard client device): nothing to send.
			sent.State, sent.Detail = "skip", "not needed — this image keeps adb listening"
			ans.State, ans.Detail = "skip", ""
		}
		// 3 · adb listening.
		lis := adbStep{Key: "listen", Label: "adb listening", State: "pending"}
		if listening {
			lis.State, lis.Detail = "ok", fmt.Sprintf("port %d", port)
			if has && l.Port > 0 && l.Status == "completed" {
				if l.Hours > 0 {
					if left := time.Until(l.UpdatedAt.Add(time.Duration(l.Hours) * time.Hour)); left > 0 {
						lis.Detail += " · closes in " + durShort(left)
					}
				} else {
					lis.Detail += " · stays on until turned off"
				}
			}
		} else if ans.State == "ok" {
			lis.State, lis.Detail = "now", "waiting for the device to report it"
		}
		// 4 · Tunnel.
		tun := adbStep{Key: "tunnel", Label: "Tunnel open", State: "pending"}
		switch {
		case !h.tunnels.Enabled():
			tun.State, tun.Detail = "skip", "tunnels are off on this server — same network only"
		case !row.CanTunnel:
			tun.State, tun.Detail = "skip", "needs "+map[bool]string{true: "firmware client 1.8.9+", false: "Standard client 0.2.9+"}[d.AgentKind == "firmware"]+" — same network only"
		default:
			adbWants.Lock()
			want, wanted := adbWants.m[d.ID]
			if wanted && time.Now().After(want.Until) {
				delete(adbWants.m, d.ID)
				wanted = false
			}
			adbWants.Unlock()
			if _, ok := h.tunnels.ForDevice(d.ID); !ok && wanted && listening && online {
				if s, err := h.tunnels.Open(d.ID, d.SerialNumber, want.User, want.Allow); err == nil {
					_ = h.db.InsertAuditDevice(r.Context(), want.User, "device.adb_tunnel", d.SerialNumber, fmt.Sprintf("opened port %d for %s", s.Port, s.AllowFrom), d.ID)
					adbWants.Lock()
					delete(adbWants.m, d.ID)
					adbWants.Unlock()
				}
			}
			if s, ok := h.tunnels.ForDevice(d.ID); ok {
				p.TunnelID = s.ID
				p.Refused = s.LastRefused()
				tun.Detail = fmt.Sprintf("%s:%d · closes after %s idle", host, s.Port, s.ClosesInText())
				if listening {
					tun.State = "ok"
					p.Anywhere = fmt.Sprintf("adb connect %s:%d", host, s.Port)
				} else {
					tun.State = "now"
					tun.Detail = fmt.Sprintf("port %d reserved · waiting for adb", s.Port)
				}
			} else if wanted && !online {
				tun.State, tun.Detail = "wait", "opens when the device is back on its live connection"
			} else if wanted {
				tun.Detail = "opens once adb is listening"
			} else if listening {
				tun.Detail = "not open — Connect opens one"
			}
		}
		if listening && row.IP != "" {
			p.LAN = fmt.Sprintf("adb connect %s:%d", row.IP, port)
		}
		p.Steps = []adbStep{sent, ans, lis, tun}

		turningOff := has && l.Port == 0 && listening && l.Status != "completed" && l.Status != "failed" && l.Status != "expired"
		switch {
		case turningOff:
			p.State, p.Word = "working", "turning off"
			p.Steps = []adbStep{{Key: "off", Label: "Turning adb off", State: map[bool]string{true: "now", false: "wait"}[online],
				Detail: map[bool]string{true: "waiting for the device", false: "offline — it switches off when the device is back"}[online]}}
		case ans.State == "failed":
			p.State, p.Word = "failed", "couldn't turn on"
		case listening && tun.State != "now":
			p.State, p.Word = "ready", "ready"
		case has && l.Port > 0 && !listening:
			p.State = "working"
			for _, s := range p.Steps {
				if s.State == "now" || s.State == "wait" {
					p.Word = strings.ToLower(s.Label[:1]) + s.Label[1:]
					if s.State == "wait" {
						p.Word = "waiting for the device"
					}
					break
				}
			}
			if p.Word == "" {
				p.Word = "turning on"
			}
		case listening:
			p.State, p.Word = "working", "opening the tunnel"
		case !p.CanOn:
			p.State, p.Word = "cant", "not available"
		default:
			p.State, p.Word = "off", "off"
		}
		out = append(out, p)
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// adbAgo is "just now" / "4 min ago" / "3 h ago".
func adbAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// adbSerialsFrom reads serials from a form or query: repeated `serial` and/or a comma list
// in `serials`, de-duplicated, at most 100.
func adbSerialsFrom(v url.Values) []string {
	var raw []string
	raw = append(raw, v["serial"]...)
	for _, s := range strings.Split(v.Get("serials"), ",") {
		raw = append(raw, s)
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == 100 {
			break
		}
	}
	return out
}

// AdbConnect turns wireless adb on for one or more devices and opens a tunnel for each that
// can take one. One adb_tcp per named device — never a group — as AdbBatch.
//
//	POST /adb/connect  serial=… (repeated) | serials=a,b  hours=24
//
// A JSON caller (the device page's pill) gets {sent, skipped}; a form post lands on /adb
// with the serials in the URL so the progress survives a reload.
func (h *Handler) AdbConnect(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	serials := adbSerialsFrom(r.Form)
	hours, err := strconv.Atoi(r.FormValue("hours"))
	if err != nil || hours < 0 || hours > 24*30 {
		hours = 24
	}
	type skip struct {
		Serial string `json:"serial"`
		Why    string `json:"why"`
	}
	var sent []string
	var skipped []skip
	payload, _ := json.Marshal(map[string]int{"port": 5555, "hours": hours})
	if len(serials) > 1 {
		payload = withBatch(payload, uuid.NewString())
	}
	user := h.currentUsername(r)
	for _, serial := range serials {
		d, err := h.db.GetDevice(r.Context(), serial)
		if err != nil {
			skipped = append(skipped, skip{serial, "not found"})
			continue
		}
		row := h.adbRowFor(d, false, nil)
		switch {
		case row.CanSwitch:
			cmd, err := h.db.CreateCommandBy(r.Context(), "adb_tcp", "", payload, "devices", []uuid.UUID{d.ID}, user)
			if err != nil {
				skipped = append(skipped, skip{serial, "couldn't queue the command"})
				continue
			}
			h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{d.ID})
			detail := fmt.Sprintf("on port 5555 for %d h", hours)
			if hours == 0 {
				detail = "on port 5555, stays on"
			}
			h.auditDev(r, "device.adb_tcp", d.ID, serial, detail)
		case row.Group == "open" && row.CanTunnel:
			// Already listening and we can't switch it (a Standard client device): tunnel only.
		default:
			why := row.Why
			if why == "" {
				why = "can't take wireless adb"
			}
			skipped = append(skipped, skip{serial, why})
			continue
		}
		// The tunnel opens from the progress poll once the device is connected and adb
		// listens (adbWants); a device already listening and connected gets it right away.
		if h.tunnels.Enabled() && row.CanTunnel {
			if _, ok := h.tunnels.ForDevice(d.ID); !ok {
				adbWants.Lock()
				adbWants.m[d.ID] = adbWant{User: user, Allow: ratelimit.ClientIP(r), Until: time.Now().Add(adbWantFor)}
				adbWants.Unlock()
				if row.Group == "open" && h.hub.IsConnected(d.ID) {
					if s, err := h.tunnels.Open(d.ID, d.SerialNumber, user, ratelimit.ClientIP(r)); err == nil {
						h.auditDev(r, "device.adb_tunnel", d.ID, serial, fmt.Sprintf("opened port %d for %s", s.Port, s.AllowFrom))
						adbWants.Lock()
						delete(adbWants.m, d.ID)
						adbWants.Unlock()
					}
				}
			}
		}
		sent = append(sent, serial)
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sent": sent, "skipped": skipped})
		return
	}
	q := url.Values{}
	if len(sent) > 0 {
		q.Set("connect", strings.Join(sent, ","))
	}
	if len(skipped) > 0 {
		var parts []string
		for _, s := range skipped {
			parts = append(parts, s.Serial+" ("+s.Why+")")
		}
		q.Set("flash", "Skipped: "+strings.Join(parts, "; "))
		q.Set("flash_type", "info")
	}
	h.hxRedirect(w, r, "/adb?"+q.Encode())
}

// AdbProgress is the connect progress for a set of devices, polled by both pages.
//
//	GET /adb/progress?serials=a,b
func (h *Handler) AdbProgress(w http.ResponseWriter, r *http.Request) {
	serials := adbSerialsFrom(r.URL.Query())
	var devices []*db.Device
	for _, s := range serials {
		if d, err := h.db.GetDevice(r.Context(), s); err == nil {
			devices = append(devices, d)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"devices": h.adbProgressFor(r, devices)})
}
