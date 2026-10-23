package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mdm/internal/db"
)

// Display preferences per person (24 Oct 2026; user: "display defaults each person"):
// rows per page, default sort, density and time format are set in My profile; anything
// left on "fleet default" follows Settings › Display.

type displayPrefsEntry struct {
	at time.Time
	p  db.DisplayPrefs
}

var displayPrefsCache sync.Map // username -> displayPrefsEntry

const displayPrefsTTL = time.Minute

// displayPrefs is the viewer's stored preferences (cached a minute; saving clears it).
func (h *Handler) displayPrefs(r *http.Request) db.DisplayPrefs {
	user := h.currentUsername(r)
	if user == "" {
		return db.DisplayPrefs{}
	}
	if v, ok := displayPrefsCache.Load(user); ok {
		if e := v.(displayPrefsEntry); time.Since(e.at) < displayPrefsTTL {
			return e.p
		}
	}
	p, _ := h.db.UserDisplayPrefs(r.Context(), user)
	displayPrefsCache.Store(user, displayPrefsEntry{at: time.Now(), p: p})
	return p
}

// effectiveDisplay is what the viewer sees: their own choice, else the fleet default.
type effectiveDisplay struct {
	PageSize int
	Sort     string
	Density  string
	Use24    bool
}

func (h *Handler) effectiveDisplay(r *http.Request) effectiveDisplay {
	e := effectiveDisplay{PageSize: h.cfg.PageSize(), Sort: h.cfg.DefaultSort(), Density: h.cfg.Density(), Use24: h.cfg.Use24Hour()}
	p := h.displayPrefs(r)
	if p.PageSize != nil {
		e.PageSize = *p.PageSize
	}
	if p.Sort != nil {
		e.Sort = *p.Sort
	}
	if p.Density != nil {
		e.Density = *p.Density
	}
	if p.Use24 != nil {
		e.Use24 = *p.Use24
	}
	return e
}

// profileDisplay is the My profile Display card: the viewer's choices and the fleet
// defaults they fall back to. Nil when viewing someone else.
func (h *Handler) profileDisplay(r *http.Request, viewingOther bool) map[string]any {
	if viewingOther {
		return nil
	}
	p := h.displayPrefs(r)
	m := map[string]any{
		"PageSize": "", "Sort": "", "Density": "", "Time": "",
		"FleetPageSize": h.cfg.PageSize(), "FleetSort": sortLabels[h.cfg.DefaultSort()],
		"FleetDensity": map[string]string{"compact": "Compact"}[h.cfg.Density()], "FleetTime": map[bool]string{true: "24-hour", false: "12-hour"}[h.cfg.Use24Hour()],
		"Sorts": sortOrder, "SortLabels": sortLabels,
	}
	if m["FleetDensity"] == "" {
		m["FleetDensity"] = "Comfortable"
	}
	if p.PageSize != nil {
		m["PageSize"] = strconv.Itoa(*p.PageSize)
	}
	if p.Sort != nil {
		m["Sort"] = *p.Sort
	}
	if p.Density != nil {
		m["Density"] = *p.Density
	}
	if p.Use24 != nil {
		m["Time"] = map[bool]string{true: "24", false: "12"}[*p.Use24]
	}
	return m
}

var sortOrder = []string{"last_seen", "created_at", "serial", "build", "battery", "ram", "temp"}

var sortLabels = map[string]string{"last_seen": "Last seen", "created_at": "First seen", "serial": "Serial number", "build": "Release version", "battery": "Battery", "ram": "RAM", "temp": "Temperature"}

var displaySorts = map[string]bool{"last_seen": true, "created_at": true, "serial": true, "build": true, "battery": true, "ram": true, "temp": true}

// ProfileDisplaySave is POST /profile/display: the viewer's own preferences; "" for a
// field means the fleet default.
func (h *Handler) ProfileDisplaySave(w http.ResponseWriter, r *http.Request) {
	user := h.currentUsername(r)
	if user == "" {
		http.Error(w, "Not signed in", http.StatusUnauthorized)
		return
	}
	var p db.DisplayPrefs
	if v := strings.TrimSpace(r.FormValue("page_size")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 200 {
			p.PageSize = &n
		}
	}
	if v := r.FormValue("default_sort"); displaySorts[v] {
		p.Sort = &v
	}
	if v := r.FormValue("density"); v == "comfortable" || v == "compact" {
		p.Density = &v
	}
	switch r.FormValue("time_format") {
	case "12":
		f := false
		p.Use24 = &f
	case "24":
		t := true
		p.Use24 = &t
	}
	if err := h.db.SetUserDisplayPrefs(r.Context(), user, p); err != nil {
		http.Error(w, "Could not save", http.StatusInternalServerError)
		return
	}
	displayPrefsCache.Delete(user)
	http.Redirect(w, r, flashURL("/profile#display", "Display preferences saved", "success"), http.StatusSeeOther)
}
