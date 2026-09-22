package dashboard

import (
	"context"
	"net/http"
	"sync"
)

// Shutdown draining for the streaming endpoints.
//
// The MDM holds a lot of connections open that never end by themselves: a dozen SSE
// endpoints feeding the dashboard, plus logcat and command-output streams. http.Server's
// Shutdown waits for active requests to finish, and those requests do not finish — so a
// deploy sat in Shutdown for its entire 25-second timeout before the new container could
// bind the port. That wait, not the container start (under a second, measured), was the
// whole of the 502 window during a deploy.
//
// So the streams are closed first, and only then does Shutdown drain what is left: a
// handful of ordinary requests that complete in milliseconds. A browser re-opens an SSE
// stream on its own, and devices already get a close frame from the hub for the same
// reason, so nothing here costs the fleet anything.

var (
	drainOnce sync.Once
	drainCh   = make(chan struct{})
)

// BeginDrain tells every open stream to end. Safe to call more than once; the streams
// see a closed channel, which is exactly what a second caller would want anyway.
func BeginDrain() { drainOnce.Do(func() { close(drainCh) }) }

// Draining is closed when the server has started shutting down.
func Draining() <-chan struct{} { return drainCh }

// drainable wraps a streaming handler so its request context is cancelled when the
// server starts shutting down. The handlers already select on r.Context().Done() — that
// is how they notice a browser navigating away — so this reuses the path they all have
// rather than threading a second signal through ten loops.
func drainable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			select {
			case <-drainCh:
				cancel()
			case <-ctx.Done():
			}
		}()
		next(w, r.WithContext(ctx))
	}
}
