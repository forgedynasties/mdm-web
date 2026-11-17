package dashboard

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// The AIO backend placements over the admin key (17 Nov 2026; user: "fix it then keeping
// backend source of truth", "make sure all linkable restaurants are linked", "venues needed
// to be created create them"). The same handlers as Enrollment › From the AIO backend's
// buttons, run as a super admin named "admin API", so mdm.dev and prod can be set straight
// without a browser:
//
//	GET  /api/v1/admin/backend/placements         backend restaurants that list devices but have no venue
//	POST /api/v1/admin/backend/place              action=create|link|ignore, env, rid[, venue]
//	POST /api/v1/admin/backend/move/{id}          move to venue id the devices its restaurant lists

// asAdminAPI runs next as a super admin session named "admin API", when the request carries
// the admin key; 404 otherwise.
func (h *Handler) asAdminAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-API-Key")
		if h.adminAPIKey == "" || k == "" || subtle.ConstantTimeCompare([]byte(k), []byte(h.adminAPIKey)) != 1 {
			http.NotFound(w, r)
			return
		}
		s := &db.Session{Username: "admin API", Role: "admin"}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxSessionKey{}, s)))
	}
}

// AdminBackendPlacements is the list behind From the AIO backend's To act on view, as JSON.
func (h *Handler) AdminBackendPlacements(w http.ResponseWriter, r *http.Request) {
	p, venues, err := h.loadPlacements(r.Context())
	if err != nil {
		http.Error(w, "Could not read the backend copy", http.StatusInternalServerError)
		return
	}
	type dev struct{ Serial, Label, Venue string }
	type row struct {
		Env   string `json:"env"`
		ID    int64  `json:"rid"`
		Name  string `json:"name"`
		City  string `json:"city"`
		Test  bool   `json:"test"`
		Total int    `json:"backend_devices"`
		Devs  []dev  `json:"devices"`
	}
	out := struct {
		Rows     []row                     `json:"rows"`
		Moves    map[string][]dev          `json:"moves"` // linked venue → its restaurant's devices that are elsewhere
		Unlinked []map[string]string       `json:"unlinked_venues"`
		Links    map[string]map[string]any `json:"links"`
	}{Moves: map[string][]dev{}, Links: map[string]map[string]any{}}
	for _, x := range p.Rows {
		rw := row{Env: x.R.Env, ID: x.R.ID, Name: x.R.Name, City: x.R.City, Test: x.Test, Total: x.Total}
		for _, d := range x.Devs {
			rw.Devs = append(rw.Devs, dev{d.Serial, d.Label, d.Venue})
		}
		out.Rows = append(out.Rows, rw)
	}
	for id, ds := range p.Moves {
		for _, d := range ds {
			out.Moves[id.String()] = append(out.Moves[id.String()], dev{d.Serial, d.Label, d.Venue})
		}
	}
	for _, v := range venues {
		if l, ok, _ := h.db.VenueBackendLinkOf(r.Context(), v.ID); ok {
			out.Links[v.ID.String()] = map[string]any{"name": v.Name, "env": l.Env, "rid": l.RestaurantID}
		} else {
			out.Unlinked = append(out.Unlinked, map[string]string{"id": v.ID.String(), "name": v.Name})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// AdminBackendMove is RestaurantBackendMove for a venue id in the path, all devices.
func (h *Handler) AdminBackendMove(w http.ResponseWriter, r *http.Request) {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		http.Error(w, "Invalid venue id", http.StatusBadRequest)
		return
	}
	h.RestaurantBackendMove(w, r)
}
