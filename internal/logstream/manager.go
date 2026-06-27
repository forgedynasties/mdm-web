// Package logstream relays live `logcat` output from a device WebSocket to one or
// more browser SSE connections.
//
// Flow:
//   - The dashboard opens an SSE connection, which mints a request_id, calls
//     Open(requestID) to register a chunk channel, and pushes a
//     {"type":"start_logcat_stream","request_id":...,filters} frame to the device.
//   - The device streams {"type":"logcat_stream","request_id":...,"chunk":...}
//     frames; HandleDeviceMessage fans each chunk out to the channel.
//   - On a {"type":"logcat_stream_end",...} frame, or when the browser disconnects
//     (Close), the channel is closed and the dashboard pushes stop_logcat_stream.
package logstream

import (
	"encoding/json"
	"log"
	"sync"
)

// stream holds one live logcat session's delivery channel.
type stream struct {
	ch    chan string
	ended bool
}

// Manager tracks active live-logcat streams keyed by request_id.
type Manager struct {
	mu      sync.Mutex
	streams map[string]*stream
}

func NewManager() *Manager {
	return &Manager{streams: make(map[string]*stream)}
}

// Open registers a new stream for requestID and returns its chunk channel. The
// caller consumes the channel until it is closed (by a stream-end frame or Close).
func (m *Manager) Open(requestID string) <-chan string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &stream{ch: make(chan string, 256)}
	m.streams[requestID] = s
	return s.ch
}

// Close ends a stream and closes its channel. Idempotent; safe to call from the
// SSE handler on browser disconnect.
func (m *Manager) Close(requestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.streams[requestID]; ok {
		if !s.ended {
			s.ended = true
			close(s.ch)
		}
		delete(m.streams, requestID)
	}
}

// HandleDeviceMessage routes a device's logcat_stream / logcat_stream_end frames.
// Returns true if the frame was a logcat-stream frame (so the hub dispatch can stop).
func (m *Manager) HandleDeviceMessage(raw []byte) bool {
	var f struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Chunk     string `json:"chunk"`
		Reason    string `json:"reason"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return false
	}
	switch f.Type {
	case "logcat_stream":
		// Send under the lock so we never send on a channel Close just closed.
		m.mu.Lock()
		if s, ok := m.streams[f.RequestID]; ok && !s.ended {
			select {
			case s.ch <- f.Chunk:
			default:
				log.Printf("[logstream] subscriber buffer full for %s, dropping chunk", f.RequestID)
			}
		}
		m.mu.Unlock()
		return true
	case "logcat_stream_end":
		if f.Error != "" {
			log.Printf("[logstream] stream %s ended: %s (%s)", f.RequestID, f.Reason, f.Error)
		}
		m.Close(f.RequestID)
		return true
	}
	return false
}
