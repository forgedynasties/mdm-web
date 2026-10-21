package dashboard

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Brightness re-lock (21 Oct 2026, user: "a cron job that checks brightness occasionally
// and relocks to 122"). The client already puts a locked level back by itself; this
// catches what it cannot: a config it missed, a client restarted with stale state, a
// device that came back from offline. Every 10 minutes it sends the config again to
// connected devices that report a level or lock other than their target, at most once
// per device every 30 minutes, so a device that keeps refusing is not hammered.

const brightnessResend = 30 * time.Minute

var (
	brightnessMu   sync.Mutex
	brightnessSent = map[uuid.UUID]time.Time{}
)

// RunBrightnessRelock is the job (main.go, every 10 minutes).
func (h *Handler) RunBrightnessRelock(ctx context.Context) {
	states, err := h.db.BrightnessStates(ctx, h.cfg.CheckinInterval()*3)
	if err != nil {
		log.Printf("[brightness] states: %v", err)
		return
	}
	now := time.Now()
	var push []uuid.UUID
	brightnessMu.Lock()
	for _, s := range states {
		if !s.Drifted() || !h.hub.IsConnected(s.ID) {
			continue
		}
		if t, ok := brightnessSent[s.ID]; ok && now.Sub(t) < brightnessResend {
			continue
		}
		brightnessSent[s.ID] = now
		push = append(push, s.ID)
	}
	for id, t := range brightnessSent { // forget old entries
		if now.Sub(t) > 24*time.Hour {
			delete(brightnessSent, id)
		}
	}
	brightnessMu.Unlock()
	if len(push) == 0 {
		return
	}
	h.pushKioskConfigToDevices(ctx, push)
	log.Printf("[brightness] re-sent brightness to %d device(s) off their locked level", len(push))
}
