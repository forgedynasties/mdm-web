package remote

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"mdm/internal/ws"
)

// frameChanSize buffers a few frames per session so a brief browser-write lag doesn't
// force a drop. Kept small so a slow operator can't build up latency — RelayFrame still
// drops the oldest frame when this fills, so the browser always gets the freshest.
const frameChanSize = 8

type Session struct {
	DeviceID  uuid.UUID
	frameCh   chan []byte
	inputCh   chan []byte
	closeOnce sync.Once
}

// remoteToken is a single-use, short-lived authorization for one device's remote
// session. The dashboard (which is session-authenticated) mints one and hands it to
// the browser; the device API redeems it when the control WebSocket connects. This
// replaces embedding the admin API key in the page / WebSocket URL.
//
// It is NOT bound to the client IP: reverse proxies commonly forward X-Forwarded-For
// on the page load but not on the WebSocket upgrade, so the mint saw the real browser
// IP while redeem saw the proxy IP — a mismatch that rejected every legitimate session.
// Single-use + a 2-minute TTL + a 256-bit unguessable token + the device match below
// are the security properties we keep.
type remoteToken struct {
	deviceID uuid.UUID
	expires  time.Time
}

// Manager tracks active remote-control sessions and relays frames and input
// events between device WebSocket connections and dashboard WebSocket connections.
type Manager struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]*Session
	hub      *ws.Hub

	tokenMu sync.Mutex
	tokens  map[string]remoteToken
}

func New(hub *ws.Hub) *Manager {
	return &Manager{
		sessions: make(map[uuid.UUID]*Session),
		hub:      hub,
		tokens:   make(map[string]remoteToken),
	}
}

// IssueToken mints a single-use token authorizing a remote session for deviceID,
// valid for ttl. Returns "" only if the system RNG fails.
func (m *Manager) IssueToken(deviceID uuid.UUID, ttl time.Duration) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	tok := hex.EncodeToString(buf)
	now := time.Now()
	m.tokenMu.Lock()
	m.tokens[tok] = remoteToken{deviceID: deviceID, expires: now.Add(ttl)}
	for k, v := range m.tokens { // opportunistic sweep of expired entries
		if now.After(v.expires) {
			delete(m.tokens, k)
		}
	}
	m.tokenMu.Unlock()
	return tok
}

// RedeemToken consumes a token (single use) and returns the device it authorizes.
// Returns false if the token is unknown or expired. It is deleted on the first lookup
// (whether or not it had expired), so a leaked token can be used at most once within
// its short lifetime.
func (m *Manager) RedeemToken(tok string) (uuid.UUID, bool) {
	if tok == "" {
		return uuid.Nil, false
	}
	m.tokenMu.Lock()
	defer m.tokenMu.Unlock()
	e, ok := m.tokens[tok]
	if !ok {
		return uuid.Nil, false
	}
	delete(m.tokens, tok)
	if time.Now().After(e.expires) {
		return uuid.Nil, false
	}
	return e.deviceID, true
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
	// Delete AND close under the same lock RelayFrame holds across its send, so a
	// frame can never be relayed onto a channel that Stop is closing (which would
	// panic the device's ReadPump goroutine — send on a closed channel).
	m.mu.Lock()
	s, ok := m.sessions[deviceID]
	if ok {
		delete(m.sessions, deviceID)
		s.closeOnce.Do(func() {
			close(s.frameCh)
			close(s.inputCh)
		})
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	stopMsg, _ := json.Marshal(map[string]any{"type": "stop_capture"})
	m.hub.Push(deviceID, stopMsg)
	log.Printf("[remote] session stopped for device %s", deviceID)
}

// RelayFrame is called by the hub when a binary frame arrives from the device.
// Drops the oldest frame in the channel if the buffer is full so the dashboard
// always gets the freshest frame. The lock is held across the send (the sends are
// all non-blocking) so Stop cannot close frameCh mid-send.
func (m *Manager) RelayFrame(deviceID uuid.UUID, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[deviceID]
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
