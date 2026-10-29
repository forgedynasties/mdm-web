// Package cluster lets several server copies share one fleet (scalability plan W5, 29
// Oct 2026), with Postgres as the only link between them: no new service.
//
// Each device's WebSocket lives in one copy. That copy records itself as the device's
// owner (device_owners) and heartbeats (cluster_instances). Another copy that needs to
// reach the device — a command, a config push, a lock — sends the message on the
// mdm_cluster NOTIFY channel and the owner pushes it down the socket. Dashboard events
// (device, command, alert… updates, presence, a Lock button's pong) go out the same way,
// so a browser on any copy sees every device. Commands were already stored before being
// pushed and re-delivered on check-in, so a notification lost while a copy reconnects
// costs a delay, never a command.
//
// Not covered yet: remote screen, shell and logcat relays still need the browser on the
// copy that holds the device's socket (the plan's proxy between copies).
package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"expvar"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/ws"
)

// Channel is the NOTIFY channel every copy listens on.
const Channel = "mdm_cluster"

// notifyMax is the largest payload sent inline; NOTIFY refuses over 8000 bytes.
const notifyMax = 7000

const (
	heartbeatEvery = 10 * time.Second
	refreshEvery   = 3 * time.Second
	ownerFlush     = 500 * time.Millisecond
	ownerResync    = time.Minute
	sweepEvery     = time.Hour
)

type msg struct {
	From  string `json:"f"`
	Kind  string `json:"k"`           // push | pushref | ev
	Dev   string `json:"d,omitempty"` // push, pushref
	Body  string `json:"m,omitempty"` // push: base64 of the frame
	Ref   int64  `json:"r,omitempty"` // pushref: cluster_msgs id
	Event string `json:"e,omitempty"` // ev: ws.Ev*
	ID    string `json:"i,omitempty"` // ev
}

type ownChange struct {
	id        uuid.UUID
	connected bool
}

// Cluster implements ws.Cluster over Postgres.
type Cluster struct {
	db   *db.DB
	hub  *ws.Hub
	id   string
	addr string

	remote atomic.Pointer[map[uuid.UUID]struct{}] // replaced whole, never mutated after
	addMu  sync.Mutex                             // serializes copy-on-write additions

	own chan ownChange

	sent, recv, forwards, dropped, errs atomic.Int64
}

// New makes this copy's cluster member. id must be unique per process; addr is how other
// copies reach it on the private network (for the session proxy to come).
func New(d *db.DB, hub *ws.Hub, id, addr string) *Cluster {
	c := &Cluster{db: d, hub: hub, id: id, addr: addr, own: make(chan ownChange, 20000)}
	empty := map[uuid.UUID]struct{}{}
	c.remote.Store(&empty)
	return c
}

// Start runs the heartbeat, the owner writer, the remote refresh, the listener and the
// sweep until ctx ends, then leaves the cluster.
func (c *Cluster) Start(ctx context.Context) {
	if err := c.db.ClusterHeartbeat(ctx, c.id, c.addr); err != nil {
		log.Printf("[cluster] first heartbeat: %v", err)
	}
	go c.loop(ctx, heartbeatEvery, func() {
		if err := c.db.ClusterHeartbeat(ctx, c.id, c.addr); err != nil {
			c.errs.Add(1)
			log.Printf("[cluster] heartbeat: %v", err)
		}
	})
	go c.loop(ctx, refreshEvery, c.refresh)
	go c.loop(ctx, sweepEvery, func() {
		if err := c.db.ClusterSweep(ctx); err != nil {
			log.Printf("[cluster] sweep: %v", err)
		}
	})
	go c.writeOwners(ctx)
	go c.listen(ctx)
	go func() {
		<-ctx.Done()
		lctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.db.ClusterLeave(lctx, c.id); err != nil {
			log.Printf("[cluster] leave: %v", err)
		}
	}()
	c.refresh()
	log.Printf("[cluster] joined as %s", c.id)
}

func (c *Cluster) loop(ctx context.Context, every time.Duration, fn func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// ── ws.Cluster ───────────────────────────────────────────────────────────────

// Owned queues an ownership change for the writer. Never blocks: if the queue is full
// the change is dropped and the minute resync puts it right.
func (c *Cluster) Owned(id uuid.UUID, connected bool) {
	select {
	case c.own <- ownChange{id, connected}:
	default:
		c.dropped.Add(1)
	}
}

// Remote is the devices on live copies other than this one (refreshed every few seconds).
func (c *Cluster) Remote() map[uuid.UUID]struct{} { return *c.remote.Load() }

// Forward hands msg to the copy holding the device's socket.
func (c *Cluster) Forward(id uuid.UUID, frame []byte) bool {
	if _, ok := c.Remote()[id]; !ok {
		return false
	}
	m := msg{From: c.id, Kind: "push", Dev: id.String()}
	if base64.StdEncoding.EncodedLen(len(frame)) <= notifyMax {
		m.Body = base64.StdEncoding.EncodeToString(frame)
	} else {
		ref, err := c.db.ClusterPutMsg(context.Background(), frame)
		if err != nil {
			c.errs.Add(1)
			log.Printf("[cluster] store a large push for %s: %v", id, err)
			return false
		}
		m.Kind, m.Ref = "pushref", ref
	}
	if !c.send(m) {
		return false
	}
	c.forwards.Add(1)
	return true
}

// Event passes a hub event to the other copies.
func (c *Cluster) Event(kind, id string) {
	c.send(msg{From: c.id, Kind: "ev", Event: kind, ID: id})
}

func (c *Cluster) send(m msg) bool {
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.db.ClusterNotify(ctx, Channel, string(b)); err != nil {
		c.errs.Add(1)
		log.Printf("[cluster] notify %s: %v", m.Kind, err)
		return false
	}
	c.sent.Add(1)
	return true
}

// ── Loops ────────────────────────────────────────────────────────────────────

func (c *Cluster) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := c.db.ClusterRemoteDevices(ctx, c.id)
	if err != nil {
		c.errs.Add(1)
		log.Printf("[cluster] remote devices: %v", err)
		return
	}
	c.addMu.Lock()
	c.remote.Store(&m)
	c.addMu.Unlock()
}

// noteRemote marks a device as on another copy straight away (its "online" event), so a
// push in the seconds before the next refresh still finds it.
func (c *Cluster) noteRemote(id uuid.UUID, online bool) {
	c.addMu.Lock()
	defer c.addMu.Unlock()
	cur := *c.remote.Load()
	if _, has := cur[id]; has == online {
		return
	}
	next := make(map[uuid.UUID]struct{}, len(cur)+1)
	for k := range cur {
		next[k] = struct{}{}
	}
	if online {
		next[id] = struct{}{}
	} else {
		delete(next, id)
	}
	c.remote.Store(&next)
}

// writeOwners batches ownership changes into one write every half second, and every
// minute re-asserts every socket this copy holds (heals a dropped or failed write).
func (c *Cluster) writeOwners(ctx context.Context) {
	flush := time.NewTicker(ownerFlush)
	resync := time.NewTicker(ownerResync)
	defer flush.Stop()
	defer resync.Stop()
	pending := map[uuid.UUID]bool{}
	write := func(owned, lost []uuid.UUID) {
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.db.ClusterSetOwners(wctx, c.id, owned, lost); err != nil {
			c.errs.Add(1)
			log.Printf("[cluster] owners: %v", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ch := <-c.own:
			pending[ch.id] = ch.connected
		case <-flush.C:
			if len(pending) == 0 {
				continue
			}
			var owned, lost []uuid.UUID
			for id, on := range pending {
				if on {
					owned = append(owned, id)
				} else {
					lost = append(lost, id)
				}
			}
			pending = map[uuid.UUID]bool{}
			write(owned, lost)
		case <-resync.C:
			local := c.hub.LocalIDs()
			owned := make([]uuid.UUID, 0, len(local))
			for id := range local {
				owned = append(owned, id)
			}
			write(owned, nil)
		}
	}
}

// listen receives the other copies' messages, reconnecting if the connection drops.
func (c *Cluster) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.listenOnce(ctx); err != nil && ctx.Err() == nil {
			c.errs.Add(1)
			log.Printf("[cluster] listen: %v (reconnecting)", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (c *Cluster) listenOnce(ctx context.Context) error {
	conn, err := c.db.ListenConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var m msg
		if json.Unmarshal([]byte(n.Payload), &m) != nil || m.From == c.id {
			continue // ours, echoed back
		}
		c.recv.Add(1)
		c.handle(ctx, m)
	}
}

func (c *Cluster) handle(ctx context.Context, m msg) {
	switch m.Kind {
	case "push", "pushref":
		id, err := uuid.Parse(m.Dev)
		if err != nil {
			return
		}
		var frame []byte
		if m.Kind == "push" {
			frame, err = base64.StdEncoding.DecodeString(m.Body)
		} else {
			frame, err = c.db.ClusterGetMsg(ctx, m.Ref)
		}
		if err != nil {
			log.Printf("[cluster] push for %s: %v", m.Dev, err)
			return
		}
		c.hub.DeliverForwarded(id, frame) // not here (it just moved): the check-in re-delivers
	case "ev":
		if id, err := uuid.Parse(m.ID); err == nil && (m.Event == ws.EvPresenceOn || m.Event == ws.EvPresenceOff) {
			c.noteRemote(id, m.Event == ws.EvPresenceOn)
		}
		c.hub.DeliverEvent(m.Event, m.ID)
	}
}

// Stats is the cluster's picture for /debug/vars.
func (c *Cluster) Stats() map[string]any {
	return map[string]any{
		"instance": c.id, "remote_devices": len(c.Remote()), "local_devices": len(c.hub.LocalIDs()),
		"sent": c.sent.Load(), "received": c.recv.Load(), "forwarded_pushes": c.forwards.Load(),
		"owner_changes_dropped": c.dropped.Load(), "errors": c.errs.Load(),
	}
}

// Publish registers the cluster's stats as /debug/vars "cluster".
func (c *Cluster) Publish() { expvar.Publish("cluster", expvar.Func(func() any { return c.Stats() })) }
