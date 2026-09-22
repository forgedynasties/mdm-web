package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The deploy outage was self-inflicted: http.Server.Shutdown waits for active requests,
// SSE streams never end, so Shutdown always ran to its full timeout with nothing
// listening on the port. This asserts the mechanism that fixes it — a stream's request
// context is cancelled when the server starts draining, so it returns at once.
func TestDrainableEndsStreamsOnShutdown(t *testing.T) {
	done := make(chan struct{})
	h := drainable(func(w http.ResponseWriter, r *http.Request) {
		// Exactly the shape every SSE handler has: block until the request context
		// ends, which normally means the browser went away.
		<-r.Context().Done()
		close(done)
	})

	go h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/events/devices", nil))

	select {
	case <-done:
		t.Fatal("the stream ended before anything asked it to")
	case <-time.After(50 * time.Millisecond):
	}

	BeginDrain()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end when the server began draining — Shutdown would wait for it, " +
			"which is the 25-second gap this exists to remove")
	}

	// Draining is a one-way door and must stay safe to call again: the shutdown path
	// can be reached from more than one signal.
	BeginDrain()
	select {
	case <-Draining():
	default:
		t.Fatal("Draining() should stay closed once shutdown has begun")
	}
}
