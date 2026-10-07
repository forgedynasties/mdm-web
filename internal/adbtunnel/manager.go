// Package adbtunnel lets an admin reach a device's wireless adb from anywhere through
// the MDM server, without the device being reachable from the internet.
//
// The shape: the admin opens a *session* for one device from the dashboard. The server
// binds a TCP port from a small range and the admin runs `adb connect <host>:<port>`.
// Each TCP connection accepted there becomes a *stream*: the server asks the device
// (over its normal command WebSocket) to open one, the agent dials 127.0.0.1:5555 and
// opens a second WebSocket back to the server, and the two legs are piped byte for byte.
//
// What keeps a public port from being an open adb shell:
//
//   - the port only exists while the session does — closed on End, after idleTimeout
//     with no traffic, and at maxLife regardless;
//   - the listener accepts only from the IP that opened the session (the dashboard
//     request's client IP, or an address the admin typed), anything else is dropped
//     before a byte is read;
//   - the port is picked at random inside the range;
//   - adbd's own RSA key check still applies on the device (ro.adb.secure=1 images).
//
// Everything is in memory: a server restart drops every session, which is the safe
// direction. Nothing here touches the database.
package adbtunnel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"mdm/internal/ws"
)

const (
	// idleTimeout closes a session that has carried no bytes for this long.
	idleTimeout = 60 * time.Minute
	// maxLife closes a session no matter what: a forgotten tunnel is not a permanent port.
	maxLife = 8 * time.Hour
	// deviceDial is how long an accepted TCP connection waits for the device's leg.
	deviceDial = 25 * time.Second
	// DevicePort is the adbd port the agent dials on the device.
	DevicePort = 5555
)

var (
	ErrNoPorts       = errors.New("no free tunnel port")
	ErrNotConfigured = errors.New("adb tunnels are not configured on this server (ADB_TUNNEL_PORTS)")
	ErrNoSession     = errors.New("no such tunnel session")
	ErrNoStream      = errors.New("no such tunnel stream")
)

// Session is one admin's tunnel to one device.
type Session struct {
	ID        string
	DeviceID  uuid.UUID
	Serial    string
	User      string
	AllowFrom string // the IPs / CIDRs the listener accepts from, comma-separated
	Port      int
	CreatedAt time.Time
	ExpiresAt time.Time

	lastActive int64 // unix nanos, atomic
	bytesUp    int64 // host → device
	bytesDown  int64 // device → host
	conns      int64 // TCP connections accepted (lifetime)
	refused    int64 // TCP connections refused by AllowFrom
	lastError  atomic.Value // string: the device's last reported failure
	lastRefused atomic.Value // string: the last source IP the listener turned away

	allowMu sync.RWMutex
	allow   []*net.IPNet
	ln      net.Listener
	mgr     *Manager
	mu      sync.Mutex
	streams map[string]*stream
	closed  chan struct{}
	once    sync.Once
}

// stream is one TCP connection from the adb host, waiting for or piped to one device WS.
type stream struct {
	id    string
	tcp   net.Conn
	ready chan *websocket.Conn
	done  chan struct{}
	once  sync.Once
}

// Manager owns the port range and the live sessions.
type Manager struct {
	hub    *ws.Hub
	host   string
	portLo int
	portHi int

	mu       sync.Mutex
	sessions map[string]*Session
	byDevice map[uuid.UUID]*Session
}

// New builds a manager. ports is "lo-hi" (e.g. "42000-42019"); empty disables tunnels.
// host is what the dashboard tells admins to `adb connect` to; empty means "use the
// dashboard's own host name".
func New(hub *ws.Hub, host, ports string) (*Manager, error) {
	m := &Manager{hub: hub, host: strings.TrimSpace(host), sessions: map[string]*Session{}, byDevice: map[uuid.UUID]*Session{}}
	ports = strings.TrimSpace(ports)
	if ports == "" {
		return m, nil
	}
	lo, hi, ok := strings.Cut(ports, "-")
	if !ok {
		hi = lo
	}
	var err error
	if m.portLo, err = strconv.Atoi(strings.TrimSpace(lo)); err != nil {
		return nil, fmt.Errorf("ADB_TUNNEL_PORTS %q: %w", ports, err)
	}
	if m.portHi, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
		return nil, fmt.Errorf("ADB_TUNNEL_PORTS %q: %w", ports, err)
	}
	if m.portLo < 1024 || m.portHi > 65535 || m.portHi < m.portLo {
		return nil, fmt.Errorf("ADB_TUNNEL_PORTS %q: want lo-hi within 1024-65535", ports)
	}
	go m.reap()
	return m, nil
}

// Enabled is whether a port range is configured.
func (m *Manager) Enabled() bool { return m.portHi > 0 }

// Host is the public host name admins connect to ("" = same as the dashboard).
func (m *Manager) Host() string { return m.host }

// PortRange is "lo-hi" for the settings/health text.
func (m *Manager) PortRange() string {
	if !m.Enabled() {
		return ""
	}
	return fmt.Sprintf("%d-%d", m.portLo, m.portHi)
}

// Open starts a session for deviceID. An existing session for the device is closed
// first (single-operator model, like remote screen). allowFrom is one or more IPs or
// CIDRs, comma-separated: an office with two WAN links sends the browser and adb out
// through different addresses, so one IP is often not enough.
func (m *Manager) Open(deviceID uuid.UUID, serial, user, allowFrom string) (*Session, error) {
	if !m.Enabled() {
		return nil, ErrNotConfigured
	}
	allow, err := parseAllow(allowFrom)
	if err != nil {
		return nil, err
	}
	if !m.hub.IsConnected(deviceID) {
		return nil, errors.New("device is not connected")
	}
	m.mu.Lock()
	if old := m.byDevice[deviceID]; old != nil {
		m.mu.Unlock()
		old.Close("replaced")
		m.mu.Lock()
	}
	ln, port, err := m.listen()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	now := time.Now()
	s := &Session{
		ID: newID(), DeviceID: deviceID, Serial: serial, User: user, AllowFrom: allowText(allow),
		Port: port, CreatedAt: now, ExpiresAt: now.Add(maxLife),
		allow: allow, ln: ln, mgr: m, streams: map[string]*stream{}, closed: make(chan struct{}),
	}
	atomic.StoreInt64(&s.lastActive, now.UnixNano())
	m.sessions[s.ID] = s
	m.byDevice[deviceID] = s
	m.mu.Unlock()
	go s.accept()
	log.Printf("[adbtunnel] session %s: %s port %d for %s from %s", s.ID[:8], serial, port, user, s.AllowFrom)
	return s, nil
}

// listen binds a free port in the range, starting at a random offset so two sessions
// opened in a row don't land on neighbouring, guessable ports. Caller holds m.mu.
func (m *Manager) listen() (net.Listener, int, error) {
	n := m.portHi - m.portLo + 1
	start := 0
	if r, err := rand.Int(rand.Reader, big.NewInt(int64(n))); err == nil {
		start = int(r.Int64())
	}
	for i := 0; i < n; i++ {
		port := m.portLo + (start+i)%n
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err == nil {
			return ln, port, nil
		}
	}
	return nil, 0, ErrNoPorts
}

// Get returns the session with id.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// ForDevice returns the device's live session, if any.
func (m *Manager) ForDevice(deviceID uuid.UUID) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byDevice[deviceID]
	return s, ok
}

// List returns every live session, newest first.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Count is how many sessions are open, for the Server page.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Close ends the session with id. Returns false if there is none.
func (m *Manager) Close(id, why string) bool {
	s, ok := m.Get(id)
	if !ok {
		return false
	}
	s.Close(why)
	return true
}

// AttachDevice hands the device's WebSocket leg to the stream that asked for it, and
// returns a channel closed when the stream is finished so the HTTP handler can block
// until then. The caller must not use conn afterwards.
func (m *Manager) AttachDevice(sessionID, streamID string, deviceID uuid.UUID, conn *websocket.Conn) (<-chan struct{}, error) {
	s, ok := m.Get(sessionID)
	if !ok || s.DeviceID != deviceID {
		return nil, ErrNoSession
	}
	s.mu.Lock()
	st, ok := s.streams[streamID]
	s.mu.Unlock()
	if !ok {
		return nil, ErrNoStream
	}
	select {
	case st.ready <- conn:
		return st.done, nil
	default:
		return nil, errors.New("stream already has a device leg")
	}
}

// AddAllow lets one more IP or CIDR through an existing session, keeping its port — the
// page's "Allow it" after a refused connection.
func (m *Manager) AddAllow(sessionID, allowFrom string) error {
	s, ok := m.Get(sessionID)
	if !ok {
		return ErrNoSession
	}
	more, err := parseAllow(allowFrom)
	if err != nil {
		return err
	}
	s.allowMu.Lock()
	s.allow = append(s.allow, more...)
	s.AllowFrom = allowText(s.allow)
	s.allowMu.Unlock()
	log.Printf("[adbtunnel] session %s: now allows %s", s.ID[:8], s.AllowFrom)
	return nil
}

// DeviceError records why the device could not open a stream (adbd not listening,
// wrong port…), for the page to show instead of a silent hang.
func (m *Manager) DeviceError(sessionID string, deviceID uuid.UUID, msg string) {
	if s, ok := m.Get(sessionID); ok && s.DeviceID == deviceID {
		s.lastError.Store(strings.TrimSpace(msg))
	}
}

func (m *Manager) remove(s *Session) {
	m.mu.Lock()
	if m.sessions[s.ID] == s {
		delete(m.sessions, s.ID)
	}
	if m.byDevice[s.DeviceID] == s {
		delete(m.byDevice, s.DeviceID)
	}
	m.mu.Unlock()
}

// reap closes idle and expired sessions.
func (m *Manager) reap() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		for _, s := range m.List() {
			switch {
			case now.After(s.ExpiresAt):
				s.Close("expired")
			case s.ActiveStreams() == 0 && now.Sub(s.LastActive()) > idleTimeout:
				s.Close("idle")
			}
		}
	}
}

// ── Session ──────────────────────────────────────────────────────────────────

// LastActive is the last moment any byte crossed the tunnel (or it opened).
func (s *Session) LastActive() time.Time { return time.Unix(0, atomic.LoadInt64(&s.lastActive)) }

// IdleFor is how long the session has carried nothing.
func (s *Session) IdleFor() time.Duration { return time.Since(s.LastActive()) }

// ClosesIn is how long until the reaper ends an idle session.
func (s *Session) ClosesIn() time.Duration {
	left := idleTimeout - s.IdleFor()
	if hard := time.Until(s.ExpiresAt); hard < left {
		left = hard
	}
	if left < 0 {
		return 0
	}
	return left
}

// Stats for the page.
func (s *Session) BytesUp() int64   { return atomic.LoadInt64(&s.bytesUp) }
func (s *Session) BytesDown() int64 { return atomic.LoadInt64(&s.bytesDown) }
func (s *Session) Conns() int64     { return atomic.LoadInt64(&s.conns) }
func (s *Session) Refused() int64   { return atomic.LoadInt64(&s.refused) }

// LastRefused is the last source IP the listener turned away ("" = none): what to add
// to the allow list when the admin's adb host leaves through a different address.
func (s *Session) LastRefused() string {
	v, _ := s.lastRefused.Load().(string)
	return v
}

func (s *Session) allowed(ip net.IP) bool {
	s.allowMu.RLock()
	defer s.allowMu.RUnlock()
	for _, n := range s.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// LastError is the device's last reported failure to open a stream ("" = none).
func (s *Session) LastError() string {
	v, _ := s.lastError.Load().(string)
	return v
}

// ActiveStreams is how many adb host connections are piped right now.
func (s *Session) ActiveStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// Connected is whether an adb host is attached right now.
func (s *Session) Connected() bool { return s.ActiveStreams() > 0 }

func (s *Session) touch() { atomic.StoreInt64(&s.lastActive, time.Now().UnixNano()) }

// Close ends the session: listener, every stream, and tells the device to drop its legs.
func (s *Session) Close(why string) {
	s.once.Do(func() {
		close(s.closed)
		_ = s.ln.Close()
		s.mu.Lock()
		streams := make([]*stream, 0, len(s.streams))
		for _, st := range s.streams {
			streams = append(streams, st)
		}
		s.mu.Unlock()
		for _, st := range streams {
			st.finish()
		}
		s.mgr.remove(s)
		msg, _ := json.Marshal(map[string]any{"type": "adb_tunnel_close", "session": s.ID})
		s.mgr.hub.Push(s.DeviceID, msg)
		log.Printf("[adbtunnel] session %s closed (%s): %d conns, %d refused, %d up / %d down",
			s.ID[:8], why, s.Conns(), s.Refused(), s.BytesUp(), s.BytesDown())
	})
}

func (s *Session) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		ip := remoteIP(c)
		if ip == nil || !s.allowed(ip) {
			atomic.AddInt64(&s.refused, 1)
			if ip != nil {
				s.lastRefused.Store(ip.String())
			}
			log.Printf("[adbtunnel] session %s: refused %s (allowed %s)", s.ID[:8], c.RemoteAddr(), s.AllowFrom)
			_ = c.Close()
			continue
		}
		atomic.AddInt64(&s.conns, 1)
		s.touch()
		go s.serve(c)
	}
}

// serve runs one adb host connection: asks the device for a leg, waits, then pipes.
func (s *Session) serve(tcp net.Conn) {
	st := &stream{id: newID()[:16], tcp: tcp, ready: make(chan *websocket.Conn, 1), done: make(chan struct{})}
	s.mu.Lock()
	s.streams[st.id] = st
	s.mu.Unlock()
	defer func() {
		st.finish()
		s.mu.Lock()
		delete(s.streams, st.id)
		s.mu.Unlock()
		s.touch()
	}()

	msg, _ := json.Marshal(map[string]any{
		"type": "adb_tunnel_open", "session": s.ID, "stream": st.id, "port": DevicePort,
	})
	if !s.mgr.hub.Push(s.DeviceID, msg) {
		s.lastError.Store("device is not connected")
		return
	}
	var dev *websocket.Conn
	select {
	case dev = <-st.ready:
	case <-time.After(deviceDial):
		if s.LastError() == "" {
			s.lastError.Store("device did not open its side of the tunnel in time")
		}
		return
	case <-s.closed:
		return
	}
	s.lastError.Store("")
	defer dev.Close()

	// Device → host.
	go func() {
		defer st.finish()
		for {
			mt, data, err := dev.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				// The agent reports a failure as a small JSON text frame.
				var e struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(data, &e) == nil && e.Error != "" {
					s.lastError.Store(e.Error)
				}
				continue
			}
			if _, err := tcp.Write(data); err != nil {
				return
			}
			atomic.AddInt64(&s.bytesDown, int64(len(data)))
			s.touch()
		}
	}()

	// Keep the WebSocket leg alive through nginx/ALB idle timeouts while adb says nothing.
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := dev.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			case <-st.done:
				return
			}
		}
	}()

	// Host → device.
	buf := make([]byte, 32*1024)
	for {
		n, err := tcp.Read(buf)
		if n > 0 {
			if werr := dev.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
				return
			}
			atomic.AddInt64(&s.bytesUp, int64(n))
			s.touch()
		}
		if err != nil {
			return
		}
	}
}

func (st *stream) finish() {
	st.once.Do(func() {
		close(st.done)
		_ = st.tcp.Close()
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return uuid.NewString()
	}
	return hex.EncodeToString(b)
}

func remoteIP(c net.Conn) net.IP {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// parseAllow accepts IPs and CIDRs, comma- or space-separated, as networks.
func parseAllow(list string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, s := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR", s)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	if len(out) == 0 {
		return nil, errors.New("an allowed address is required")
	}
	return out, nil
}

func allowText(nets []*net.IPNet) string {
	parts := make([]string, 0, len(nets))
	for _, n := range nets {
		if ones, bits := n.Mask.Size(); ones == bits {
			parts = append(parts, n.IP.String()) // a single address reads better without /32
		} else {
			parts = append(parts, n.String())
		}
	}
	return strings.Join(parts, ", ")
}

// IdleText / ClosesInText are the page's "idle 4 min" / "closes in 56 min".
func (s *Session) IdleText() string     { return durText(s.IdleFor()) }
func (s *Session) ClosesInText() string { return durText(s.ClosesIn()) }

func durText(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
}
