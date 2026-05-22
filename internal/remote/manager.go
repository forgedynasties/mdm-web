package remote

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/google/uuid"
	"mdm/internal/ws"
)

const frameChanSize = 4

type Session struct {
	DeviceID    uuid.UUID
	frameCh     chan []byte
	inputCh     chan []byte
	closeOnce   sync.Once
}

// Manager tracks active remote-control sessions and relays frames and input
// events between device WebSocket connections and dashboard WebSocket connections.
type Manager struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]*Session
	hub      *ws.Hub
}

func New(hub *ws.Hub) *Manager {
	return &Manager{
		sessions: make(map[uuid.UUID]*Session),
		hub:      hub,
	}
}

// Start creates a session for the device. Returns an error if a session is
// already active for this device.
func (m *Manager) Start(deviceID uuid.UUID) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[deviceID]; ok {
		return nil, ErrSessionActive
	}
	s := &Session{
		DeviceID: deviceID,
		frameCh:  make(chan []byte, frameChanSize),
		inputCh:  make(chan []byte, 32),
	}
	m.sessions[deviceID] = s
	log.Printf("[remote] session started for device %s", deviceID)
	return s, nil
}

// Stop tears down the session and sends a stop_capture command to the device.
func (m *Manager) Stop(deviceID uuid.UUID) {
	m.mu.Lock()
	s, ok := m.sessions[deviceID]
	if ok {
		delete(m.sessions, deviceID)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	s.closeOnce.Do(func() {
		close(s.frameCh)
		close(s.inputCh)
	})
	stopMsg, _ := json.Marshal(map[string]any{"type": "stop_capture"})
	m.hub.Push(deviceID, stopMsg)
	log.Printf("[remote] session stopped for device %s", deviceID)
}

// RelayFrame is called by the hub when a binary frame arrives from the device.
// Drops the oldest frame in the channel if the buffer is full so the dashboard
// always gets the freshest frame.
func (m *Manager) RelayFrame(deviceID uuid.UUID, data []byte) {
	m.mu.Lock()
	s, ok := m.sessions[deviceID]
	m.mu.Unlock()
	if !ok {
		return
	}
	select {
	case s.frameCh <- data:
	default:
		// Buffer full — drop oldest, push newest
		select {
		case <-s.frameCh:
		default:
		}
		select {
		case s.frameCh <- data:
		default:
		}
	}
}

// RelayInput is called by the dashboard WS handler when an input event arrives.
// Forwards the raw JSON to the device via hub.Push.
func (m *Manager) RelayInput(deviceID uuid.UUID, data []byte) {
	m.mu.Lock()
	_, ok := m.sessions[deviceID]
	m.mu.Unlock()
	if !ok {
		return
	}
	m.hub.Push(deviceID, data)
}

// SubscribeFrames returns a channel that receives binary frames from the device.
func (m *Manager) SubscribeFrames(deviceID uuid.UUID) (<-chan []byte, error) {
	m.mu.Lock()
	s, ok := m.sessions[deviceID]
	m.mu.Unlock()
	if !ok {
		return nil, ErrSessionNotActive
	}
	return s.frameCh, nil
}

// ErrSessionActive is returned when starting a session for a device that
// already has one.
var ErrSessionActive = fmtError("remote session already active for this device")

// ErrSessionNotActive is returned when subscribing to a session that doesn't exist.
var ErrSessionNotActive = fmtError("no remote session for this device")

type fmtError string

func (e fmtError) Error() string { return string(e) }
