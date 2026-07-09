package ws

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 45 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Upgrader returns the WebSocket upgrader used by the hub.
func Upgrader() websocket.Upgrader { return upgrader }

// PresenceEvent is emitted when a device connects or disconnects.
type PresenceEvent struct {
	DeviceID uuid.UUID
	Online   bool
}

// DeviceUpdateEvent is emitted when dashboard-visible device data changes.
type DeviceUpdateEvent struct {
	DeviceID uuid.UUID
}

// CommandUpdateEvent is emitted when a command delivery status changes.
type CommandUpdateEvent struct {
	CommandID uuid.UUID
}

// LogcatUpdateEvent is emitted when a logcat result is received for a device.
type LogcatUpdateEvent struct {
	DeviceID uuid.UUID
}

// AlertUpdateEvent is emitted when an alert's status changes (ack/resolve). It
// carries no payload — the alerts page just re-fetches the current list.
type AlertUpdateEvent struct{}

// DeploymentUpdateEvent is emitted when an OTA deployment changes — a device's
// rollout progress ticks, or an operator retries/cancels/edits it. Payload-less:
// open deployment pages re-fetch their own (scoped, ETag-cached) targets partial.
type DeploymentUpdateEvent struct{}

// ProblemUpdateEvent is emitted when a release problem (or a QA test result that
// creates/resolves one) changes anywhere. Payload-less: the releases hub board and any
// open release workspace re-fetch their own (release-scoped) fragments.
type ProblemUpdateEvent struct{}

// Hub maintains the set of active WebSocket clients keyed by device ID.
type Hub struct {
	mu              sync.RWMutex
	clients         map[uuid.UUID]*Client
	onMessage       func(deviceID uuid.UUID, msg []byte)
	onBinaryMessage func(deviceID uuid.UUID, data []byte)
	subMu           sync.RWMutex
	subscribers     map[chan PresenceEvent]struct{}
	updateMu        sync.RWMutex
	updates         map[chan DeviceUpdateEvent]struct{}
	updThrottleMu   sync.Mutex
	lastUpdateAt    map[uuid.UUID]time.Time // per-device last broadcast, to coalesce floods
	cmdMu           sync.RWMutex
	cmdUpdates      map[chan CommandUpdateEvent]struct{}
	logcatMu        sync.RWMutex
	logcatUpdates   map[chan LogcatUpdateEvent]struct{}
	alertMu         sync.RWMutex
	alertUpdates    map[chan AlertUpdateEvent]struct{}
	deployMu        sync.RWMutex
	deployUpdates   map[chan DeploymentUpdateEvent]struct{}
	problemMu       sync.RWMutex
	problemUpdates  map[chan ProblemUpdateEvent]struct{}
	pingMu          sync.Mutex
	pingWaiters     map[string]chan struct{}
}

// SetOnMessage registers a function that is called for every text message
// received from any device. Safe to call before any connections are established.
func (h *Hub) SetOnMessage(fn func(deviceID uuid.UUID, msg []byte)) {
	h.onMessage = fn
}

// SetOnBinaryMessage registers a function that is called for every binary
// message received from any device.
func (h *Hub) SetOnBinaryMessage(fn func(deviceID uuid.UUID, data []byte)) {
	h.onBinaryMessage = fn
}

// Client represents a single device WebSocket connection.
type Client struct {
	DeviceID uuid.UUID
	conn     *websocket.Conn
	Send     chan []byte
	hub      *Hub
}

func NewHub() *Hub {
	return &Hub{
		clients:       make(map[uuid.UUID]*Client),
		subscribers:   make(map[chan PresenceEvent]struct{}),
		updates:       make(map[chan DeviceUpdateEvent]struct{}),
		lastUpdateAt:  make(map[uuid.UUID]time.Time),
		cmdUpdates:    make(map[chan CommandUpdateEvent]struct{}),
		logcatUpdates: make(map[chan LogcatUpdateEvent]struct{}),
		alertUpdates:  make(map[chan AlertUpdateEvent]struct{}),
		deployUpdates:  make(map[chan DeploymentUpdateEvent]struct{}),
		problemUpdates: make(map[chan ProblemUpdateEvent]struct{}),
		pingWaiters:    make(map[string]chan struct{}),
	}
}

// RegisterPingWaiter registers a channel that will be closed when a
// pong_response with the given nonce arrives from any device.
func (h *Hub) RegisterPingWaiter(nonce string) chan struct{} {
	ch := make(chan struct{})
	h.pingMu.Lock()
	h.pingWaiters[nonce] = ch
	h.pingMu.Unlock()
	return ch
}

func (h *Hub) UnregisterPingWaiter(nonce string) {
	h.pingMu.Lock()
	delete(h.pingWaiters, nonce)
	h.pingMu.Unlock()
}

// SignalPong is called by the message dispatcher when a pong_response arrives.
func (h *Hub) SignalPong(nonce string) {
	h.pingMu.Lock()
	ch, ok := h.pingWaiters[nonce]
	if ok {
		delete(h.pingWaiters, nonce)
	}
	h.pingMu.Unlock()
	if ok {
		close(ch)
	}
}


// SubscribeCommandUpdates returns a channel that receives command update events.
func (h *Hub) SubscribeCommandUpdates() chan CommandUpdateEvent {
	ch := make(chan CommandUpdateEvent, 32)
	h.cmdMu.Lock()
	h.cmdUpdates[ch] = struct{}{}
	h.cmdMu.Unlock()
	return ch
}

// UnsubscribeCommandUpdates closes the channel and removes it from the subscriber set.
func (h *Hub) UnsubscribeCommandUpdates(ch chan CommandUpdateEvent) {
	h.cmdMu.Lock()
	if _, ok := h.cmdUpdates[ch]; ok {
		delete(h.cmdUpdates, ch)
		close(ch)
	}
	h.cmdMu.Unlock()
}

// PublishCommandUpdate notifies subscribers that a command delivery changed.
func (h *Hub) PublishCommandUpdate(commandID uuid.UUID) {
	ev := CommandUpdateEvent{CommandID: commandID}
	h.cmdMu.RLock()
	defer h.cmdMu.RUnlock()
	for ch := range h.cmdUpdates {
		select {
		case ch <- ev:
		default:
		}
	}
}

// SubscribeAlertUpdates returns a channel that receives alert update events.
func (h *Hub) SubscribeAlertUpdates() chan AlertUpdateEvent {
	ch := make(chan AlertUpdateEvent, 32)
	h.alertMu.Lock()
	h.alertUpdates[ch] = struct{}{}
	h.alertMu.Unlock()
	return ch
}

// UnsubscribeAlertUpdates closes the channel and removes it from the subscriber set.
func (h *Hub) UnsubscribeAlertUpdates(ch chan AlertUpdateEvent) {
	h.alertMu.Lock()
	if _, ok := h.alertUpdates[ch]; ok {
		delete(h.alertUpdates, ch)
		close(ch)
	}
	h.alertMu.Unlock()
}

// PublishAlertUpdate notifies subscribers that an alert's status changed.
func (h *Hub) PublishAlertUpdate() {
	h.alertMu.RLock()
	defer h.alertMu.RUnlock()
	for ch := range h.alertUpdates {
		select {
		case ch <- AlertUpdateEvent{}:
		default:
		}
	}
}

// SubscribeDeploymentUpdates returns a channel that receives deployment update events.
func (h *Hub) SubscribeDeploymentUpdates() chan DeploymentUpdateEvent {
	ch := make(chan DeploymentUpdateEvent, 32)
	h.deployMu.Lock()
	h.deployUpdates[ch] = struct{}{}
	h.deployMu.Unlock()
	return ch
}

// UnsubscribeDeploymentUpdates closes the channel and removes it from the subscriber set.
func (h *Hub) UnsubscribeDeploymentUpdates(ch chan DeploymentUpdateEvent) {
	h.deployMu.Lock()
	if _, ok := h.deployUpdates[ch]; ok {
		delete(h.deployUpdates, ch)
		close(ch)
	}
	h.deployMu.Unlock()
}

// PublishDeploymentUpdate notifies subscribers that a deployment changed.
func (h *Hub) PublishDeploymentUpdate() {
	h.deployMu.RLock()
	defer h.deployMu.RUnlock()
	for ch := range h.deployUpdates {
		select {
		case ch <- DeploymentUpdateEvent{}:
		default:
		}
	}
}

// SubscribeProblemUpdates returns a channel that receives release-problem update events.
func (h *Hub) SubscribeProblemUpdates() chan ProblemUpdateEvent {
	ch := make(chan ProblemUpdateEvent, 32)
	h.problemMu.Lock()
	h.problemUpdates[ch] = struct{}{}
	h.problemMu.Unlock()
	return ch
}

// UnsubscribeProblemUpdates closes the channel and removes it from the subscriber set.
func (h *Hub) UnsubscribeProblemUpdates(ch chan ProblemUpdateEvent) {
	h.problemMu.Lock()
	if _, ok := h.problemUpdates[ch]; ok {
		delete(h.problemUpdates, ch)
		close(ch)
	}
	h.problemMu.Unlock()
}

// PublishProblemUpdate notifies subscribers that a release problem (or a QA result that
// creates/resolves one) changed.
func (h *Hub) PublishProblemUpdate() {
	h.problemMu.RLock()
	defer h.problemMu.RUnlock()
	for ch := range h.problemUpdates {
		select {
		case ch <- ProblemUpdateEvent{}:
		default:
		}
	}
}

// SubscribeLogcatUpdates returns a channel that receives logcat update events.
func (h *Hub) SubscribeLogcatUpdates() chan LogcatUpdateEvent {
	ch := make(chan LogcatUpdateEvent, 32)
	h.logcatMu.Lock()
	h.logcatUpdates[ch] = struct{}{}
	h.logcatMu.Unlock()
	return ch
}

// UnsubscribeLogcatUpdates closes the channel and removes it from the subscriber set.
func (h *Hub) UnsubscribeLogcatUpdates(ch chan LogcatUpdateEvent) {
	h.logcatMu.Lock()
	if _, ok := h.logcatUpdates[ch]; ok {
		delete(h.logcatUpdates, ch)
		close(ch)
	}
	h.logcatMu.Unlock()
}

// PublishLogcatUpdate notifies subscribers that a logcat result arrived for a device.
func (h *Hub) PublishLogcatUpdate(deviceID uuid.UUID) {
	ev := LogcatUpdateEvent{DeviceID: deviceID}
	h.logcatMu.RLock()
	defer h.logcatMu.RUnlock()
	for ch := range h.logcatUpdates {
		select {
		case ch <- ev:
		default:
		}
	}
}

// SubscribePresence returns a channel that receives presence events for all devices.
// Callers must invoke UnsubscribePresence to release resources.
func (h *Hub) SubscribePresence() chan PresenceEvent {
	ch := make(chan PresenceEvent, 32)
	h.subMu.Lock()
	h.subscribers[ch] = struct{}{}
	h.subMu.Unlock()
	return ch
}

// UnsubscribePresence closes the channel and removes it from the subscriber set.
func (h *Hub) UnsubscribePresence(ch chan PresenceEvent) {
	h.subMu.Lock()
	if _, ok := h.subscribers[ch]; ok {
		delete(h.subscribers, ch)
		close(ch)
	}
	h.subMu.Unlock()
}

// SubscribeDeviceUpdates returns a channel that receives device update events.
// Callers must invoke UnsubscribeDeviceUpdates to release resources.
func (h *Hub) SubscribeDeviceUpdates() chan DeviceUpdateEvent {
	ch := make(chan DeviceUpdateEvent, 64)
	h.updateMu.Lock()
	h.updates[ch] = struct{}{}
	h.updateMu.Unlock()
	return ch
}

// UnsubscribeDeviceUpdates closes the channel and removes it from the subscriber set.
func (h *Hub) UnsubscribeDeviceUpdates(ch chan DeviceUpdateEvent) {
	h.updateMu.Lock()
	if _, ok := h.updates[ch]; ok {
		delete(h.updates, ch)
		close(ch)
	}
	h.updateMu.Unlock()
}

func (h *Hub) publishPresence(ev PresenceEvent) {
	h.subMu.RLock()
	defer h.subMu.RUnlock()
	for ch := range h.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

// deviceUpdateThrottle bounds how often a single device may push a live update to
// dashboards. A misbehaving unit (e.g. a faulty charger toggling on/off every second)
// checks in constantly; without this it would repaint the fleet card and re-fetch the
// device page many times a second. One update per device per window is plenty for the UI.
const deviceUpdateThrottle = 4 * time.Second

// PublishDeviceUpdate notifies dashboard subscribers that a device changed, rate-limited
// per device so a check-in flood can't spam the dashboard.
func (h *Hub) PublishDeviceUpdate(deviceID uuid.UUID) {
	h.updThrottleMu.Lock()
	now := time.Now()
	if last, ok := h.lastUpdateAt[deviceID]; ok && now.Sub(last) < deviceUpdateThrottle {
		h.updThrottleMu.Unlock()
		return
	}
	h.lastUpdateAt[deviceID] = now
	h.updThrottleMu.Unlock()

	ev := DeviceUpdateEvent{DeviceID: deviceID}
	h.updateMu.RLock()
	defer h.updateMu.RUnlock()
	for ch := range h.updates {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	if old, ok := h.clients[c.DeviceID]; ok {
		close(old.Send)
	}
	h.clients[c.DeviceID] = c
	h.mu.Unlock()
	log.Printf("[ws] connected: %s", c.DeviceID)
	h.publishPresence(PresenceEvent{DeviceID: c.DeviceID, Online: true})
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	removed := false
	if cur, ok := h.clients[c.DeviceID]; ok && cur == c {
		delete(h.clients, c.DeviceID)
		removed = true
	}
	h.mu.Unlock()
	log.Printf("[ws] disconnected: %s", c.DeviceID)
	if removed {
		h.publishPresence(PresenceEvent{DeviceID: c.DeviceID, Online: false})
	}
}

// Push sends msg to a specific device. Returns true if the device is connected.
func (h *Hub) Push(deviceID uuid.UUID, msg []byte) bool {
	h.mu.RLock()
	c, ok := h.clients[deviceID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case c.Send <- msg:
		return true
	default:
		log.Printf("[ws] send buffer full for device %s, dropping message", deviceID)
		return false
	}
}

// IsConnected reports whether the device currently has an active WebSocket connection.
func (h *Hub) IsConnected(deviceID uuid.UUID) bool {
	h.mu.RLock()
	_, ok := h.clients[deviceID]
	h.mu.RUnlock()
	return ok
}

// ConnectedIDs returns the set of device IDs with active connections.
func (h *Hub) ConnectedIDs() map[uuid.UUID]struct{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make(map[uuid.UUID]struct{}, len(h.clients))
	for id := range h.clients {
		ids[id] = struct{}{}
	}
	return ids
}

// PushToDevices sends msg to each device in the list.
func (h *Hub) PushToDevices(deviceIDs []uuid.UUID, msg []byte) {
	for _, id := range deviceIDs {
		h.Push(id, msg)
	}
}

// Broadcast sends msg to all currently connected devices.
func (h *Hub) Broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		select {
		case c.Send <- msg:
		default:
		}
	}
}

// Upgrade performs the HTTP→WebSocket upgrade and registers the client with the hub.
func (h *Hub) Upgrade(w http.ResponseWriter, r *http.Request, deviceID uuid.UUID) (*Client, error) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	c := &Client{
		DeviceID: deviceID,
		conn:     conn,
		Send:     make(chan []byte, 256),
		hub:      h,
	}
	h.register(c)
	return c, nil
}

// WritePump drains c.Send to the WebSocket connection and sends periodic pings.
// Must be called in its own goroutine.
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.Send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ReadPump reads from the WebSocket, dispatching messages and detecting disconnection.
// Must be called in its own goroutine. Unregisters the client on return.
func (c *Client) ReadPump() {
	defer func() {
		c.hub.Unregister(c)
		c.conn.Close()
	}()
	c.conn.SetReadLimit(512 * 1024) // 512 KB — large enough for shell/screenshot output
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		mt, msg, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) == 0 {
			continue
		}
		switch mt {
		case websocket.TextMessage:
			if c.hub.onMessage != nil {
				c.hub.onMessage(c.DeviceID, msg)
			}
		case websocket.BinaryMessage:
			if c.hub.onBinaryMessage != nil {
				c.hub.onBinaryMessage(c.DeviceID, msg)
			}
		}
	}
}
