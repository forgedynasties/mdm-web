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

// Hub maintains the set of active WebSocket clients keyed by device ID.
type Hub struct {
	mu        sync.RWMutex
	clients   map[uuid.UUID]*Client
	onMessage func(deviceID uuid.UUID, msg []byte)
}

// SetOnMessage registers a function that is called for every message received
// from any device. Safe to call before any connections are established.
func (h *Hub) SetOnMessage(fn func(deviceID uuid.UUID, msg []byte)) {
	h.onMessage = fn
}

// Client represents a single device WebSocket connection.
type Client struct {
	DeviceID uuid.UUID
	conn     *websocket.Conn
	Send     chan []byte
	hub      *Hub
}

func NewHub() *Hub {
	return &Hub{clients: make(map[uuid.UUID]*Client)}
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	if old, ok := h.clients[c.DeviceID]; ok {
		close(old.Send)
	}
	h.clients[c.DeviceID] = c
	h.mu.Unlock()
	log.Printf("[ws] connected: %s", c.DeviceID)
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	if cur, ok := h.clients[c.DeviceID]; ok && cur == c {
		delete(h.clients, c.DeviceID)
	}
	h.mu.Unlock()
	log.Printf("[ws] disconnected: %s", c.DeviceID)
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
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) > 0 && c.hub.onMessage != nil {
			c.hub.onMessage(c.DeviceID, msg)
		}
	}
}
