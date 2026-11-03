package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// DeviceGroup lets a Lab device pick its own group from the Standard client's status screen
// (G3, picked 3 Nov 2026: "in lab devices standard mdm the mdm app shows a list of groups to
// choose and choosing there adds it to that group it also shows the current group"; the app
// opens on the picker while a Lab device has no group).
//
//	POST /api/v1/device-group {"serial"}                 → {lab, current, groups, venue?, role?}
//	POST /api/v1/device-group {"serial", "set": "<id>"}  → moves it there ("" = no group)
//
// A device has one group: a pick moves it out of any other. Only Lab devices (at no venue,
// not waiting to be placed) may pick, and only groups with no member at a venue (the rule
// that lets an engineer edit a group). Audited as group.device_pick by the device itself.
func (h *Handler) DeviceGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Serial string  `json:"serial"`
		Set    *string `json:"set"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	serial := strings.TrimSpace(body.Serial)
	if !validSerial(serial) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidSerial})
		return
	}
	if h.deviceRateLimited(w, serial) {
		return
	}
	if !h.requireDeviceIdentity(w, r, serial) {
		return
	}
	ctx := r.Context()
	device, err := h.db.GetDevice(ctx, serial)
	if err != nil || device == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	placement, _ := h.db.DevicePlacement(ctx, device.ID)
	lab := device.RestaurantID == nil && placement != db.PlacementQueue
	groups, err := h.db.LabGroups(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if body.Set != nil {
		if !lab {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "only a Lab device picks its own group"})
			return
		}
		var to *uuid.UUID
		name := "no group"
		if s := strings.TrimSpace(*body.Set); s != "" {
			id, err := uuid.Parse(s)
			ok := err == nil
			if ok {
				ok = false
				for _, g := range groups {
					if g.ID == id {
						ok, name = true, g.Name
						break
					}
				}
			}
			if !ok {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "that group can't be picked here"})
				return
			}
			to = &id
		}
		if err := h.db.MoveDeviceToGroup(ctx, device.ID, to); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		_ = h.db.InsertAudit(ctx, "device:"+serial, "group.device_pick", serial, "on the device: "+name)
		h.hub.PublishDeviceUpdate(device.ID)
		if groups, err = h.db.LabGroups(ctx); err != nil {
			groups = nil
		}
	}
	type grp struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	out := map[string]any{"lab": lab, "current": nil, "groups": []grp{}}
	if mine, err := h.db.ListDeviceGroups(ctx, device.ID); err == nil && len(mine) > 0 {
		var names []string
		for _, g := range mine {
			names = append(names, g.Name)
		}
		out["current"] = map[string]string{"id": mine[0].ID.String(), "name": strings.Join(names, ", ")}
	}
	if device.RestaurantName != "" {
		out["venue"] = device.RestaurantName
	}
	if c := device.Class(); c != "" {
		out["role"] = product.ClassLabel(c) // the app's badge (S3): "Sunmi D3 Pro · POS terminal"
	}
	if lab {
		list := []grp{}
		for _, g := range groups {
			list = append(list, grp{g.ID.String(), g.Name, g.DeviceCount})
		}
		out["groups"] = list
	}
	writeJSON(w, http.StatusOK, out)
}
