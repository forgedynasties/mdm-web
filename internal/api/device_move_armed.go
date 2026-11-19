package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Arming venues (19 Nov 2026, see db/device_move_arm.go), on mdm.dev only:
//
//	POST /api/v1/admin/move/arm[?venues=all|id,id][&exclude=name,name][&confirm=arm][&dry=1]
//	POST /api/v1/admin/move/unarm?venues=all|id,id[&cancel=1]
//
// arm is a dry run unless confirm=arm is sent (dry=1 forces one). For each venue except the
// excluded names (default: AIO Lab) that has devices in view: every device without a move
// sent or arrived gets one filed (imported on prod, pending), then the venue and its devices
// are hidden. Nothing is hidden for a venue where a device could not be filed. Safe to
// repeat: it files only what is missing and hides what is still in view.

var defaultArmExclude = []string{"AIO Lab"}

type armDeviceOut struct {
	Serial string `json:"serial"`
	Kind   string `json:"agent_kind"`
	Client string `json:"client_version"`
	Move   string `json:"move_state,omitempty"` // what is filed now
	Action string `json:"action"`               // file | refile | keep (pending/sent/arrived)
	Error  string `json:"error,omitempty"`
}

type armVenueOut struct {
	VenueID  uuid.UUID      `json:"venue_id"`
	Venue    string         `json:"venue"`
	Hidden   bool           `json:"hidden"`
	Skipped  string         `json:"skipped,omitempty"`
	Devices  []armDeviceOut `json:"devices,omitempty"`
	Counts   map[string]int `json:"counts,omitempty"`
	Kinds    map[string]int `json:"kinds,omitempty"`
	Clients  map[string]int `json:"clients,omitempty"`
	Errors   int            `json:"errors,omitempty"`
	Done     bool           `json:"done,omitempty"` // hidden by this call
	WillHide bool           `json:"will_hide,omitempty"`
}

// parseVenueIDs reads venues=all|id,id. all = nil, true.
func parseVenueIDs(raw string) (ids []uuid.UUID, all bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return nil, true, nil
	}
	for _, p := range strings.Split(raw, ",") {
		id, err := uuid.Parse(strings.TrimSpace(p))
		if err != nil {
			return nil, false, fmt.Errorf("invalid venue id %q", p)
		}
		ids = append(ids, id)
	}
	return ids, false, nil
}

func armAction(state string) string {
	switch state {
	case db.MovePending, db.MoveSent, db.MoveArrived:
		return "keep"
	case "":
		return "file"
	}
	return "refile"
}

// MoveArm is POST /api/v1/admin/move/arm.
func (h *Handler) MoveArm(w http.ResponseWriter, r *http.Request) {
	target := deviceMoveTarget()
	if target == "" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	ids, _, err := parseVenueIDs(q.Get("venues"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	exclude := defaultArmExclude
	if q.Has("exclude") {
		exclude = nil
		for _, n := range strings.Split(q.Get("exclude"), ",") {
			if strings.TrimSpace(n) != "" {
				exclude = append(exclude, n)
			}
		}
	}
	dry := q.Get("confirm") != "arm" || q.Get("dry") == "1"
	ctx := r.Context()
	cands, err := h.db.ArmCandidates(ctx, ids)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := []armVenueOut{}
	tot := map[string]int{"venues": 0, "devices": 0, "to_file": 0, "filed": 0, "errors": 0, "hidden_venues": 0}
	for _, v := range cands {
		vo := armVenueOut{VenueID: v.ID, Venue: v.Name, Hidden: v.Hidden,
			Counts: map[string]int{}, Kinds: map[string]int{}, Clients: map[string]int{}}
		if db.SameVenueName(v.Name, exclude) {
			vo.Skipped = "excluded"
			vo.Counts, vo.Kinds, vo.Clients = nil, nil, nil
			out = append(out, vo)
			continue
		}
		tot["venues"]++
		var hide []string
		for _, d := range v.Devices {
			do := armDeviceOut{Serial: d.Serial, Kind: d.Kind, Client: d.Client, Move: d.MoveState, Action: armAction(d.MoveState)}
			vo.Kinds[d.Kind]++
			vo.Clients[d.Client]++
			vo.Counts["move:"+orNone(d.MoveState)]++
			tot["devices"]++
			if do.Action != "keep" {
				tot["to_file"]++
				if !dry {
					if res := h.startMove(ctx, target, d.Serial, "arm"); res.Error != "" {
						do.Error = res.Error
						vo.Errors++
						tot["errors"]++
					} else {
						do.Move = res.State
						tot["filed"]++
					}
				}
			}
			if do.Error == "" {
				hide = append(hide, d.Serial)
			}
			vo.Devices = append(vo.Devices, do)
		}
		vo.WillHide = vo.Errors == 0
		if !dry && vo.Errors == 0 {
			if err := h.db.HideVenue(ctx, v.ID, hide); err != nil {
				vo.Skipped = "could not hide: " + err.Error()
				vo.WillHide = false
				tot["errors"]++
			} else {
				vo.Done = true
				tot["hidden_venues"]++
				_ = h.db.InsertAudit(ctx, "admin API", "device.move_arm", v.Name,
					fmt.Sprintf("%d devices hidden, moves filed for %d", len(hide), tot["filed"]))
			}
		}
		out = append(out, vo)
	}
	writeJSON(w, http.StatusOK, map[string]any{"dry": dry, "target": target, "excluded": exclude, "totals": tot, "venues": out})
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// MoveUnarm is POST /api/v1/admin/move/unarm.
func (h *Handler) MoveUnarm(w http.ResponseWriter, r *http.Request) {
	if deviceMoveTarget() == "" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if q.Get("venues") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "venues=all or venues=id,id is required"})
		return
	}
	ids, all, err := parseVenueIDs(q.Get("venues"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx := r.Context()
	if all {
		if ids, err = h.db.HiddenVenueIDs(ctx); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	// Cancel pending moves of those venues' devices first, when asked.
	canceled := 0
	if q.Get("cancel") == "1" {
		for _, id := range ids {
			moves, err := h.db.ListDeviceMoves(ctx, []string{db.MovePending}, &id)
			if err != nil {
				continue
			}
			for _, m := range moves {
				if ok, err := h.db.CancelDeviceMove(ctx, m.Serial); err == nil && ok {
					canceled++
				}
			}
		}
	}
	n, err := h.db.UnhideVenues(ctx, ids)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = h.db.InsertAudit(ctx, "admin API", "device.move_unarm", "", fmt.Sprintf("%d venues, %d devices shown, %d moves canceled", len(ids), n, canceled))
	writeJSON(w, http.StatusOK, map[string]any{"venues": len(ids), "devices_shown": n, "moves_canceled": canceled,
		"note": "moves already filed stay pending unless cancel=1"})
}
