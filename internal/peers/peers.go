// Package peers keeps two or more MDM servers honest with each other about which of
// them a device is actually reporting to.
//
// The problem it solves: a device is pointed at a server by persist.sys.mdm.url. That
// property can be changed from the device page, by hand over adb, or by flashing an
// image whose build.prop already carries it — and only the first of those passes
// through the server the device is leaving. To that server, all three look identical
// to a device that died: it goes quiet, and stays quiet.
//
// Silence cannot be told apart from departure by looking harder at silence. It can only
// be resolved by asking somewhere the device might have gone. So:
//
//   - Push: whoever a device arrives at announces it to its peers (durable outbox,
//     retried, so a peer being down loses nothing).
//   - Pull: every server periodically asks its peers about the devices it has not
//     heard from. This is the part that makes the system self-correcting — it catches
//     announcements that were never sent (a peer added later, a device moved before
//     any of this existed, hardware flashed in the factory), so state converges
//     whether or not any single message got through.
//
// A peer is trusted for exactly one thing: reporting when it last saw a serial. It
// cannot create a device, change a status, or issue a command. That is why the peer key
// is not the admin key.
package peers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
)

// Store is the slice of the database this package needs.
type Store interface {
	EnqueuePeerAnnounce(ctx context.Context, peer, serial string, payload json.RawMessage) error
	ClaimPeerOutbox(ctx context.Context, limit int) ([]db.PeerOutboxItem, error)
	DeletePeerOutbox(ctx context.Context, id int64) error
	FailPeerOutbox(ctx context.Context, id int64, attempts int, reason string) error
	DropPeerOutboxFor(ctx context.Context, keep []string) error
	DevicesToReconcile(ctx context.Context, quietFor, recheckAfter time.Duration, limit int) ([]db.CustodyCandidate, error)
	SetDeviceCustody(ctx context.Context, deviceID uuid.UUID, server, url string, seenAt time.Time, source string) (bool, error)
	CancelPendingForCustody(ctx context.Context, deviceID uuid.UUID) (int64, error)
	ClearDeviceCustody(ctx context.Context, deviceID uuid.UUID) error
	DeviceIDBySerial(ctx context.Context, serial string) (uuid.UUID, time.Time, bool, error)
	ExpireStaleCustody(ctx context.Context) (int64, error)
}

// Service is the peer client half: it sends announcements and runs the sweep. The
// server half (the endpoints peers call) lives in internal/api.
type Service struct {
	db   Store
	cfg  *config.Config
	http *http.Client
}

func New(store Store, cfg *config.Config) *Service {
	return &Service{db: store, cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}}
}

// Arrival is what one server tells another: I have this device, and this is when I last
// heard from it. Nothing about the device's configuration travels — a peer is told
// where a serial is, not given a copy of the fleet.
type Arrival struct {
	Serial  string    `json:"serial"`
	SeenAt  time.Time `json:"seen_at"`
	BuildID string    `json:"build_id,omitempty"`
	Product string    `json:"product,omitempty"`
	// From names the announcing server. Its URL is deliberately NOT carried: the
	// receiver already knows where its peers live, having authenticated this call
	// against one of their keys. A peer cannot talk another server into pointing
	// operators at a URL of its choosing.
	From   string    `json:"from"`
	SentAt time.Time `json:"sent_at"` // for clock-skew checks at the far end
}

// AnnounceArrival queues "this device is here" for every configured peer. It never
// blocks the check-in that triggered it: the work is a row per peer in the outbox.
func (s *Service) AnnounceArrival(ctx context.Context, serial, buildID, product string) {
	peers := s.cfg.Peers(true)
	if len(peers) == 0 || strings.TrimSpace(serial) == "" {
		return
	}
	now := time.Now().UTC()
	payload, err := json.Marshal(Arrival{
		Serial: serial, SeenAt: now, BuildID: buildID, Product: product,
		From: s.cfg.SelfName(), SentAt: now,
	})
	if err != nil {
		return
	}
	for _, p := range peers {
		if err := s.db.EnqueuePeerAnnounce(ctx, p.Name, serial, payload); err != nil {
			log.Printf("[peers] queue announce %s -> %s: %v", serial, p.Name, err)
		}
	}
}

// RunOutbox delivers queued announcements until ctx ends. Failures are left in the
// outbox with a backoff; nothing is lost by a peer being down, and nothing is retried
// forever either — the sweep asks directly, so an announcement is an optimisation.
func (s *Service) RunOutbox(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.drainOutbox(ctx)
		}
	}
}

func (s *Service) drainOutbox(ctx context.Context) {
	peers := map[string]config.Peer{}
	names := []string{}
	for _, p := range s.cfg.Peers(true) {
		peers[p.Name] = p
		names = append(names, p.Name)
	}
	if len(peers) == 0 {
		// Nothing configured: drop anything queued for a peer that no longer exists,
		// so a removed peer cannot leave rows retrying into the void.
		_ = s.db.DropPeerOutboxFor(ctx, names)
		return
	}
	items, err := s.db.ClaimPeerOutbox(ctx, 50)
	if err != nil {
		log.Printf("[peers] claim outbox: %v", err)
		return
	}
	for _, it := range items {
		p, ok := peers[it.Peer]
		if !ok {
			_ = s.db.DeletePeerOutbox(ctx, it.ID)
			continue
		}
		if err := s.post(ctx, p, "/api/v1/peer/device-seen", it.Payload, nil); err != nil {
			_ = s.db.FailPeerOutbox(ctx, it.ID, it.Attempts, err.Error())
			continue
		}
		_ = s.db.DeletePeerOutbox(ctx, it.ID)
	}
}

// lookupRequest asks a peer about a batch of serials.
type lookupRequest struct {
	Serials []string  `json:"serials"`
	From    string    `json:"from"`
	SentAt  time.Time `json:"sent_at"`
}

// LookupResult is one serial the peer knows about.
type LookupResult struct {
	Serial  string    `json:"serial"`
	SeenAt  time.Time `json:"seen_at"`
	BuildID string    `json:"build_id,omitempty"`
}

type lookupResponse struct {
	Server  string         `json:"server"`
	Now     time.Time      `json:"now"`
	Devices []LookupResult `json:"devices"`
}

// RunSweep is the self-correcting half. Every interval it takes the devices this server
// has not heard from, asks each peer whether they have, and files custody where the
// peer's sighting is newer than ours. It also drops claims nobody has refreshed, so a
// device that dies on the far server stops hiding behind "it's on stage".
func (s *Service) RunSweep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Sweep(ctx); err != nil {
				log.Printf("[peers] sweep: %v", err)
			} else if n > 0 {
				log.Printf("[peers] sweep filed %d device(s) as reporting elsewhere", n)
			}
			if n, err := s.db.ExpireStaleCustody(ctx); err != nil {
				log.Printf("[peers] expire custody: %v", err)
			} else if n > 0 {
				log.Printf("[peers] %d custody claim(s) went stale and were dropped", n)
			}
		}
	}
}

// QuietBefore is how long a device must be silent before the sweep asks about it. Well
// past any normal check-in interval, so a healthy fleet is never in the batch.
const QuietBefore = 30 * time.Minute

// RecheckAfter is how old a custody sighting may get before the sweep re-tests it.
const RecheckAfter = 6 * time.Hour

// Sweep runs one reconciliation pass and returns how many devices it filed.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	peers := s.cfg.Peers(true)
	if len(peers) == 0 {
		return 0, nil
	}
	cands, err := s.db.DevicesToReconcile(ctx, QuietBefore, RecheckAfter, 500)
	if err != nil || len(cands) == 0 {
		return 0, err
	}
	bySerial := make(map[string]db.CustodyCandidate, len(cands))
	serials := make([]string, 0, len(cands))
	for _, c := range cands {
		bySerial[c.Serial] = c
		serials = append(serials, c.Serial)
	}
	filed := 0
	for _, p := range peers {
		for start := 0; start < len(serials); start += 200 {
			end := start + 200
			if end > len(serials) {
				end = len(serials)
			}
			req, _ := json.Marshal(lookupRequest{Serials: serials[start:end], From: s.cfg.SelfName(), SentAt: time.Now().UTC()})
			var resp lookupResponse
			if err := s.post(ctx, p, "/api/v1/peer/lookup", req, &resp); err != nil {
				log.Printf("[peers] lookup %s: %v", p.Name, err)
				break // this peer is unreachable; the next sweep tries again
			}
			for _, d := range resp.Devices {
				c, ok := bySerial[d.Serial]
				if !ok || d.SeenAt.IsZero() || !d.SeenAt.After(c.LastSeen) {
					continue // we heard from it more recently than they did
				}
				ok2, err := s.db.SetDeviceCustody(ctx, c.ID, p.Name, peerDashboard(p), d.SeenAt, "sweep")
				if err != nil {
					log.Printf("[peers] set custody %s: %v", d.Serial, err)
					continue
				}
				if ok2 {
					filed++
					// Whatever was queued for it here is undeliverable now.
					if n, err := s.db.CancelPendingForCustody(ctx, c.ID); err == nil && n > 0 {
						log.Printf("[peers] dropped %d queued command target(s) for %s", n, d.Serial)
					}
				}
			}
		}
	}
	return filed, nil
}

// peerDashboard is where an operator should be sent to see the device: the peer's
// dashboard when it has one, else its API URL.
func peerDashboard(p config.Peer) string {
	if strings.TrimSpace(p.DashboardURL) != "" {
		return strings.TrimRight(p.DashboardURL, "/")
	}
	return strings.TrimRight(p.URL, "/")
}

// Ping checks a peer answers and names itself — what the Settings "test" button calls.
func (s *Service) Ping(ctx context.Context, p config.Peer) (string, error) {
	var out struct {
		Server string `json:"server"`
	}
	req, _ := json.Marshal(map[string]any{"from": s.cfg.SelfName(), "sent_at": time.Now().UTC()})
	if err := s.post(ctx, p, "/api/v1/peer/ping", req, &out); err != nil {
		return "", err
	}
	return out.Server, nil
}

func (s *Service) post(ctx context.Context, p config.Peer, path string, body []byte, out any) error {
	base := strings.TrimRight(strings.TrimSpace(p.URL), "/")
	if base == "" {
		return fmt.Errorf("peer %s has no URL", p.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Peer-Key", p.Key)
	req.Header.Set("X-Peer-Name", s.cfg.SelfName())
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
