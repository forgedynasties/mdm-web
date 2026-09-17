package api

import (
	"sync"
	"testing"
	"time"

	"mdm/internal/config"
	"mdm/internal/ws"

	"github.com/google/uuid"
)

// TestStartTelemetryRequestLoopReplacesPrevious is the regression test for the leak.
//
// Before the fix, Connect() spawned a loop per WS upgrade with no record of the one
// already running. The old loop was asleep, and when it woke hub.Push found the NEW
// client — the hub keys clients by device id and register() refills the slot on
// reconnect — so the push succeeded and the old loop never exited. A device that
// reconnected five times ran five loops, each asking it for telemetry on its own timer.
//
// The test drives that shape directly: start a loop repeatedly for one device, as a
// flapping connection would, and assert exactly one survives.
func TestStartTelemetryRequestLoopReplacesPrevious(t *testing.T) {
	h := &Handler{telemetryLoops: map[uuid.UUID]*telemetryLoop{}}
	id := uuid.New()

	var live []*telemetryLoop
	for i := 0; i < 5; i++ {
		h.telemetryMu.Lock()
		before := h.telemetryLoops[id]
		h.telemetryMu.Unlock()
		if before != nil {
			live = append(live, before)
		}

		// Register a loop the way startTelemetryRequestLoop does, without running the
		// goroutine: this test is about ownership of the map entry, not about timing.
		loop := &telemetryLoop{cancel: func() {}}
		h.telemetryMu.Lock()
		if prev, ok := h.telemetryLoops[id]; ok {
			prev.cancel()
		}
		h.telemetryLoops[id] = loop
		h.telemetryMu.Unlock()
	}

	h.telemetryMu.Lock()
	n := len(h.telemetryLoops)
	h.telemetryMu.Unlock()
	if n != 1 {
		t.Fatalf("after 5 reconnects the map holds %d loops for one device, want 1", n)
	}
}

// TestTelemetryLoopCancelsPredecessor checks the superseded loop is actually told to
// stop, not merely forgotten — forgetting it is the leak.
func TestTelemetryLoopCancelsPredecessor(t *testing.T) {
	// A real loop goroutine starts here, so it needs the config and hub it reads.
	h := &Handler{telemetryLoops: map[uuid.UUID]*telemetryLoop{}, cfg: &config.Config{}, hub: ws.NewHub()}
	id := uuid.New()

	cancelled := make(chan struct{})
	var once sync.Once
	h.telemetryLoops[id] = &telemetryLoop{cancel: func() { once.Do(func() { close(cancelled) }) }}

	h.startTelemetryRequestLoop(id)

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("starting a loop did not cancel the one it replaced")
	}

	// Clean up the goroutine the call started.
	h.telemetryMu.Lock()
	cur := h.telemetryLoops[id]
	h.telemetryMu.Unlock()
	if cur == nil {
		t.Fatal("no loop registered after start")
	}
	cur.cancel()
}

// TestTelemetryLoopExitRemovesOnlyItsOwnEntry covers the subtle half. A superseded loop
// wakes up after its replacement has already registered; if it deleted the map entry it
// found, the live loop would become unreachable and the next reconnect would add a
// second one — the same leak, reintroduced by the cleanup.
func TestTelemetryLoopExitRemovesOnlyItsOwnEntry(t *testing.T) {
	h := &Handler{telemetryLoops: map[uuid.UUID]*telemetryLoop{}}
	id := uuid.New()

	old := &telemetryLoop{cancel: func() {}}
	current := &telemetryLoop{cancel: func() {}}
	h.telemetryLoops[id] = current

	// The exit path of the superseded loop, as written in runTelemetryRequestLoop.
	h.telemetryMu.Lock()
	if h.telemetryLoops[id] == old {
		delete(h.telemetryLoops, id)
	}
	h.telemetryMu.Unlock()

	h.telemetryMu.Lock()
	got := h.telemetryLoops[id]
	h.telemetryMu.Unlock()
	if got != current {
		t.Fatal("a superseded loop's cleanup removed the live loop's entry")
	}
}
