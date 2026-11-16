package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
)

// People and Lineup sync over the admin API (see db/sync_people_lineup.go).
//
//	GET  /api/v1/admin/sync/people-lineup              any server: the export (JSON)
//	POST /api/v1/admin/sync/people-lineup?dry_run=1    prod only: what the export would change
//	POST /api/v1/admin/sync/people-lineup              prod only: apply it (one transaction)
//
// The import sits behind the fleet reset's lock (fleet_reset.go): only on the server whose
// own PUBLIC_ORIGIN is mdm.aioapp.com, or with ALLOW_FLEET_RESET=1, so it can never write
// over the fleet server's people. The export carries password hashes: admin key only.

type peopleLineupExport struct {
	From string `json:"exported_from"`
	At   string `json:"exported_at"`
	*db.PeopleLineupBundle
}

// ExportPeopleLineup is GET /api/v1/admin/sync/people-lineup.
func (h *Handler) ExportPeopleLineup(w http.ResponseWriter, r *http.Request) {
	b, err := h.db.ExportPeopleLineup(r.Context())
	if err != nil {
		log.Printf("[sync] export: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, peopleLineupExport{From: ownHost(), At: time.Now().UTC().Format(time.RFC3339), PeopleLineupBundle: b})
}

// ImportPeopleLineup is POST /api/v1/admin/sync/people-lineup[?dry_run=1].
func (h *Handler) ImportPeopleLineup(w http.ResponseWriter, r *http.Request) {
	host := ownHost()
	if os.Getenv("ALLOW_FLEET_RESET") != "1" && (host == "" || host != fleetResetHost) {
		http.NotFound(w, r)
		return
	}
	var in peopleLineupExport
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.PeopleLineupBundle == nil || len(in.Tables) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send the export from GET /api/v1/admin/sync/people-lineup"})
		return
	}
	if in.From != "" && strings.EqualFold(in.From, host) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that export came from this server"})
		return
	}
	dry := r.URL.Query().Get("dry_run") == "1"
	rep, err := h.db.ImportPeopleLineup(r.Context(), in.PeopleLineupBundle, dry)
	if err != nil {
		log.Printf("[sync] import: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !dry {
		var parts []string
		for t, x := range rep {
			parts = append(parts, fmt.Sprintf("%s +%d ~%d -%d", t, x.Added, x.Changed, x.Removed))
		}
		sort.Strings(parts)
		detail := "from " + in.From + ": " + strings.Join(parts, ", ")
		log.Printf("[sync] people and Lineup %s", detail)
		if err := h.db.InsertAudit(r.Context(), "admin API", "sync.people_lineup", host, detail); err != nil {
			log.Printf("[sync] audit: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": host, "from": in.From, "dry_run": dry, "tables": rep})
}
