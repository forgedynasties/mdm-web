package api

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// ── Lost-device reporting in the device config (18 Oct) ──────────────────────
//
// Two config keys ride every config answer (check-in response and the socket's config
// frame), see internal/db/lost_reporting.go for the why:
//
//	lost_listen  the beacons of the devices at the same venue that are lost now, or null —
//	             always sent, so a scan stops the moment the lost device is found.
//	self_lock    the venue's opt-in to lock its devices when they leave (firmware client);
//	             {"enabled": false} when off.
//
// Both need one lookup; it is cached per device until something changes the answer
// (db.LostConfigGen moves) or a minute passes, so a stream of socket frames costs nothing.

const (
	lostCfgTTL   = time.Minute
	lostFixEvery = 5 * time.Minute // one live trail point per this while a lost device is online
)

type lostCfgEntry struct {
	gen uint64
	at  time.Time
	cfg db.LostConfig
}

var lostCfgCache sync.Map // uuid.UUID → lostCfgEntry

func (h *Handler) lostConfig(ctx context.Context, deviceID uuid.UUID) (db.LostConfig, bool) {
	gen := h.db.LostConfigGen()
	if v, ok := lostCfgCache.Load(deviceID); ok {
		e := v.(lostCfgEntry)
		if e.gen == gen && time.Since(e.at) < lostCfgTTL {
			return e.cfg, true
		}
	}
	c, err := h.db.LostConfigFor(ctx, deviceID)
	if err != nil {
		log.Printf("[lost] config for %s: %v", deviceID, err)
		return db.LostConfig{}, false
	}
	lostCfgCache.Store(deviceID, lostCfgEntry{gen: gen, at: time.Now(), cfg: c})
	return c, true
}

// addLostConfig puts lost_listen and self_lock into a device's config answer (see
// db.LostConfigKeys). dpc comes from the report itself: a device's first check-in may
// predate the agent kind being stored.
func (h *Handler) addLostConfig(ctx context.Context, cfg map[string]any, deviceID uuid.UUID, dpc bool) {
	c, ok := h.lostConfig(ctx, deviceID)
	if !ok {
		return
	}
	c.DPC = c.DPC || dpc
	for k, v := range db.LostConfigKeys(c, h.cfg.LostModeSupportPhone()) {
		cfg[k] = v
	}
}

// noteLostReport handles the lost-mode parts of a report: a live trail point while a lost
// device is online with a resolved location, and a self-lock the device says it did.
func (h *Handler) noteLostReport(ctx context.Context, deviceID uuid.UUID, extra json.RawMessage, at time.Time) {
	if len(extra) == 0 || !strings.Contains(string(extra), "lost_mode") && !strings.Contains(string(extra), "latitude") {
		return
	}
	var e struct {
		Lost *struct {
			On      bool  `json:"on"`
			Self    bool  `json:"self"`
			SinceMs int64 `json:"since_ms"`
		} `json:"lost_mode"`
		Lat *float64 `json:"latitude"`
		Lon *float64 `json:"longitude"`
		Acc *float64 `json:"location_accuracy"`
	}
	if json.Unmarshal(extra, &e) != nil {
		return
	}
	lost := e.Lost != nil && e.Lost.On
	if e.Lost == nil {
		// A socket delta may carry the location without lost_mode: the server's own record
		// says whether it is lost.
		if c, ok := h.lostConfig(ctx, deviceID); ok {
			lost = c.Lost
		}
	}
	if lost && e.Lat != nil && e.Lon != nil {
		acc := 0.0
		if e.Acc != nil {
			acc = *e.Acc
		}
		if err := h.db.InsertTrailFix(ctx, deviceID, at, *e.Lat, *e.Lon, acc, lostFixEvery); err != nil {
			log.Printf("[lost] trail fix %s: %v", deviceID, err)
		}
	}
	switch {
	case e.Lost != nil && e.Lost.On && e.Lost.Self:
		since := at
		if e.Lost.SinceMs > 0 {
			since = time.UnixMilli(e.Lost.SinceMs)
		}
		h.db.RecordSelfLock(ctx, deviceID, since)
	case e.Lost != nil && !e.Lost.On:
		// Every report says {"on": false}; only one from a device the server holds lost can
		// close anything.
		if c, ok := h.lostConfig(ctx, deviceID); ok && c.Lost {
			h.db.EndSelfLock(ctx, deviceID)
		}
	}
}

// NudgeLostVenue asks the other devices at the venues of the given devices to check in now,
// so they pick up (or drop) lost_listen at once instead of at their next report. Called
// after a lost_mode command is pushed; a device that is not connected gets it at its next
// check-in anyway.
func (h *Handler) NudgeLostVenue(ctx context.Context, deviceIDs []uuid.UUID) {
	ids, err := h.db.VenueDeviceIDs(ctx, deviceIDs)
	if err != nil {
		log.Printf("[lost] venue devices: %v", err)
		return
	}
	msg, _ := json.Marshal(map[string]any{"type": "checkin_now"})
	for _, id := range ids {
		h.hub.Push(id, msg)
	}
}
