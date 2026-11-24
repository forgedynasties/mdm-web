package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// APILockOffAll switches Lock off for every venue (admin key, 24 Nov 2026; user: "disable lock
// on all venues on prod", "add an admin api then"). A venue's lock is a standing rule in
// kiosk_policies, so this deletes the rules that reach venue devices (a venue's, a group's,
// the all-devices rule; a rule on one serial stays) and lets reconcileKiosk release the
// devices those rules had locked. A device locked by hand, or already switched to its own
// setting, is left as it is. ?dry=1 only lists the rules. The body must say
// {"confirm":"all venues"} unless it is a dry run.
func (h *Handler) APILockOffAll(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry") == "1"
	var body struct {
		Confirm string `json:"confirm"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	if !dry && body.Confirm != "all venues" {
		writeJSONError(w, http.StatusBadRequest, `body must be {"confirm": "all venues"} (or use ?dry=1)`)
		return
	}
	ctx := r.Context()
	rules, err := h.db.ListKioskPolicies(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	type ruleOut struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Target string `json:"target_type"`
	}
	var doomed, kept []ruleOut
	for _, p := range rules {
		o := ruleOut{ID: p.ID.String(), Name: p.Name, Target: p.TargetType}
		if p.TargetType == "device" {
			kept = append(kept, o)
			continue
		}
		doomed = append(doomed, o)
		if dry {
			continue
		}
		if err := h.db.DeleteKioskPolicy(ctx, p.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not delete rule "+p.Name)
			return
		}
	}
	changed := 0
	if !dry {
		changed = h.reconcileKiosk(ctx)
		_ = h.db.InsertAudit(ctx, "API key", "lock.off_all", "all venues",
			fmt.Sprintf("rules_deleted=%d devices_released=%d", len(doomed), changed))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"dry_run": dry, "rules_deleted": doomed, "rules_kept": kept, "devices_released": changed,
	})
}
