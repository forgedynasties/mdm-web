package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

// fleetResetHost is the one server that may empty itself without ALLOW_FLEET_RESET in its
// .env: prod, whose .env only devops can edit. A server knows its own name from its
// PUBLIC_ORIGIN (never from the request, whose Host anyone can set). Remove once every
// device has moved there.
const fleetResetHost = "mdm.aioapp.com"

// ownHost is this server's own name: the host of the first PUBLIC_ORIGIN in its .env.
func ownHost() string {
	first := strings.TrimSpace(strings.Split(os.Getenv("PUBLIC_ORIGIN"), ",")[0])
	if u, err := url.Parse(first); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return ""
}

// FleetReset is POST /api/v1/admin/fleet-reset (admin key): empties this server's fleet
// data (devices, venues, groups, history; see db/fleet_reset.go) so devices can be moved in
// one by one (16 Nov 2026, for mdm.aioapp.com). Three locks, because the same code runs on
// the fleet server:
//
//  1. It exists only where .env says ALLOW_FLEET_RESET=1, or on the server whose own
//     PUBLIC_ORIGIN is mdm.aioapp.com (404 everywhere else, the fleet server included).
//  2. {"dry_run": true} only counts. A real run must name this server, as its PUBLIC_ORIGIN
//     does: {"confirm": "mdm.aioapp.com"}.
//  3. It refuses while any device is connected or checked in within the last 10 minutes.
//
// The run is one transaction and is audited as fleet.reset.
func (h *Handler) FleetReset(w http.ResponseWriter, r *http.Request) {
	host := ownHost()
	if os.Getenv("ALLOW_FLEET_RESET") != "1" && (host == "" || host != fleetResetHost) {
		http.NotFound(w, r)
		return
	}
	var req struct {
		DryRun  bool   `json:"dry_run"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if host == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this server has no PUBLIC_ORIGIN, so it can't name itself"})
		return
	}
	ctx := r.Context()

	connected := len(h.hub.ConnectedIDs())
	recent, err := h.db.DevicesSeenSince(ctx, 10)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not count recent devices"})
		return
	}

	counts, err := h.db.FleetReset(ctx, true)
	if err != nil {
		log.Printf("[fleet-reset] count: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]any{
		"server":              host,
		"tables":              counts,
		"devices_connected":   connected,
		"devices_seen_10_min": recent,
		"to_run_send":         map[string]string{"confirm": host},
	}
	if req.DryRun {
		resp["dry_run"] = true
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if req.Confirm == "" || !strings.EqualFold(req.Confirm, host) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf(`a real run must name this server: {"confirm": %q}`, host)})
		return
	}
	if connected > 0 || recent > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":               "devices are still talking to this server; move them away first",
			"devices_connected":   connected,
			"devices_seen_10_min": recent,
		})
		return
	}
	removed, err := h.db.FleetReset(ctx, false)
	if err != nil {
		log.Printf("[fleet-reset] run: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var parts []string
	for t, n := range removed {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", t, n))
		}
	}
	sort.Strings(parts)
	log.Printf("[fleet-reset] %s: cleared %s", host, strings.Join(parts, ", "))
	if err := h.db.InsertAudit(ctx, "admin API", "fleet.reset", host, "cleared: "+strings.Join(parts, ", ")); err != nil {
		log.Printf("[fleet-reset] audit: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": host, "removed": removed})
}
