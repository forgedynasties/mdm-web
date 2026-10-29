package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// TestCloseAllSaysRestart: a shutdown close carries 1012, so clients spread their reconnect;
// a single device's close stays a normal 1000.
func TestCloseAllSaysRestart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(h *Hub, id uuid.UUID)
		want  int
	}{
		{"shutdown", func(h *Hub, _ uuid.UUID) { h.CloseAll() }, websocket.CloseServiceRestart},
		{"one device", func(h *Hub, id uuid.UUID) { h.Close(id) }, websocket.CloseNormalClosure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHub()
			id := uuid.New()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := h.Upgrade(w, r, id)
				if err != nil {
					return
				}
				go c.WritePump()
				c.ReadPump()
			}))
			defer srv.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for i := 0; i < 50 && !h.IsConnected(id); i++ {
				time.Sleep(20 * time.Millisecond)
			}
			tc.close(h, id)
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, _, err = conn.ReadMessage()
			ce, ok := err.(*websocket.CloseError)
			if !ok || ce.Code != tc.want {
				t.Fatalf("close = %v, want code %d", err, tc.want)
			}
		})
	}
}
