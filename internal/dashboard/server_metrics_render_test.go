package dashboard

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"mdm/internal/ingest"
	"mdm/internal/metrics"
)

// The Server page is rendered entirely in the browser from one JSON payload, which
// means a mistake in the shape of that payload is invisible until someone opens the
// page. This asserts the contract between the handler and the script: the field names
// the template reads must be the field names the collector emits.
func TestServerMetricsPayloadShape(t *testing.T) {
	c := metrics.New()
	c.Observe("GET", "/devices/AT070AABU00231", 200, 42*time.Millisecond)
	c.Observe("POST", "/api/v1/checkin", 200, 8*time.Millisecond)
	c.Observe("POST", "/api/v1/checkin", 500, 12*time.Millisecond)
	c.Checkin()
	c.Emit("peer", "ok", "arrival announced for AT070AABU00231")
	c.SampleNow(31, 6, 25, 0)

	payload := serverPayload{
		Snapshot: c.Snapshot(12, 40),
		Queues:   serverQueues{CommandsPending: 3, DevicesTotal: 36, DevicesOnline: 31},
		Now:      time.Now(),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(b)

	// Every key the page's render() reads. A rename on either side breaks here rather
	// than on a blank dashboard.
	for _, key := range []string{
		`"snapshot"`, `"queues"`, `"now"`,
		`"series"`, `"latest"`, `"routes"`, `"events"`, `"uptime_sec"`, `"req_total"`, `"checkin_total"`,
		`"requests"`, `"checkins"`, `"ws"`, `"goroutines"`, `"heap_mb"`, `"sys_mb"`,
		`"db_used"`, `"db_total"`, `"db_waiting"`,
		`"route"`, `"calls"`, `"p50_ms"`, `"p95_ms"`, `"errors"`,
		`"db"`, `"size_mb"`, `"cache_hit_pct"`, `"conn_active"`, `"conn_max"`, `"longest_query_sec"`, `"tables"`,
		`"commands_pending"`, `"deployments_live"`, `"ota_in_flight"`, `"shell_sessions"`,
		`"peer_outbox"`, `"devices_total"`, `"devices_online"`, `"devices_elsewhere"`, `"alerts_open"`,
	} {
		if !strings.Contains(body, key) {
			t.Errorf("payload is missing %s — the page reads it", key)
		}
	}

	// A device serial in a URL must collapse to a placeholder, or the slowest-routes
	// table becomes one row per device and says nothing about which route is slow.
	if !strings.Contains(body, "/devices/{serial}") {
		t.Errorf("route not normalised; payload: %s", body)
	}
	// The 500 must be counted as an error against its route.
	var got serverPayload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var checkin *metrics.RouteStat
	for i := range got.Snapshot.Routes {
		if got.Snapshot.Routes[i].Route == "POST /api/v1/checkin" {
			checkin = &got.Snapshot.Routes[i]
		}
	}
	if checkin == nil {
		t.Fatal("check-in route missing from the snapshot")
	}
	if checkin.Calls != 2 || checkin.Errors != 1 {
		t.Errorf("check-in route: %d calls / %d errors, want 2 / 1", checkin.Calls, checkin.Errors)
	}
}

// TestServerPipelinePayload pins the check-in pipeline card's keys: the frame carries
// the pipeline only once main has wired it, and carries every field the card reads.
func TestServerPipelinePayload(t *testing.T) {
	h := &Handler{}
	if b, _ := json.Marshal(serverPayload{Ingest: nil}); strings.Contains(string(b), `"ingest"`) {
		t.Error("an unwired pipeline should leave the card out of the frame")
	}
	h.SetIngestStats(func() ingest.Pipeline {
		return ingest.Pipeline{Stats: ingest.Stats{Depth: 3, Capacity: 2048, Written: 12, Batches: 2, Rejected: 1},
			SlotsInUse: 4, Slots: 16, Shed: 5, HistoryFailed: 0}
	})
	st := h.ingestStats()
	b, err := json.Marshal(serverPayload{Ingest: &st})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"ingest"`, `"slots_in_use"`, `"slots"`, `"depth"`, `"capacity"`, `"written"`,
		`"batches"`, `"last_flush_ms"`, `"max_flush_ms"`, `"shed"`, `"rejected"`, `"history_failed"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("pipeline frame is missing %s — the card reads it", key)
		}
	}
	page, err := os.ReadFile("../../templates/server_metrics.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="sv-pipe-wrap"`, `id="sv-pipe"`, "p.ingest"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("server page is missing %s", want)
		}
	}
}
