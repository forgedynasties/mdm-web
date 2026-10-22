package dashboard

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RunPresenceRecord keeps the online/offline history the network graph draws (N1, 22 Oct
// 2026), once a minute. A device is online by the dashboard's own rule: its socket is
// connected, or it was heard within three check-in intervals. A change to offline is
// placed at the last time the device was heard, not when this job noticed, so the band
// starts where the device actually went quiet.
func (h *Handler) RunPresenceRecord(ctx context.Context) {
	presenceMu.Lock()
	defer presenceMu.Unlock()
	rows, err := h.db.PresenceRows(ctx)
	if err != nil {
		log.Printf("[presence] %v", err)
		return
	}
	now := time.Now()
	stale := time.Duration(h.cfg.CheckinInterval()*3) * time.Second
	for _, r := range rows {
		online := h.hub.IsConnected(r.ID) || now.Sub(r.LastSeen) < stale
		prev := r.Recorded
		if v, ok := presenceKnown[r.ID]; ok {
			prev = v
		}
		want := "false"
		if online {
			want = "true"
		}
		if prev == want {
			presenceKnown[r.ID] = want
			continue
		}
		at := now
		if !online && !r.LastSeen.IsZero() && r.LastSeen.Before(now) {
			at = r.LastSeen
		}
		if err := h.db.RecordPresence(ctx, r.ID, at, online, prev); err != nil {
			log.Printf("[presence] %s: %v", r.ID, err)
			continue
		}
		presenceKnown[r.ID] = want
	}
}

var (
	presenceMu    sync.Mutex
	presenceKnown = map[uuid.UUID]string{}
)
