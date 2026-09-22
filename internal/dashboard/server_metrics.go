package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/metrics"
)

// The Server page: what this MDM is doing right now. Tiles and sparklines for load,
// the work queues that explain it, the slowest routes, and a live feed of the work
// itself. Everything comes from the in-process collector (internal/metrics) plus a
// handful of counts from the database — no external monitoring, no agent, nothing
// persisted. See internal/metrics for why it is deliberately memory-only.

// serverQueues is the "work in flight" block: the things this server is currently on
// the hook for. These are cheap counting queries, run once per page load and once per
// stream tick, and they are what make this page different from a generic host monitor —
// a load average cannot tell you an OTA deployment is stuck.
type serverQueues struct {
	CommandsPending  int `json:"commands_pending"`
	DeploymentsLive  int `json:"deployments_live"`
	OTAInFlight      int `json:"ota_in_flight"`
	ShellSessions    int `json:"shell_sessions"`
	PeerOutbox       int `json:"peer_outbox"`
	DevicesTotal     int `json:"devices_total"`
	DevicesOnline    int `json:"devices_online"`
	DevicesElsewhere int `json:"devices_elsewhere"`
	AlertsOpen       int `json:"alerts_open"`
}

func (h *Handler) serverQueues(r *http.Request) serverQueues {
	q := serverQueues{}
	ctx := r.Context()
	q.CommandsPending, q.DeploymentsLive, q.OTAInFlight, q.PeerOutbox,
		q.DevicesTotal, q.DevicesElsewhere, q.AlertsOpen = h.db.ServerWorkCounts(ctx)
	q.DevicesOnline = len(h.hub.ConnectedIDs())
	// Live shell + remote-screen sessions come from the managers that hold them; both
	// report 0 when nothing is attached, which is the normal state.
	q.ShellSessions = h.liveSessionCount()
	return q
}

// serverPayload is one frame: the collector's snapshot plus the queue counts and the
// database's own vitals.
type serverPayload struct {
	Snapshot metrics.Snapshot `json:"snapshot"`
	Queues   serverQueues     `json:"queues"`
	DB       db.PGStats       `json:"db"`
	Now      time.Time        `json:"now"`
}

// Postgres vitals read catalog views and, for the table sizes, the filesystem. Cheap
// once a minute; not cheap on every two-second tick with several operators watching,
// which is why they are cached and shared rather than fetched per frame.
var (
	pgMu     sync.Mutex
	pgCached db.PGStats
	pgAt     time.Time
)

const pgStatsTTL = 30 * time.Second

func (h *Handler) pgStats(r *http.Request) db.PGStats {
	pgMu.Lock()
	defer pgMu.Unlock()
	if time.Since(pgAt) < pgStatsTTL {
		return pgCached
	}
	s, err := h.db.ServerPGStats(r.Context())
	if err != nil {
		// Keep showing the last good read rather than blanking the block: a failed
		// catalog query is not a reason to imply the database has no stats.
		return pgCached
	}
	pgCached, pgAt = s, time.Now()
	return pgCached
}

// ServerPage renders the shell. The numbers arrive over the stream immediately after,
// so the first paint is never a page of zeroes waiting for a poll.
func (h *Handler) ServerPage(w http.ResponseWriter, r *http.Request) {
	payload := serverPayload{Snapshot: metrics.Default.Snapshot(12, 40), Queues: h.serverQueues(r), DB: h.pgStats(r), Now: time.Now()}
	b, _ := json.Marshal(payload)
	h.render(w, r, "server_metrics.html", map[string]any{
		"Title":      "Server",
		"ActivePage": "server",
		// template.JS so the payload lands as raw JSON inside the script tag rather
		// than as an escaped JS string literal the page would have to parse twice.
		"Initial": template.JS(b),
	})
}

// ServerEvents streams the same payload every two seconds. It is its own SSE endpoint
// rather than another event on the fleet stream: this page is the only thing that wants
// this data, and a device-list page should not carry it.
func (h *Handler) ServerEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			payload := serverPayload{Snapshot: metrics.Default.Snapshot(12, 40), Queues: h.serverQueues(r), DB: h.pgStats(r), Now: time.Now()}
			b, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// liveSessionCount is how many interactive sessions are open across shell and remote
// screen. Both managers track their own subscribers; this is only ever a display
// number, so a manager that is nil (not wired in a test) counts as zero rather than
// panicking a page that is otherwise fine.
func (h *Handler) liveSessionCount() int {
	n := 0
	if h.remote != nil {
		n += h.remote.SessionCount()
	}
	return n
}
