package dashboard

import (
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ws"
)

type Handler struct {
	db       *db.DB
	hub      *ws.Hub
	store    *sessions.CookieStore
	tmpl     *template.Template
	user     string
	password string
	cfg      *config.Config
}

func NewHandler(d *db.DB, hub *ws.Hub, sessionSecret, user, password string, cfg *config.Config) *Handler {
	store := sessions.NewCookieStore([]byte(sessionSecret))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
	}

	funcMap := template.FuncMap{
		"batteryClass": func(pct int) string {
			switch {
			case pct < 20:
				return "battery-low"
			case pct < 50:
				return "battery-mid"
			default:
				return "battery-ok"
			}
		},
		"formatTime": func(t time.Time) string {
			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
		"timeSince": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"batteryWidth": func(pct int) string {
			return fmt.Sprintf("%d%%", pct)
		},
		"shortTime": func(t time.Time) string {
			return t.UTC().Format("15:04")
		},
		"minuteOfDay": func(t time.Time) int {
			u := t.UTC()
			return u.Hour()*60 + u.Minute()
		},
		"reverseCheckins": func(checkins []db.Checkin) []db.Checkin {
			n := len(checkins)
			out := make([]db.Checkin, n)
			for i, c := range checkins {
				out[n-1-i] = c
			}
			return out
		},
		"statusClass": func(s string) string {
			switch s {
			case "installed", "completed":
				return "ok"
			case "failed":
				return "danger"
			default:
				return "muted"
			}
		},
		"cmdLabel": func(cmdType string) string {
			switch cmdType {
			case "install_apk":
				return "Install APK"
			case "shell":
				return "Shell"
			case "screenshot":
				return "Screenshot"
			case "reboot":
				return "Reboot"
			case "ota":
				return "OTA Update"
			default:
				return cmdType
			}
		},
		"logcatStatusClass": func(s string) string {
			switch s {
			case "fulfilled":
				return "ok"
			case "delivered":
				return "warn"
			default:
				return "muted"
			}
		},
		"rawJSON": func(b []byte) string { return string(b) },
		"ramUsage": func(raw json.RawMessage) map[string]int {
			if len(raw) == 0 {
				return nil
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil
			}
			v, ok := m["ram_usage_mb"]
			if !ok {
				return nil
			}
			var ram map[string]int
			if err := json.Unmarshal(v, &ram); err != nil {
				return nil
			}
			return ram
		},
		"deviceTime": func(raw json.RawMessage) string {
			if len(raw) == 0 {
				return ""
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return ""
			}
			v, ok := m["timezone"]
			if !ok {
				return ""
			}
			var tz string
			if err := json.Unmarshal(v, &tz); err != nil {
				return ""
			}
			// Map timezone string to offset
			var loc *time.Location
			switch strings.ToUpper(tz) {
			case "GMT", "UTC", "GMT+0", "GMT-0":
				loc = time.UTC
			default:
				// Try parsing as "GMT+N" or "GMT-N"
				if strings.HasPrefix(strings.ToUpper(tz), "GMT") {
					offset := strings.TrimPrefix(strings.ToUpper(tz), "GMT")
					if h, err := strconv.Atoi(offset); err == nil {
						loc = time.FixedZone(tz, h*3600)
					}
				}
			}
			if loc == nil {
				loc = time.UTC
			}
			return time.Now().In(loc).Format("15:04:05") + " (" + tz + ")"
		},
		"batteryTemp": func(raw json.RawMessage) string {
			if len(raw) == 0 {
				return ""
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return ""
			}
			v, ok := m["battery_temp_c"]
			if !ok {
				return ""
			}
			var temp float64
			if err := json.Unmarshal(v, &temp); err != nil {
				return ""
			}
			return fmt.Sprintf("%.1f°C", temp)
		},
		"tempClass": func(raw json.RawMessage) string {
			if len(raw) == 0 {
				return ""
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return ""
			}
			v, ok := m["battery_temp_c"]
			if !ok {
				return ""
			}
			var temp float64
			if err := json.Unmarshal(v, &temp); err != nil {
				return ""
			}
			switch {
			case temp >= 60:
				return "danger"
			case temp >= 45:
				return "warn"
			case temp <= -10:
				return "danger"
			case temp <= 0:
				return "warn"
			default:
				return "ok"
			}
		},
		"ramPct": func(ram map[string]int) int {
			total, ok := ram["total"]
			if !ok || total == 0 {
				return 0
			}
			used, ok := ram["used"]
			if !ok {
				return 0
			}
			return used * 100 / total
		},
		"wlcStatus": func(raw json.RawMessage) template.JS {
			if len(raw) == 0 {
				return "undefined"
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return "undefined"
			}
			v, ok := m["wlc_status"]
			if !ok {
				return "undefined"
			}
			var n int
			if err := json.Unmarshal(v, &n); err != nil {
				return "undefined"
			}
			if n != 0 {
				return "1"
			}
			return "0"
		},
		"extraField": func(raw []byte, key string) string {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return ""
			}
			v, ok := m[key]
			if !ok {
				return "—"
			}
			// strip quotes for plain strings
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				return s
			}
			return string(v)
		},
		"add":    func(a, b int) int { return a + b },
		"sub":    func(a, b int) int { return a - b },
		"div":    func(a, b int) int { return a / b },
		"hasBit": func(mask, bit int) bool { return mask&bit != 0 },
		"iter": func(start, end int) []int {
			var out []int
			for i := start; i <= end; i++ {
				out = append(out, i)
			}
			return out
		},
	}

	tmpl := template.Must(template.New("").Funcs(funcMap).ParseGlob("templates/*.html"))

	return &Handler{
		db:       d,
		hub:      hub,
		store:    store,
		tmpl:     tmpl,
		user:     user,
		password: password,
		cfg:      cfg,
	}
}

func (h *Handler) isAuthenticated(r *http.Request) bool {
	session, err := h.store.Get(r, "mdm-session")
	if err != nil {
		return false
	}
	auth, ok := session.Values["authenticated"].(bool)
	return ok && auth
}

func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.isAuthenticated(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if h.isAuthenticated(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	h.tmpl.ExecuteTemplate(w, "login.html", nil)
}

func (h *Handler) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	user := r.FormValue("username")
	pass := r.FormValue("password")

	userMatch := subtle.ConstantTimeCompare([]byte(user), []byte(h.user)) == 1
	passMatch := subtle.ConstantTimeCompare([]byte(pass), []byte(h.password)) == 1

	if !userMatch || !passMatch {
		h.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": "Invalid credentials"})
		return
	}

	session, _ := h.store.Get(r, "mdm-session")
	session.Values["authenticated"] = true
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	session, _ := h.store.Get(r, "mdm-session")
	session.Values["authenticated"] = false
	session.Options.MaxAge = -1
	session.Save(r, w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

const pageSize = 25

func (h *Handler) DeviceList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	sort := r.URL.Query().Get("sort")
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	offset := (page - 1) * pageSize

	devices, err := h.db.ListDevices(r.Context(), q, offset, pageSize, sort)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	total, err := h.db.CountDevices(r.Context(), q)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	summary, err := h.db.GetSummary(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	data := map[string]any{
		"Title":      "Devices",
		"Devices":    devices,
		"Total":      total,
		"Page":       page,
		"TotalPages": totalPages,
		"Query":      q,
		"PageSize":   pageSize,
		"Summary":    summary,
		"Sort":       sort,
	}

	if r.Header.Get("HX-Request") == "true" {
		h.tmpl.ExecuteTemplate(w, "device-table", data)
		return
	}
	h.tmpl.ExecuteTemplate(w, "devices.html", data)
}

func (h *Handler) DeviceDetail(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	const pageSize = 25
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	offset := (page - 1) * pageSize

	total, err := h.db.GetCheckinsCount(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
		offset = (page - 1) * pageSize
	}

	checkins, err := h.db.GetCheckinsPaged(r.Context(), device.ID, pageSize, offset)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	chartCheckins, err := h.db.GetCheckinsForDuration(r.Context(), device.ID, time.Now().UTC().Add(-48*time.Hour))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	commands, err := h.db.GetDeviceCommands(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	apps, err := h.db.ListApps(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	installedPkgs, err := h.db.GetDevicePackages(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	kioskCfg, err := h.db.GetOrCreateDeviceConfig(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	otaPkgs, err := h.db.ListOTAPackages(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	h.tmpl.ExecuteTemplate(w, "device.html", map[string]any{
		"Title":             device.SerialNumber,
		"Device":            device,
		"Checkins":          checkins,
		"ChartCheckins":     chartCheckins,
		"Commands":          commands,
		"ExtraColumns":      h.cfg.Columns(),
		"Apps":              apps,
		"CheckinPage":       page,
		"CheckinPages":      totalPages,
		"CheckinTotal":      total,
		"InstalledPackages": installedPkgs,
		"KioskConfig":       kioskCfg,
		"OTAPackages":       otaPkgs,
	})
}

func wlcStatusFromExtra(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m["wlc_status"]
	if !ok {
		return ""
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return ""
	}
	if n != 0 {
		return "charging"
	}
	return "not_charging"
}

func (h *Handler) DeviceBatteryCSV(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	hours := 48
	if h := r.URL.Query().Get("hours"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 && n <= 168 {
			hours = n
		}
	}

	checkins, err := h.db.GetCheckinsForDuration(r.Context(), device.ID, time.Now().UTC().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("%s_battery_%dh.csv", serial, hours)
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	cw := csv.NewWriter(w)
	cw.Write([]string{"timestamp", "battery_pct", "wlc_status"})
	for i := len(checkins) - 1; i >= 0; i-- {
		c := checkins[i]
		cw.Write([]string{
			c.CreatedAt.Format(time.RFC3339),
			strconv.Itoa(c.BatteryPct),
			wlcStatusFromExtra(c.Extra),
		})
	}
	cw.Flush()
}

func (h *Handler) DeviceStatsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	h.tmpl.ExecuteTemplate(w, "device-stats", map[string]any{
		"Device": device,
	})
}

func (h *Handler) DeviceCommandsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	commands, err := h.db.GetDeviceCommands(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "device-commands", map[string]any{
		"Device":   device,
		"Commands": commands,
	})
}

func (h *Handler) DeviceCheckinsPartial(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	const pageSize = 25
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	offset := (page - 1) * pageSize

	total, err := h.db.GetCheckinsCount(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
		offset = (page - 1) * pageSize
	}

	checkins, err := h.db.GetCheckinsPaged(r.Context(), device.ID, pageSize, offset)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	h.tmpl.ExecuteTemplate(w, "device-checkins", map[string]any{
		"Device":       device,
		"Checkins":     checkins,
		"ExtraColumns": h.cfg.Columns(),
		"CheckinPage":  page,
		"CheckinPages": totalPages,
		"CheckinTotal": total,
	})
}

// ── Groups ────────────────────────────────────────────────────────────────────

func (h *Handler) GroupList(w http.ResponseWriter, r *http.Request) {
	groups, err := h.db.ListGroups(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "groups.html", map[string]any{
		"Title":  "Groups",
		"Groups": groups,
	})
}

func (h *Handler) GroupDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	g, err := h.db.GetGroup(r.Context(), id)
	if err != nil {
		http.Error(w, "Group not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListGroupDevices(r.Context(), id)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	pkgs, err := h.db.ListOTAPackages(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "group_detail.html", map[string]any{
		"Title":       g.Name,
		"Group":       g,
		"Devices":     devices,
		"OTAPackages": pkgs,
	})
}

func (h *Handler) GroupSetOTA(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	pkgID, _ := strconv.Atoi(r.FormValue("ota_package_id")) // 0 = clear
	if err := h.db.SetGroupOTAPackage(r.Context(), id, pkgID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/groups/"+id.String(), http.StatusSeeOther)
}

func (h *Handler) GroupCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/groups", http.StatusFound)
		return
	}
	if _, err := h.db.CreateGroup(r.Context(), name); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/groups", http.StatusFound)
}

func (h *Handler) GroupDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteGroup(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/groups", http.StatusFound)
}

func (h *Handler) GroupAddDevice(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	serial := strings.TrimSpace(r.FormValue("serial_number"))
	if serial == "" {
		http.Redirect(w, r, "/groups/"+id.String(), http.StatusFound)
		return
	}
	if err := h.db.AddDeviceToGroup(r.Context(), serial, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/groups/"+id.String(), http.StatusFound)
}

func (h *Handler) GroupRemoveDevice(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid group ID", http.StatusBadRequest)
		return
	}
	serial := r.PathValue("serial")
	if err := h.db.RemoveDeviceFromGroup(r.Context(), serial, id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/groups/"+id.String(), http.StatusFound)
}

func (h *Handler) DeviceSetOTA(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	pkgID, _ := strconv.Atoi(r.FormValue("ota_package_id")) // 0 = clear
	if err := h.db.SetDeviceOTAPackage(r.Context(), serial, pkgID); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/devices/"+serial, http.StatusSeeOther)
}

// ── OTA Updates ───────────────────────────────────────────────────────────────

func (h *Handler) Updates(w http.ResponseWriter, r *http.Request) {
	pkgs, err := h.db.ListOTAPackages(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "updates.html", map[string]any{
		"Title":    "Updates",
		"Packages": pkgs,
	})
}

func (h *Handler) UpdateCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	buildID := strings.TrimSpace(r.FormValue("build_id"))
	updateURL := strings.TrimSpace(r.FormValue("update_url"))
	headersRaw := r.FormValue("payload_headers")
	releaseDateStr := r.FormValue("release_date")

	if buildID == "" || updateURL == "" {
		http.Error(w, "build_id and update_url are required", http.StatusBadRequest)
		return
	}
	offset, err := strconv.ParseInt(r.FormValue("payload_offset"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid payload_offset", http.StatusBadRequest)
		return
	}
	size, err := strconv.ParseInt(r.FormValue("payload_size"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid payload_size", http.StatusBadRequest)
		return
	}

	var headers []string
	for _, line := range strings.Split(strings.ReplaceAll(headersRaw, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			headers = append(headers, line)
		}
	}

	releaseDate := time.Now().UTC()
	if releaseDateStr != "" {
		if t, err := time.Parse("2006-01-02T15:04", releaseDateStr); err == nil {
			releaseDate = t.UTC()
		}
	}

	if _, err := h.db.CreateOTAPackage(r.Context(), buildID, updateURL, offset, size, headers, releaseDate); err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/updates", http.StatusSeeOther)
}

func (h *Handler) UpdateDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteOTAPackage(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/updates", http.StatusSeeOther)
}

// ── Commands ──────────────────────────────────────────────────────────────────

func (h *Handler) CommandList(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.db.ListCommands(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	groups, err := h.db.ListGroups(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	apps, err := h.db.ListApps(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "commands.html", map[string]any{
		"Title":    "Commands",
		"Commands": cmds,
		"Groups":   groups,
		"Apps":     apps,
	})
}

func (h *Handler) CommandStatusPartial(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "command-deliveries", map[string]any{
		"Command":    cmd,
		"Deliveries": deliveries,
	})
}

func (h *Handler) CommandDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteCommand(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/commands", http.StatusFound)
}

func (h *Handler) CommandDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid command ID", http.StatusBadRequest)
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil {
		http.Error(w, "Command not found", http.StatusNotFound)
		return
	}
	deliveries, err := h.db.GetCommandDeliveries(r.Context(), id)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "command_detail.html", map[string]any{
		"Title":      "Command " + id.String()[:8],
		"Command":    cmd,
		"Deliveries": deliveries,
	})
}

func (h *Handler) CommandCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}
	targetType := r.FormValue("target_type")
	if targetType != "all" && targetType != "devices" && targetType != "groups" {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}

	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if cmdType == "install_apk" && apkURL == "" {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}

	payload := buildPayload(cmdType, r)

	var targetIDs []uuid.UUID
	switch targetType {
	case "devices":
		serials := db.ParseSerials(r.FormValue("target_serials"))
		ids, err := h.db.GetDeviceIDsBySerials(r.Context(), serials)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		targetIDs = ids
	case "groups":
		for _, gid := range r.Form["target_groups"] {
			id, err := uuid.Parse(gid)
			if err != nil {
				continue
			}
			targetIDs = append(targetIDs, id)
		}
	}

	cmd, err := h.db.CreateCommand(r.Context(), cmdType, apkURL, payload, targetType, targetIDs)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), cmd, targetType, targetIDs)
	http.Redirect(w, r, "/commands", http.StatusFound)
}

func buildPayload(cmdType string, r *http.Request) json.RawMessage {
	switch cmdType {
	case "shell":
		cmd := strings.TrimSpace(r.FormValue("shell_cmd"))
		b, _ := json.Marshal(map[string]string{"cmd": cmd})
		return json.RawMessage(b)
	default:
		return json.RawMessage("{}")
	}
}

// ── Setup ─────────────────────────────────────────────────────────────────────

func (h *Handler) SetupPage(w http.ResponseWriter, r *http.Request) {
	apps, err := h.db.ListApps(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "setup.html", map[string]any{
		"Title": "Setup",
		"Apps":  apps,
	})
}

func (h *Handler) SetupCreateApp(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name   := strings.TrimSpace(r.FormValue("name"))
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if name == "" || apkURL == "" {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if _, err := h.db.CreateApp(r.Context(), name, apkURL); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/setup", http.StatusFound)
}

func (h *Handler) SetupCreateAppJSON(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name   := strings.TrimSpace(r.FormValue("name"))
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if name == "" || apkURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	app, err := h.db.CreateApp(r.Context(), name, apkURL)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app)
}

func (h *Handler) SetupDeleteApp(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteApp(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/setup", http.StatusFound)
}

// ── Settings ──────────────────────────────────────────────────────────────────

func (h *Handler) SettingsPage(w http.ResponseWriter, r *http.Request) {
	h.tmpl.ExecuteTemplate(w, "settings.html", map[string]any{
		"Title":        "Settings",
		"ExtraColumns": h.cfg.Columns(),
	})
}

func (h *Handler) SettingsAddColumn(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	key := strings.TrimSpace(r.FormValue("key"))
	label := strings.TrimSpace(r.FormValue("label"))
	if key == "" || label == "" {
		http.Redirect(w, r, "/settings", http.StatusFound)
		return
	}
	h.cfg.Add(config.ExtraColumn{Key: key, Label: label})
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (h *Handler) SettingsRemoveColumn(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	h.cfg.Remove(key)
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (h *Handler) LogcatPage(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	entries, err := h.db.GetLogcatEntriesForDevice(r.Context(), device.ID, 20)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	hasPending := false
	for _, e := range entries {
		if e.Request.Status == "pending" || e.Request.Status == "delivered" {
			hasPending = true
			break
		}
	}

	h.tmpl.ExecuteTemplate(w, "logcat.html", map[string]any{
		"Title":      device.SerialNumber + " — Logcat",
		"Device":     device,
		"Entries":    entries,
		"HasPending": hasPending,
	})
}

func (h *Handler) LogcatRefresh(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	entries, err := h.db.GetLogcatEntriesForDevice(r.Context(), device.ID, 20)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	hasPending := false
	for _, e := range entries {
		if e.Request.Status == "pending" || e.Request.Status == "delivered" {
			hasPending = true
			break
		}
	}

	h.tmpl.ExecuteTemplate(w, "logcat-entries", map[string]any{
		"Device":     device,
		"Entries":    entries,
		"HasPending": hasPending,
	})
}

func (h *Handler) LogcatRequestCreate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	r.ParseForm()
	level := r.FormValue("level")
	if level != "V" && level != "D" && level != "I" && level != "W" && level != "E" {
		level = "W"
	}
	lines := 500
	if n, err := strconv.Atoi(r.FormValue("lines")); err == nil && n > 0 && n <= 5000 {
		lines = n
	}
	tag := strings.TrimSpace(r.FormValue("tag"))

	req, err := h.db.CreateLogcatRequest(r.Context(), device.ID, level, lines, tag)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushLogcatRequest(r.Context(), req)

	http.Redirect(w, r, "/devices/"+serial+"/logcat", http.StatusFound)
}

func (h *Handler) DeviceCommandCreate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	cmdType := r.FormValue("type")
	if cmdType == "" {
		cmdType = "install_apk"
	}

	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	if cmdType == "install_apk" && apkURL == "" {
		http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
		return
	}

	payload := buildPayload(cmdType, r)

	cmd, err := h.db.CreateCommand(r.Context(), cmdType, apkURL, payload, "devices", []uuid.UUID{device.ID})
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{device.ID})
	http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
}

func (h *Handler) DeviceSetPollInterval(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	ms, err := strconv.Atoi(r.FormValue("poll_interval_ms"))
	if err != nil || ms < 5000 {
		http.Error(w, "poll_interval_ms must be >= 5000", http.StatusBadRequest)
		return
	}
	if err := h.db.SetDevicePollInterval(r.Context(), serial, ms); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
}

func (h *Handler) DeviceKioskUpdate(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()

	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}

	enabled := r.FormValue("kiosk_enabled") == "1"
	pkg := strings.TrimSpace(r.FormValue("kiosk_package"))

	if err := h.db.SetKioskConfig(r.Context(), device.ID, enabled, pkg, 0); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/devices/"+serial, http.StatusFound)
}

// ── Packages ──────────────────────────────────────────────────────────────────

func (h *Handler) FleetPackages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	pkgs, err := h.db.SearchFleetPackages(r.Context(), q)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "packages.html", map[string]any{
		"Title":    "Package Inventory",
		"Packages": pkgs,
		"Query":    q,
	})
}

func (h *Handler) DevicePackages(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	pkgs, err := h.db.GetDevicePackages(r.Context(), device.ID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.tmpl.ExecuteTemplate(w, "device_packages.html", map[string]any{
		"Title":    serial + " — Packages",
		"Device":   device,
		"Packages": pkgs,
	})
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.LoginPage)
	mux.HandleFunc("POST /login", h.LoginSubmit)
	mux.HandleFunc("POST /logout", h.Logout)

	mux.HandleFunc("GET /{$}", h.requireAuth(h.DeviceList))
	mux.HandleFunc("GET /devices/{serial}", h.requireAuth(h.DeviceDetail))
	mux.HandleFunc("GET /devices/{serial}/stats", h.requireAuth(h.DeviceStatsPartial))
	mux.HandleFunc("GET /devices/{serial}/battery.csv", h.requireAuth(h.DeviceBatteryCSV))
	mux.HandleFunc("GET /devices/{serial}/commands-status", h.requireAuth(h.DeviceCommandsPartial))
	mux.HandleFunc("GET /devices/{serial}/checkins-live", h.requireAuth(h.DeviceCheckinsPartial))
	mux.HandleFunc("POST /devices/{serial}/commands", h.requireAuth(h.DeviceCommandCreate))
	mux.HandleFunc("POST /devices/{serial}/poll-interval", h.requireAuth(h.DeviceSetPollInterval))
	mux.HandleFunc("POST /devices/{serial}/kiosk", h.requireAuth(h.DeviceKioskUpdate))
	mux.HandleFunc("POST /devices/{serial}/ota", h.requireAuth(h.DeviceSetOTA))
	mux.HandleFunc("GET /devices/{serial}/packages", h.requireAuth(h.DevicePackages))
	mux.HandleFunc("GET /devices/{serial}/logcat", h.requireAuth(h.LogcatPage))
	mux.HandleFunc("GET /devices/{serial}/logcat/entries", h.requireAuth(h.LogcatRefresh))
	mux.HandleFunc("POST /devices/{serial}/logcat", h.requireAuth(h.LogcatRequestCreate))

	mux.HandleFunc("GET /groups", h.requireAuth(h.GroupList))
	mux.HandleFunc("POST /groups", h.requireAuth(h.GroupCreate))
	mux.HandleFunc("GET /groups/{id}", h.requireAuth(h.GroupDetail))
	mux.HandleFunc("POST /groups/{id}/delete", h.requireAuth(h.GroupDelete))
	mux.HandleFunc("POST /groups/{id}/devices", h.requireAuth(h.GroupAddDevice))
	mux.HandleFunc("POST /groups/{id}/devices/{serial}/remove", h.requireAuth(h.GroupRemoveDevice))
	mux.HandleFunc("POST /groups/{id}/ota", h.requireAuth(h.GroupSetOTA))

	mux.HandleFunc("GET /commands", h.requireAuth(h.CommandList))
	mux.HandleFunc("POST /commands", h.requireAuth(h.CommandCreate))
	mux.HandleFunc("GET /commands/{id}", h.requireAuth(h.CommandDetail))
	mux.HandleFunc("GET /commands/{id}/status", h.requireAuth(h.CommandStatusPartial))
	mux.HandleFunc("POST /commands/{id}/delete", h.requireAuth(h.CommandDelete))

	mux.HandleFunc("GET /settings", h.requireAuth(h.SettingsPage))
	mux.HandleFunc("POST /settings/columns/add", h.requireAuth(h.SettingsAddColumn))
	mux.HandleFunc("POST /settings/columns/{key}/remove", h.requireAuth(h.SettingsRemoveColumn))

	mux.HandleFunc("GET /setup", h.requireAuth(h.SetupPage))
	mux.HandleFunc("POST /setup/apps", h.requireAuth(h.SetupCreateApp))
	mux.HandleFunc("POST /setup/apps/create", h.requireAuth(h.SetupCreateAppJSON))
	mux.HandleFunc("POST /setup/apps/{id}/delete", h.requireAuth(h.SetupDeleteApp))

	mux.HandleFunc("GET /packages", h.requireAuth(h.FleetPackages))

	mux.HandleFunc("GET /updates", h.requireAuth(h.Updates))
	mux.HandleFunc("POST /updates", h.requireAuth(h.UpdateCreate))
	mux.HandleFunc("POST /updates/{id}/delete", h.requireAuth(h.UpdateDelete))
}

// ── WS push helpers ───────────────────────────────────────────────────────────

func (h *Handler) pushCommand(ctx context.Context, cmd *db.Command, targetType string, targetIDs []uuid.UUID) {
	msg, _ := json.Marshal(map[string]any{
		"type":         "command",
		"id":           cmd.ID,
		"command_type": cmd.Type,
		"apk_url":      cmd.ApkURL,
		"payload":      cmd.Payload,
	})

	switch targetType {
	case "all":
		h.hub.Broadcast(msg)
		return
	case "groups":
		ids, err := h.db.GetDeviceIDsByGroupIDs(ctx, targetIDs)
		if err != nil {
			return
		}
		targetIDs = ids
	}

	for _, deviceID := range targetIDs {
		if !h.hub.Push(deviceID, msg) {
			continue
		}
		if cmd.Type == "reboot" {
			_ = h.db.AckCommand(ctx, cmd.ID, deviceID, "completed")
		} else {
			_ = h.db.MarkCommandsDelivered(ctx, deviceID, []uuid.UUID{cmd.ID})
		}
	}
}

func (h *Handler) pushLogcatRequest(ctx context.Context, req *db.LogcatRequest) {
	msg, _ := json.Marshal(map[string]any{
		"type":  "logcat_request",
		"id":    req.ID,
		"level": req.Level,
		"lines": req.Lines,
		"tag":   req.Tag,
	})
	if h.hub.Push(req.DeviceID, msg) {
		_ = h.db.MarkLogcatRequestsDelivered(ctx, []uuid.UUID{req.ID})
	}
}
