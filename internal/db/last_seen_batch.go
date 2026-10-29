package db

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Disconnect stamps in batches (scalability plan, from the 29 Oct stage load test): every
// socket that closes stamps last_seen, and when the fleet drops at once — a restart, a
// deploy — that was one UPDATE per device at the same moment, which took the whole
// connection pool (60 of 60, 706 waits, at 500 fake devices). QueueLastSeen hands the
// stamp to one writer that sends them all in a single statement every 250 ms.

const lastSeenFlush = 250 * time.Millisecond

type lastSeenBatch struct {
	mu      sync.Mutex
	pending map[uuid.UUID]time.Time
	started bool
}

// StartLastSeenBatcher runs the writer until ctx ends (then writes what is left).
func (d *DB) StartLastSeenBatcher(ctx context.Context) {
	d.lastSeen.mu.Lock()
	d.lastSeen.started = true
	if d.lastSeen.pending == nil {
		d.lastSeen.pending = map[uuid.UUID]time.Time{}
	}
	d.lastSeen.mu.Unlock()
	go func() {
		t := time.NewTicker(lastSeenFlush)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				d.flushLastSeen(fctx)
				cancel()
				return
			case <-t.C:
				d.flushLastSeen(ctx)
			}
		}
	}()
}

// QueueLastSeen stamps a device's last_seen_at at t, in the next batch (or at once when no
// batcher runs, as in tests). A later stamp for the same device replaces an earlier one.
func (d *DB) QueueLastSeen(ctx context.Context, deviceID uuid.UUID, t time.Time) error {
	d.lastSeen.mu.Lock()
	if !d.lastSeen.started {
		d.lastSeen.mu.Unlock()
		return d.TouchLastSeen(ctx, deviceID, t)
	}
	if cur, ok := d.lastSeen.pending[deviceID]; !ok || t.After(cur) {
		d.lastSeen.pending[deviceID] = t
	}
	d.lastSeen.mu.Unlock()
	return nil
}

func (d *DB) flushLastSeen(ctx context.Context) {
	d.lastSeen.mu.Lock()
	if len(d.lastSeen.pending) == 0 {
		d.lastSeen.mu.Unlock()
		return
	}
	batch := d.lastSeen.pending
	d.lastSeen.pending = make(map[uuid.UUID]time.Time, len(batch))
	d.lastSeen.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(batch))
	ts := make([]time.Time, 0, len(batch))
	for id, t := range batch {
		ids, ts = append(ids, id), append(ts, t)
	}
	// Never moves last_seen backwards: a check-in may have stamped it later meanwhile.
	if _, err := d.pool.Exec(ctx, `
		UPDATE devices d SET last_seen_at = v.t
		FROM unnest($1::uuid[], $2::timestamptz[]) AS v(id, t)
		WHERE d.id = v.id AND d.last_seen_at < v.t`, ids, ts); err != nil {
		log.Printf("[last-seen] write %d stamps: %v", len(ids), err)
	}
}
