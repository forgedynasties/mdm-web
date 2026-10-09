// Package scout drives "a T7 enrols the other devices in its restaurant": a firmware
// device sweeps its venue's Wi-Fi on :5555 with the fleet adb key (net_scan), reports
// each device that answers (net_sighting), and on an operator's approval installs the
// standard client on one and makes it Device Owner (net_enroll). The firmware-client half
// is NetScout.java / AdbClient.java; the sightings table and schema are in internal/db.
package scout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/fleetkey"
	prod "mdm/internal/product"
	"mdm/internal/ws"
)

// scanInterval is how often an open venue is swept; the scheduler runs this often and
// each run scans every eligible venue once. "Scan now" from the dashboard is immediate.
const scanInterval = 15 * time.Minute

// enrollTokenTTL bounds the one-shot token a scout is handed for one device.
const enrollTokenTTL = 10 * time.Minute

// staleAfter removes a ready/blocked/failed sighting not seen for this long.
const staleAfter = 7 * 24 * time.Hour

// scanTTL is how long a scan may be considered in flight before a venue is allowed
// another one. A sweep of 254 hosts with a second patient pass takes well under this;
// the ceiling only matters when a scout goes away mid-scan and never answers.
const scanTTL = 3 * time.Minute

// Service holds the shared dependencies. One instance, started with Run.
type Service struct {
	db  *db.DB
	hub *ws.Hub
	cfg *config.Config

	// One scan per venue at a time. Several venues can share one physical network —
	// two restaurants in a building, or the lab — and each picks its own scout, so
	// without this the same /24 is swept twice over and every host is probed twice.
	// The client refuses a second concurrent scan too; this stops the frame being sent
	// at all, and gives the dashboard something to say.
	mu       sync.Mutex
	scanning map[uuid.UUID]time.Time
}

func New(database *db.DB, hub *ws.Hub, cfg *config.Config) *Service {
	return &Service{db: database, hub: hub, cfg: cfg, scanning: map[uuid.UUID]time.Time{}}
}

// claimScan marks a venue as scanning, or reports how long the running scan has been
// going so the caller can refuse.
func (s *Service) claimScan(restaurantID uuid.UUID) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at, ok := s.scanning[restaurantID]; ok {
		if since := time.Since(at); since < scanTTL {
			return since, false
		}
	}
	s.scanning[restaurantID] = time.Now()
	return 0, true
}

func (s *Service) releaseScan(restaurantID uuid.UUID) {
	s.mu.Lock()
	delete(s.scanning, restaurantID)
	s.mu.Unlock()
}

// ScanDone frees a venue when its scout reports the sweep finished (net_scan_done), so
// the next scan does not have to wait out scanTTL.
func (s *Service) ScanDone(ctx context.Context, scoutID uuid.UUID) {
	_, restaurantID, err := s.db.DeviceScoutVenue(ctx, scoutID)
	if err != nil || restaurantID == nil {
		return
	}
	s.releaseScan(*restaurantID)
}

// fleetKeyPEM unseals the default adb private key. Returns an error the caller can show
// when no key is set (the feature is inert until one is uploaded in Settings).
func (s *Service) fleetKeyPEM(ctx context.Context) (string, error) {
	keys, err := s.fleetKeys(ctx)
	if err != nil {
		return "", err
	}
	return keys[0].PEM, nil
}

// labelledKey is one key as it rides in a frame: the scout tries each and tells us which
// one the device took.
type labelledKey struct {
	Label string `json:"label"`
	PEM   string `json:"key_pem"`
}

// fleetKeys unseals every adb key, default first. A device only trusts the key that was in
// its own image, so the scout is given all of them rather than one.
func (s *Service) fleetKeys(ctx context.Context) ([]labelledKey, error) {
	if s.cfg.FleetAdbKeySecret() == "" {
		return nil, errors.New("FLEET_ADB_KEY_SECRET is not set")
	}
	stored, err := s.db.ListFleetAdbKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]labelledKey, 0, len(stored))
	for _, k := range stored {
		pem, err := fleetkey.Open(s.cfg.FleetAdbKeySecret(), k.PrivateSealed)
		if err != nil {
			log.Printf("[scout] key %s will not unseal: %v", k.Label, err)
			continue // one bad key must not cost the scout the others
		}
		out = append(out, labelledKey{Label: k.Label, PEM: pem})
	}
	if len(out) == 0 {
		return nil, errors.New("no fleet adb key uploaded")
	}
	return out, nil
}

// withKeys puts the keys in a frame: the list for a scout that understands labels, and the
// default key alone under the old name for one that does not.
func withKeys(frame map[string]any, keys []labelledKey) map[string]any {
	frame["keys"] = keys
	frame["key_pem"] = keys[0].PEM
	return frame
}

// pickScout returns the connected firmware device that should scout a restaurant, or an
// error when none is online. Most-recently-seen first; the first one the hub has a live
// socket to wins.
func (s *Service) pickScout(ctx context.Context, restaurantID uuid.UUID) (db.ScoutCandidate, error) {
	cands, err := s.db.ScoutCandidates(ctx, restaurantID)
	if err != nil {
		return db.ScoutCandidate{}, err
	}
	for _, c := range cands {
		if s.hub.IsConnected(c.ID) {
			return c, nil
		}
	}
	return db.ScoutCandidate{}, errors.New("no Tableside AI online at this venue")
}

// StartScan sends a scout a net_scan for its whole venue, or (host set) a re-probe of one
// device. The key rides in the frame and is used only for this scan.
func (s *Service) StartScan(ctx context.Context, restaurantID uuid.UUID, host string) error {
	keys, err := s.fleetKeys(ctx)
	if err != nil {
		return err
	}
	c, err := s.pickScout(ctx, restaurantID)
	if err != nil {
		return err
	}
	if since, ok := s.claimScan(restaurantID); !ok {
		return fmt.Errorf("a scan of this venue started %s ago — wait for it to finish", since.Round(time.Second))
	}
	frame := withKeys(map[string]any{
		"type":    "net_scan",
		"session": newID(),
		"ttl_s":   600,
	}, keys)
	if host != "" {
		frame["host"] = host
	}
	raw, _ := json.Marshal(frame)
	if !s.hub.Push(c.ID, raw) {
		s.releaseScan(restaurantID)
		return errors.New("scout went offline")
	}
	log.Printf("[scout] net_scan -> %s (restaurant %s, host %q)", c.Serial, restaurantID, host)
	return nil
}

// Approve turns one sighting into an enrolment: mints a one-shot profile token placed at
// the sighting's venue, marks the row enrolling, and sends the scout a net_enroll. serverURL
// and apkURL are absolute (the caller builds them from the request); apkSHA256 may be ""
// for an externally hosted APK whose file digest we don't hold.
func (s *Service) Approve(ctx context.Context, sightingID uuid.UUID, class, approver, serverURL, apkURL, apkSHA256 string) error {
	if !prod.IsClass(class) {
		return errors.New("unknown device class")
	}
	sg, err := s.db.GetSighting(ctx, sightingID)
	if err != nil {
		return err
	}
	if sg.State != "ready" && sg.State != "failed" {
		return errors.New("this device is not ready to enrol")
	}
	keys, err := s.fleetKeys(ctx)
	if err != nil {
		return err
	}
	c, err := s.pickScout(ctx, sg.RestaurantID)
	if err != nil {
		return err
	}

	token, err := s.mintToken(ctx, *sg, class, approver)
	if err != nil {
		return err
	}
	job := newID()
	ok, err := s.db.MarkSightingEnrolling(ctx, sightingID, job, approver)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("this device is already being enrolled")
	}
	frame := withKeys(map[string]any{
		"type":       "net_enroll",
		"job":        job,
		"host":       sg.Host,
		"port":       sg.Port,
		"serial":     sg.Serial,
		"class":      class,
		"token":      token,
		"apk_url":    apkURL,
		"apk_sha256": apkSHA256,
		"server_url": serverURL,
		// The key this device took when it was seen: the scout tries it first.
		"key_label": sg.KeyLabel,
	}, keys)
	raw, _ := json.Marshal(frame)
	if !s.hub.Push(c.ID, raw) {
		_ = s.db.FinishSighting(ctx, job, false, "scout went offline")
		return errors.New("scout went offline")
	}
	_ = s.db.InsertAudit(ctx, approver, "device.enroll", sg.Serial,
		"via "+c.Serial+" at "+sg.RestaurantName+" as "+prod.ClassLabel(class))
	log.Printf("[scout] net_enroll job=%s %s via %s class=%s", job, sg.Serial, c.Serial, class)
	return nil
}

// mintToken creates the one-shot enrollment profile this device enrols with: placed at the
// sighting's venue so it skips the inbox, one enrol, short-lived.
func (s *Service) mintToken(ctx context.Context, sg db.Sighting, class, approver string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	one := 1
	exp := time.Now().Add(enrollTokenTTL)
	rid := sg.RestaurantID
	in := db.EnrollmentProfileInput{
		Name:         "scout · " + sg.RestaurantName + " · " + sg.Serial,
		Token:        "enr_" + hex.EncodeToString(raw),
		DeviceClass:  class,
		RestaurantID: &rid,
		ExpiresAt:    &exp,
		MaxEnrolls:   &one,
		Notes:        "One-shot, minted for scout enrolment via " + sg.ScoutSerial + " by " + approver,
		CreatedBy:    approver,
	}
	if _, err := s.db.CreateEnrollmentProfile(ctx, in); err != nil {
		return "", err
	}
	return in.Token, nil
}

// ── frame ingest (called from the device WS router in main.go) ───────────────────────

type sightingFrame struct {
	Serial       string `json:"serial"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Android      string `json:"android"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Auth         string `json:"auth"`   // "refused" when adb answered but would not take the key
	Reason       string `json:"reason"` // what adb said when it refused
	Owner        struct {
		Set  bool   `json:"set"`
		Ours bool   `json:"ours"`
		Pkg  string `json:"pkg"`
	} `json:"owner"`
	Accounts        int    `json:"accounts"`
	Users           int    `json:"users"`
	DPCVersion      string `json:"dpc_version"`
	FirmwareVersion string `json:"firmware_version"`
	KeyLabel        string `json:"key_label"`
}

// IngestSighting records one net_sighting. deviceID is the scout's; its restaurant is the
// venue the sighting belongs to. A scout with no restaurant (a bench unit) is ignored.
func (s *Service) IngestSighting(ctx context.Context, scoutID uuid.UUID, raw []byte) {
	scoutSerial, restaurantID, err := s.db.DeviceScoutVenue(ctx, scoutID)
	if err != nil {
		log.Printf("[scout] sighting from %s: %v", scoutID, err)
		return
	}
	if restaurantID == nil {
		log.Printf("[scout] sighting from %s ignored: scout is not placed at a venue", scoutSerial)
		return
	}
	var f sightingFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("[scout] sighting from %s: bad frame: %v", scoutSerial, err)
		return
	}
	// A host that speaks adb but refused the key has no serial to report — nothing can
	// be read from it without auth. It is still listed, keyed on its address, because a
	// person at the device can tap Allow and then it enrols like any other.
	if f.Auth == "refused" {
		if f.Host == "" {
			return
		}
		why := strings.TrimSpace(f.Reason)
		if why == "" {
			why = "did not accept the fleet adb key"
		}
		if err := s.db.UpsertUnauthorized(ctx, *restaurantID, scoutSerial, f.Host, orDefault(f.Port, 5555),
			"Has not accepted the fleet adb key — tap Allow on the device, or reflash it with the key"); err != nil {
			log.Printf("[scout] upsert unauthorized %s: %v", f.Host, err)
			return
		}
		log.Printf("[scout] unauthorized host %s via %s: %s", f.Host, scoutSerial, why)
		return
	}
	if f.Serial == "" {
		log.Printf("[scout] sighting from %s at %s: no serial in the probe, dropped", scoutSerial, f.Host)
		return
	}
	if f.KeyLabel != "" && f.Serial != "" {
		// The same record the Enroll apps write: which key this device takes, by serial.
		if err := s.db.PutDeviceAdbKey(ctx, f.Serial, f.KeyLabel, "scout", scoutSerial); err != nil {
			log.Printf("[scout] key label for %s: %v", f.Serial, err)
		}
	}
	state, reason := classifySighting(f)
	up := db.SightingUpsert{
		RestaurantID:    *restaurantID,
		ScoutSerial:     scoutSerial,
		Host:            f.Host,
		Port:            orDefault(f.Port, 5555),
		Serial:          f.Serial,
		Manufacturer:    f.Manufacturer,
		Model:           f.Model,
		Android:         f.Android,
		OwnerPkg:        f.Owner.Pkg,
		OwnerOurs:       f.Owner.Ours,
		Accounts:        f.Accounts,
		Users:           max1(f.Users),
		DPCVersion:      f.DPCVersion,
		FirmwareVersion: f.FirmwareVersion,
		ClassGuess:      prod.ClassForModel("", f.Manufacturer, f.Model),
		KeyLabel:        f.KeyLabel,
		State:           state,
		Reason:          reason,
	}
	// Our own firmware (it carries a firmware version) enrols itself — nothing to do here.
	if f.FirmwareVersion != "" {
		up.State = "ours"
		up.Reason = ""
	}
	// Nor is a device this server already manages, however it got here: the desktop app
	// enrols over USB, and the probe of such a device looks exactly like a fresh one
	// that happens to have our client on it.
	if known, err := s.db.DeviceKnown(ctx, f.Serial); err == nil && known {
		up.State = "ours"
		up.Reason = ""
	}
	if err := s.db.UpsertSighting(ctx, up); err != nil {
		log.Printf("[scout] upsert sighting %s: %v", f.Serial, err)
		return
	}
	log.Printf("[scout] sighting %s (%s %s) at %s via %s: %s %s",
		f.Serial, f.Manufacturer, f.Model, f.Host, scoutSerial, up.State, up.Reason)
}

// classifySighting maps a probe to ready/blocked. Mirrors enroll-adb.sh's blockers.
func classifySighting(f sightingFrame) (state, reason string) {
	ours := f.Owner.Ours
	switch {
	case f.Owner.Set && !ours:
		return "blocked", "Owned by " + f.Owner.Pkg + " — factory reset this device first"
	case f.Accounts > 0 && !ours:
		return "blocked", "Has an account — factory reset, and don't add an account"
	case f.Users > 1 && !ours:
		return "blocked", "Has a second user — remove it first"
	default:
		return "ready", ""
	}
}

type progressFrame struct {
	Job  string `json:"job"`
	Step int    `json:"step"`
}

func (s *Service) IngestProgress(ctx context.Context, raw []byte) {
	var f progressFrame
	if err := json.Unmarshal(raw, &f); err != nil || f.Job == "" {
		return
	}
	_ = s.db.SetSightingStep(ctx, f.Job, f.Step)
}

type doneFrame struct {
	Job      string `json:"job"`
	OK       bool   `json:"ok"`
	Serial   string `json:"serial"`
	Reason   string `json:"reason"`
	KeyLabel string `json:"key_label"`
}

func (s *Service) IngestEnrollDone(ctx context.Context, scoutID uuid.UUID, raw []byte) {
	var f doneFrame
	if err := json.Unmarshal(raw, &f); err != nil || f.Job == "" {
		return
	}
	if err := s.db.FinishSighting(ctx, f.Job, f.OK, f.Reason); err != nil {
		log.Printf("[scout] finish job %s: %v", f.Job, err)
	}
	if f.OK && f.Serial != "" {
		if scoutSerial, _, err := s.db.DeviceScoutVenue(ctx, scoutID); err == nil {
			_ = s.db.SetEnrolledViaSerial(ctx, f.Serial, scoutSerial)
			if f.KeyLabel != "" {
				// The enrolment itself proves which key this device's image carries — better
				// evidence than the scan, which may have been a different device at that address.
				if err := s.db.PutDeviceAdbKey(ctx, f.Serial, f.KeyLabel, "scout", scoutSerial); err != nil {
					log.Printf("[scout] key label for %s: %v", f.Serial, err)
				}
			}
		}
	}
}

// ── scheduler ────────────────────────────────────────────────────────────────────────

// Run sweeps every eligible venue once per scanInterval and prunes stale sightings. Stops
// when ctx is done. It does not auto-enrol: that (phase 3) is gated and not built yet.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(scanInterval)
	defer t.Stop()
	s.sweepAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepAll(ctx)
		}
	}
}

func (s *Service) sweepAll(ctx context.Context) {
	if _, err := s.fleetKeyPEM(ctx); err != nil {
		return // no key: the feature is inert, don't spam scouts
	}
	ids, err := s.db.RestaurantsToScan(ctx)
	if err != nil {
		log.Printf("[scout] list venues: %v", err)
		return
	}
	for _, id := range ids {
		if err := s.StartScan(ctx, id, ""); err != nil {
			// "no scout online" is normal and quiet; anything else is worth a line.
			if err.Error() != "no Tableside AI online at this venue" {
				log.Printf("[scout] scan venue %s: %v", id, err)
			}
		}
	}
	if n, err := s.db.PruneStaleSightings(ctx, staleAfter); err == nil && n > 0 {
		log.Printf("[scout] pruned %d stale sighting(s)", n)
	}
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}
