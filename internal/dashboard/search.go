package dashboard

import (
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mdm/internal/db"
)

// DeviceSearch is a generic serial type-ahead (not scoped to a group), used by the
// command builder's "specific devices" target to look devices up instead of typing serials.
// CmdkIndex returns the navigable entities (groups, restaurants) as JSON, so
// the Cmd-K palette can fuzzy-match them alongside pages and devices. Productions are
// deliberately excluded — they're a manufacturing concern, not a navigation target.
func (h *Handler) CmdkIndex(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Label string `json:"label"`
		Sub   string `json:"sub"`
		URL   string `json:"url"`
		Type  string `json:"type"`
	}
	out := []entry{}
	// Groups and venues go through the same filter as their list pages: a user with a
	// visibility limit sees only those holding one of their devices — the rest 404 for
	// them, so their names must not turn up here either.
	listGroups := h.db.ListGroups // hidden groups: the super admin's only
	if h.role(r) == "admin" {
		listGroups = h.db.ListAllGroups
	}
	if gs, err := listGroups(r.Context()); err == nil {
		for _, g := range h.visibleGroupList(r, gs) {
			out = append(out, entry{g.Name, fmt.Sprintf("%d device%s", g.DeviceCount, plural(g.DeviceCount)), "/groups/" + g.ID.String(), "Group"})
		}
	}
	if rs, err := h.db.ListRestaurants(r.Context()); err == nil {
		for _, rest := range h.visibleRestaurantList(r, rs) {
			out = append(out, entry{rest.Name, fmt.Sprintf("%d device%s", rest.DeviceCount, plural(rest.DeviceCount)), "/restaurants/" + rest.ID.String(), "Restaurant"})
		}
	}
	// People: anyone signed in may open a colleague's profile (owners have no Users area).
	if !roleIsCustomer(h.role(r)) {
		if us, err := h.db.ListUsers(r.Context()); err == nil {
			managesPeople := roleManagesUsers(h.role(r))
			for _, u := range us {
				// Customer accounts only for people who manage users (security audit,
				// 26 Oct 2026): this listed every owner and staff member to the whole team.
				if roleIsCustomer(u.Role) && !managesPeople {
					continue
				}
				sub := roleLabel(u.Role)
				if u.Username != u.DisplayName() {
					sub += " · " + u.Username
				}
				out = append(out, entry{u.DisplayName(), sub, "/users/" + u.ID.String() + "/profile", "User"})
			}
		}
	}
	// Builds the fleet is running, each a jump to the fleet filtered to it. Releases
	// are deliberately not indexed: dozens of version strings drowned out the devices
	// and sites people actually jump to. The palette keeps builds out of the way the
	// same way — it offers them only to a query that looks like a version.
	if bs, err := h.db.GetDistinctBuildIDs(r.Context()); err == nil {
		for _, b := range bs {
			out = append(out, entry{b, "Devices on this build", "/devices?build=" + url.QueryEscape(b), "Build"})
		}
	}
	// Private and short: the palette keeps its own copy for a tab and refreshes it in
	// the background, so this only spares a burst of opens the round trip.
	w.Header().Set("Cache-Control", "private, max-age=30")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *Handler) DeviceSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	// The Cmd-K palette asks for JSON; the HTMX type-ahead inputs want the HTML list.
	wantJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	acc := h.access(r)
	// ?serials=a,b,c: the palette's recent devices, looked up for their status.
	if list := r.URL.Query().Get("serials"); list != "" && wantJSON {
		var serials []string
		for _, s := range strings.Split(list, ",") {
			if s = strings.TrimSpace(s); s != "" && len(serials) < 10 {
				serials = append(serials, s)
			}
		}
		hits, err := h.db.DevicesBySerials(r.Context(), serials, acc.visibleIDs())
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.writeDeviceHits(w, hits)
		return
	}
	if query == "" {
		if wantJSON {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		h.tmpl.ExecuteTemplate(w, "device-search-results", map[string]any{"Query": "", "Devices": []db.Device{}})
		return
	}
	hits, err := h.db.SearchDevices(r.Context(), query, 8, acc.visibleIDs())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if wantJSON {
		h.writeDeviceHits(w, hits)
		return
	}
	devices := make([]db.Device, len(hits))
	for i, d := range hits {
		devices[i] = d.Device
	}
	h.tmpl.ExecuteTemplate(w, "device-search-results", map[string]any{"Query": query, "Devices": devices})
}

// writeDeviceHits is the palette's JSON for a device list: enough to draw a row
// (name, venue, type, build, online or how long offline) and to say why it matched.
func (h *Handler) writeDeviceHits(w http.ResponseWriter, hits []db.DeviceHit) {
	type hit struct {
		Serial     string    `json:"serial"`
		Nickname   string    `json:"nickname,omitempty"`
		Build      string    `json:"build,omitempty"`
		Online     bool      `json:"online"`
		LastSeen   time.Time `json:"last_seen"`
		Restaurant string    `json:"restaurant,omitempty"`
		ClassKey   string    `json:"class_key,omitempty"`
		Class      string    `json:"class,omitempty"`
		IP         string    `json:"ip,omitempty"`
		Rank       int       `json:"rank"` // how it matched; see db.SearchDevices
	}
	out := make([]hit, 0, len(hits))
	for _, d := range hits {
		e := hit{Serial: d.SerialNumber, Nickname: d.Nickname, Build: d.BuildID,
			Online: h.hub.IsConnectedForDisplay(d.ID), LastSeen: d.LastSeenAt,
			Restaurant: d.RestaurantName, Rank: d.Rank}
		if c := d.Class(); c != "" {
			e.ClassKey, e.Class = c, d.ClassLabel()
		}
		// The IP only when it is what matched, so a serial hit stays one line.
		if d.Rank == 1 || d.Rank == 4 {
			e.IP = d.IP
		}
		out = append(out, e)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
