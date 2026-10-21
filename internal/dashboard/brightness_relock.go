package dashboard

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
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

// brightnessLastSent is when the job last re-sent a device its brightness (zero if never).
func brightnessLastSent(id uuid.UUID) time.Time {
	brightnessMu.Lock()
	defer brightnessMu.Unlock()
	return brightnessSent[id]
}

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

// briView is the device page's brightness row (B1, picked 21 Oct 2026): a chip with the
// level and lock, whether it is the default or set for this device (by whom, when), what
// the device reports, and a pencil that opens the editor in place.
type briView struct {
	Level          int // 0 = not managed
	Locked         bool
	Default        bool
	DefaultApplies bool
	Max            int
	Pct            int // the meter
	Reports        bool // the client reports brightness (1.14.0+)
	Reported       int
	ReportedLocked bool
	Drift          bool
	SetBy, SetAgo  string
	SentAgo        string // the re-lock job's last resend, when drifted
}

func briViewFor(cfg *db.DeviceConfig, extra json.RawMessage) briView {
	if cfg == nil {
		return briView{}
	}
	b := cfg.Brightness()
	v := briView{Level: b.Level, Locked: b.Locked, Default: b.Default, DefaultApplies: b.DefaultApplies, Max: b.Max,
		SetBy: cfg.ScreenBrightnessSetBy}
	if cfg.ScreenBrightnessSetAt != nil {
		v.SetAgo = agoShort(*cfg.ScreenBrightnessSetAt)
	}
	if b.Max > 0 && b.Level > 0 {
		v.Pct = b.Level * 100 / b.Max
		if v.Pct > 100 {
			v.Pct = 100
		}
	}
	var e struct {
		Level  *int  `json:"screen_brightness"`
		Locked *bool `json:"screen_brightness_locked"`
	}
	if len(extra) > 0 && json.Unmarshal(extra, &e) == nil && e.Level != nil {
		v.Reports, v.Reported = true, *e.Level
		v.ReportedLocked = e.Locked != nil && *e.Locked
		st := db.BrightnessState{Config: *cfg, Reported: e.Level, ReportedLocked: v.ReportedLocked}
		v.Drift = st.Drifted()
		if v.Drift {
			if t := brightnessLastSent(cfg.DeviceID); !t.IsZero() {
				v.SentAgo = agoShort(t)
			}
		}
	}
	return v
}
