package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// APIAutoEnrollAll switches venue auto-enrol on (or off) for every venue (admin key, 25 Nov
// 2026; user: "on all venues on prod enable auto enroll", "add it to admin api"). Body
// {"on": true, "confirm": "all venues"}; ?dry=1 only lists venues, their setting and how
// many firmware scouts each has (a venue with none never scans, so auto-enrol does nothing).
func (h *Handler) APIAutoEnrollAll(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry") == "1"
	var body struct {
		On      *bool  `json:"on"`
		Confirm string `json:"confirm"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	if !dry && (body.On == nil || body.Confirm != "all venues") {
		writeJSONError(w, http.StatusBadRequest, `body must be {"on": true|false, "confirm": "all venues"} (or use ?dry=1)`)
		return
	}
	ctx := r.Context()
	var changed int64
	if !dry {
		n, err := h.db.SetAllVenuesAutoEnroll(ctx, *body.On)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		changed = n
		state := "off"
		if *body.On {
			state = "on"
		}
		_ = h.db.InsertAudit(ctx, "API key", "restaurant.auto_enroll_all", "all venues",
			fmt.Sprintf("auto_enroll=%s venues_changed=%d", state, n))
	}
	venues, err := h.db.ListVenueAutoEnroll(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	noScout := 0
	for _, v := range venues {
		if v.Scouts == 0 {
			noScout++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"dry_run": dry, "venues_changed": changed, "venues": venues, "venues_without_scout": noScout,
	})
}
