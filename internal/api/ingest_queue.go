package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"mdm/internal/db"
	"mdm/internal/ingest"
)

// Check-in admission and the history queue. See internal/ingest for why the queue is
// in-process; this file decides what goes on it and what a device is told when the
// server is behind.

const (
	// ingestSlotCount caps how many check-ins run their synchronous half at once. Each
	// holds a pool connection per statement, and the pool (60) is shared with the
	// dashboard: without a cap, the whole fleet reconnecting after a deploy can take
	// every connection and leave an operator's page waiting on devices.
	ingestSlotCount = 16
	// ingestSlotWait is how long an HTTP check-in may queue for a slot before it is told
	// to come back later. A device on the socket waits instead — it has nowhere to be
	// told that, and its frame is the only copy.
	ingestSlotWait = 5 * time.Second
)

// historyJob is the part of a check-in nothing in the reply depends on: the shaped
// history UpsertCheckin produced and the crash/reboot events in the report.
type historyJob struct {
	Shaped  db.ShapedTelemetry
	BuildID string
	Extra   json.RawMessage
}

func newHistoryQueue(d *db.DB) *ingest.Queue[historyJob] {
	return ingest.New(ingest.Options{},
		func(j historyJob) uint32 { return binary.BigEndian.Uint32(j.Shaped.DeviceID[12:]) },
		func(ctx context.Context, jobs []historyJob) { writeHistory(ctx, d, jobs) })
}

// writeHistory writes one batch. Samples and state events go in one transaction for
// the whole batch; if it fails — one device deleted mid-flight is enough, through its
// foreign key — each report is retried on its own so the rest are not lost with it.
func writeHistory(ctx context.Context, d *db.DB, jobs []historyJob) {
	shaped := make([]db.ShapedTelemetry, len(jobs))
	for i, j := range jobs {
		shaped[i] = j.Shaped
	}
	if err := d.WriteShapedBatch(ctx, shaped); err != nil {
		log.Printf("[ingest] batch of %d failed, writing one by one: %v", len(jobs), err)
		for _, s := range shaped {
			if err := d.WriteShapedBatch(ctx, []db.ShapedTelemetry{s}); err != nil {
				historyFailed.Add(1)
				log.Printf("[ingest] history for %s lost: %v", s.DeviceID, err)
			}
		}
	}
	for _, j := range jobs {
		d.IngestDeviceEvents(ctx, j.Shaped.DeviceID, j.BuildID, j.Extra, j.Shaped.At)
	}
}

var (
	historyFailed atomic.Int64 // reports whose history could not be written at all
	checkinsShed  atomic.Int64 // HTTP check-ins told to come back later
)

// queueHistory hands a report's history to the queue, or writes it here when the
// queue will not take it (full, or closing during a deploy).
func (h *Handler) queueHistory(ctx context.Context, job historyJob) {
	if h.history.Enqueue(job) {
		return
	}
	writeHistory(ctx, h.db, []historyJob{job})
}

// admitCheckin takes a slot for an HTTP check-in, or answers 429 and returns false.
// The caller must call the returned release once the check-in is done.
//
// Retry-After is spread over 20–60 s: every device that was shed at the same moment
// coming back at the same moment is the burst again. The firmware client honours it
// as a floor on its next poll; MDM-lite simply tries on its next scheduled run.
func (h *Handler) admitCheckin(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	shed := func() (func(), bool) {
		checkinsShed.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(20+rand.IntN(41)))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "busy"})
		return nil, false
	}
	if h.history.Saturated() {
		return shed()
	}
	t := time.NewTimer(ingestSlotWait)
	defer t.Stop()
	select {
	case h.ingestSlots <- struct{}{}:
		return func() { <-h.ingestSlots }, true
	case <-t.C:
		return shed()
	case <-r.Context().Done():
		return nil, false
	}
}

// acquireSlotWS waits for a slot however long it takes: a WebSocket frame cannot be
// refused, and the socket's own reader is the only thing held up.
func (h *Handler) acquireSlotWS() (release func()) {
	h.ingestSlots <- struct{}{}
	return func() { <-h.ingestSlots }
}

// CloseIngest writes out whatever history is still queued. Called on shutdown after the
// HTTP server has drained, so a deploy loses nothing.
func (h *Handler) CloseIngest(ctx context.Context) error {
	return h.history.Close(ctx)
}

// IngestStats is the queue and admission picture for /debug/vars.
type IngestStats struct {
	ingest.Stats
	SlotsInUse    int   `json:"slots_in_use"`
	Slots         int   `json:"slots"`
	Shed          int64 `json:"shed"`
	HistoryFailed int64 `json:"history_failed"`
}

func (h *Handler) IngestStats() IngestStats {
	return IngestStats{
		Stats:         h.history.Stats(),
		SlotsInUse:    len(h.ingestSlots),
		Slots:         cap(h.ingestSlots),
		Shed:          checkinsShed.Load(),
		HistoryFailed: historyFailed.Load(),
	}
}
