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
	"net/http"
	"net/http/httputil"
	"net/url"
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
	Evs   []ev   `json:"v,omitempty"` // evs: several events in one notification
}

// ev is one hub event inside an "evs" batch.
type ev struct {
	E string `json:"e"`
	I string `json:"i"`
}

const (
	// eventFlush is how long events gather before going out as one notification: a fleet
	// dropping at once (a deploy) was one NOTIFY per device, which took the whole pool.
	eventFlush = 50 * time.Millisecond
	// eventsPerNotify keeps a batch under notifyMax (an event is ~60 bytes of JSON).
	eventsPerNotify = 90
)

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
	evs chan ev

	sent, recv, forwards, dropped, errs, proxied atomic.Int64
}

// New makes this copy's cluster member. id must be unique per process; addr is how other
// copies reach it on the private network (for the session proxy to come).
func New(d *db.DB, hub *ws.Hub, id, addr string) *Cluster {
	c := &Cluster{db: d, hub: hub, id: id, addr: addr, own: make(chan ownChange, 20000), evs: make(chan ev, 50000)}
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
	go c.sendEvents(ctx)
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

// Event passes a hub event to the other copies, in the next batch. Never blocks: a full
// queue drops the event (a dashboard refresh, at worst, comes late).
func (c *Cluster) Event(kind, id string) {
	select {
	case c.evs <- ev{E: kind, I: id}:
	default:
		c.dropped.Add(1)
	}
}

// sendEvents gathers events for eventFlush and sends them in as few notifications as fit.
func (c *Cluster) sendEvents(ctx context.Context) {
	for {
		var first ev
		select {
		case <-ctx.Done():
			return
		case first = <-c.evs:
		}
		batch := []ev{first}
		timer := time.NewTimer(eventFlush)
	gather:
		for {
			select {
			case e := <-c.evs:
				batch = append(batch, e)
			case <-timer.C:
				break gather
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		for len(batch) > 0 {
			n := min(len(batch), eventsPerNotify)
			c.send(msg{From: c.id, Kind: "evs", Evs: batch[:n]})
			batch = batch[n:]
		}
	}
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
		c.deliver(m.Event, m.ID)
	case "evs":
		for _, e := range m.Evs {
			c.deliver(e.E, e.I)
		}
	}
}

// deliver hands one event from another copy to this copy's hub (and notes presence).
func (c *Cluster) deliver(kind, id string) {
	if u, err := uuid.Parse(id); err == nil && (kind == ws.EvPresenceOn || kind == ws.EvPresenceOff) {
		c.noteRemote(u, kind == ws.EvPresenceOn)
	}
	c.hub.DeliverEvent(kind, id)
}

// Stats is the cluster's picture for /debug/vars.
func (c *Cluster) Stats() map[string]any {
	return map[string]any{
		"instance": c.id, "remote_devices": len(c.Remote()), "local_devices": len(c.hub.LocalIDs()),
		"sent": c.sent.Load(), "received": c.recv.Load(), "forwarded_pushes": c.forwards.Load(),
		"dropped": c.dropped.Load(), "errors": c.errs.Load(), "proxied_sessions": c.proxied.Load(),
	}
}

// Publish registers the cluster's stats as /debug/vars "cluster".
func (c *Cluster) Publish() { expvar.Publish("cluster", expvar.Func(func() any { return c.Stats() })) }

// ── Sessions on another copy ────────────────────────────────────────────────

// ProxiedHeader marks a request one copy passed to another, so it is never passed on again.
const ProxiedHeader = "X-Mdm-Cluster-Proxied"

// ProxyIfRemote serves r from the copy that holds deviceID's socket, when that is not this
// one: remote screen, shell and logcat run where the device's frames and output arrive.
// It reports whether it handled the request. Websocket upgrades and streams pass through
// (the reverse proxy flushes at once).
func (c *Cluster) ProxyIfRemote(w http.ResponseWriter, r *http.Request, deviceID uuid.UUID) bool {
	if r.Header.Get(ProxiedHeader) != "" {
		return false
	}
	if _, here := c.hub.LocalIDs()[deviceID]; here {
		return false
	}
	owner, addr, ok, err := c.db.ClusterOwner(r.Context(), deviceID)
	if err != nil || !ok || owner == c.id || addr == "" {
		return false
	}
	target, err := url.Parse(addr)
	if err != nil {
		log.Printf("[cluster] owner %s has a bad address %q: %v", owner, addr, err)
		return false
	}
	p := httputil.NewSingleHostReverseProxy(target)
	p.FlushInterval = -1
	base := p.Director
	p.Director = func(req *http.Request) {
		base(req)
		req.Header.Set(ProxiedHeader, c.id)
		req.Host = r.Host
	}
	c.proxied.Add(1)
	p.ServeHTTP(w, r)
	return true
}
