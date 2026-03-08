package dashboard

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"mdm/internal/db"
)

type Handler struct {
	db       *db.DB
	store    *sessions.CookieStore
	tmpl     *template.Template
	user     string
	password string
}

func NewHandler(d *db.DB, sessionSecret, user, password string) *Handler {
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
			case "installed":
				return "ok"
			case "failed":
				return "danger"
			default:
				return "muted"
			}
		},
		"add":  func(a, b int) int { return a + b },
		"sub":  func(a, b int) int { return a - b },
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
		store:    store,
		tmpl:     tmpl,
		user:     user,
		password: password,
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
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	offset := (page - 1) * pageSize

	devices, err := h.db.ListDevices(r.Context(), q, offset, pageSize)
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

	checkins, err := h.db.GetCheckins(r.Context(), device.ID, 100)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	h.tmpl.ExecuteTemplate(w, "device.html", map[string]any{
		"Title":    device.SerialNumber,
		"Device":   device,
		"Checkins": checkins,
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
	h.tmpl.ExecuteTemplate(w, "group_detail.html", map[string]any{
		"Title":   g.Name,
		"Group":   g,
		"Devices": devices,
	})
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
	h.tmpl.ExecuteTemplate(w, "commands.html", map[string]any{
		"Title":    "Commands",
		"Commands": cmds,
		"Groups":   groups,
	})
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
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	targetType := r.FormValue("target_type")
	if apkURL == "" || (targetType != "all" && targetType != "devices" && targetType != "groups") {
		http.Redirect(w, r, "/commands", http.StatusFound)
		return
	}

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

	if _, err := h.db.CreateCommand(r.Context(), apkURL, targetType, targetIDs); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/commands", http.StatusFound)
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.LoginPage)
	mux.HandleFunc("POST /login", h.LoginSubmit)
	mux.HandleFunc("POST /logout", h.Logout)

	mux.HandleFunc("GET /{$}", h.requireAuth(h.DeviceList))
	mux.HandleFunc("GET /devices/{serial}", h.requireAuth(h.DeviceDetail))

	mux.HandleFunc("GET /groups", h.requireAuth(h.GroupList))
	mux.HandleFunc("POST /groups", h.requireAuth(h.GroupCreate))
	mux.HandleFunc("GET /groups/{id}", h.requireAuth(h.GroupDetail))
	mux.HandleFunc("POST /groups/{id}/delete", h.requireAuth(h.GroupDelete))
	mux.HandleFunc("POST /groups/{id}/devices", h.requireAuth(h.GroupAddDevice))
	mux.HandleFunc("POST /groups/{id}/devices/{serial}/remove", h.requireAuth(h.GroupRemoveDevice))

	mux.HandleFunc("GET /commands", h.requireAuth(h.CommandList))
	mux.HandleFunc("POST /commands", h.requireAuth(h.CommandCreate))
	mux.HandleFunc("GET /commands/{id}", h.requireAuth(h.CommandDetail))
}
