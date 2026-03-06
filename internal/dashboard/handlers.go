package dashboard

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"net/http"
	"time"

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

func (h *Handler) DeviceList(w http.ResponseWriter, r *http.Request) {
	devices, err := h.db.ListDevices(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Title":   "Devices",
		"Devices": devices,
		"Count":   len(devices),
	}

	// HTMX partial refresh — return only the table body
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

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", h.LoginPage)
	mux.HandleFunc("POST /login", h.LoginSubmit)
	mux.HandleFunc("POST /logout", h.Logout)
	mux.HandleFunc("GET /{$}", h.requireAuth(h.DeviceList))
	mux.HandleFunc("GET /devices/{serial}", h.requireAuth(h.DeviceDetail))
}
