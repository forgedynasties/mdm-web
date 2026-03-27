// Package shell routes real-time device messages to browser connections.
//
// Device → server message types (received over the device WebSocket):
//
//	{"type":"command_output","command_id":"<uuid>","chunk":"<base64>"}
//	{"type":"command_done",  "command_id":"<uuid>","exit_code":<int>}
//	{"type":"shell_output",  "session_id":"<uuid>","data":"<base64>"}
//	{"type":"shell_exit",    "session_id":"<uuid>","exit_code":<int>}
package shell

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/google/uuid"
)

// outputKey identifies a command output stream for a specific (command, device) pair.
type outputKey struct {
	CommandID uuid.UUID
	DeviceID  uuid.UUID
}

// outputStream holds the subscriber channels and buffered chunks for one stream.
type outputStream struct {
	chunks []string
	subs   []chan string
	closed bool
}

// Manager routes messages from devices to browser connections.
type Manager struct {
	// shell sessions: session_id → channel carrying raw output bytes for the browser
	sessionMu sync.RWMutex
	sessions  map[uuid.UUID]chan []byte

	// command output streams keyed by (commandID, deviceID)
	outMu   sync.Mutex
	outputs map[outputKey]*outputStream
}

func NewManager() *Manager {
	return &Manager{
		sessions: make(map[uuid.UUID]chan []byte),
		outputs:  make(map[outputKey]*outputStream),
	}
}

// HandleDeviceMessage is called by the hub for every message a device sends.
func (m *Manager) HandleDeviceMessage(deviceID uuid.UUID, raw []byte) {
	var frame struct {
		Type      string    `json:"type"`
		CommandID uuid.UUID `json:"command_id"`
		SessionID uuid.UUID `json:"session_id"`
		Chunk     string    `json:"chunk"`
		Data      string    `json:"data"`
		ExitCode  int       `json:"exit_code"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return
	}
	switch frame.Type {
	case "command_output":
		m.appendCommandOutput(outputKey{frame.CommandID, deviceID}, frame.Chunk)
	case "command_done":
		m.closeCommandOutput(outputKey{frame.CommandID, deviceID})
	case "shell_output":
		m.forwardShellOutput(frame.SessionID, []byte(frame.Data))
	case "shell_exit":
		m.closeShellSession(frame.SessionID)
	}
}

// ── Command output ────────────────────────────────────────────────────────────

func (m *Manager) appendCommandOutput(key outputKey, chunk string) {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	s := m.ensureStream(key)
	if s.closed {
		return
	}
	s.chunks = append(s.chunks, chunk)
	for _, ch := range s.subs {
		select {
		case ch <- chunk:
		default:
			log.Printf("[shell] command output subscriber buffer full, dropping chunk")
		}
	}
}

func (m *Manager) closeCommandOutput(key outputKey) {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	s := m.ensureStream(key)
	if s.closed {
		return
	}
	s.closed = true
	for _, ch := range s.subs {
		close(ch)
	}
	s.subs = nil
}

// SubscribeCommandOutput returns a channel that receives output chunks for the
// given (command, device) pair. The caller must consume the channel until it is
// closed. Call the returned unsubscribe func to clean up if the browser
// disconnects before the stream ends.
func (m *Manager) SubscribeCommandOutput(commandID, deviceID uuid.UUID) (<-chan string, func()) {
	key := outputKey{commandID, deviceID}
	ch := make(chan string, 512)

	m.outMu.Lock()
	s := m.ensureStream(key)
	// Replay buffered chunks so a late subscriber doesn't miss anything.
	for _, c := range s.chunks {
		ch <- c
	}
	if s.closed {
		close(ch)
		m.outMu.Unlock()
		return ch, func() {}
	}
	s.subs = append(s.subs, ch)
	m.outMu.Unlock()

	unsub := func() {
		m.outMu.Lock()
		if st, ok := m.outputs[key]; ok {
			for i, sub := range st.subs {
				if sub == ch {
					st.subs = append(st.subs[:i], st.subs[i+1:]...)
					break
				}
			}
		}
		m.outMu.Unlock()
	}
	return ch, unsub
}

func (m *Manager) ensureStream(key outputKey) *outputStream {
	s, ok := m.outputs[key]
	if !ok {
		s = &outputStream{}
		m.outputs[key] = s
	}
	return s
}

// ── Shell sessions ────────────────────────────────────────────────────────────

// RegisterSession creates a new shell session and returns its ID and the
// channel on which the device's output will be sent to the browser.
func (m *Manager) RegisterSession() (uuid.UUID, <-chan []byte) {
	id := uuid.New()
	ch := make(chan []byte, 512)
	m.sessionMu.Lock()
	m.sessions[id] = ch
	m.sessionMu.Unlock()
	return id, ch
}

// UnregisterSession removes a session and closes its output channel.
func (m *Manager) UnregisterSession(id uuid.UUID) {
	m.sessionMu.Lock()
	if ch, ok := m.sessions[id]; ok {
		delete(m.sessions, id)
		close(ch)
	}
	m.sessionMu.Unlock()
}

func (m *Manager) forwardShellOutput(sessionID uuid.UUID, data []byte) {
	m.sessionMu.RLock()
	ch, ok := m.sessions[sessionID]
	m.sessionMu.RUnlock()
	if !ok {
		return
	}
	select {
	case ch <- data:
	default:
		log.Printf("[shell] shell output buffer full for session %s", sessionID)
	}
}

func (m *Manager) closeShellSession(sessionID uuid.UUID) {
	m.UnregisterSession(sessionID)
}
