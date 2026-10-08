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
	"log"
	"strings"
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

// Service holds the shared dependencies. One instance, started with Run.
type Service struct {
	db  *db.DB
	hub *ws.Hub
	cfg *config.Config
}

func New(database *db.DB, hub *ws.Hub, cfg *config.Config) *Service {
	return &Service{db: database, hub: hub, cfg: cfg}
}

// fleetKeyPEM unseals the fleet adb private key. Returns an error the caller can show when
// no key is set (the feature is inert until one is uploaded in Settings).
func (s *Service) fleetKeyPEM(ctx context.Context) (string, error) {
	if s.cfg.FleetAdbKeySecret() == "" {
		return "", errors.New("FLEET_ADB_KEY_SECRET is not set")
	}
	k, err := s.db.GetFleetAdbKey(ctx)
	if err != nil {
		return "", err
	}
	if k == nil {
		return "", errors.New("no fleet adb key uploaded")
	}
	return fleetkey.Open(s.cfg.FleetAdbKeySecret(), k.PrivateSealed)
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
	key, err := s.fleetKeyPEM(ctx)
	if err != nil {
		return err
	}
	c, err := s.pickScout(ctx, restaurantID)
	if err != nil {
		return err
	}
	frame := map[string]any{
		"type":    "net_scan",
		"session": newID(),
		"key_pem": key,
		"ttl_s":   600,
	}
	if host != "" {
		frame["host"] = host
	}
	raw, _ := json.Marshal(frame)
	if !s.hub.Push(c.ID, raw) {
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
	key, err := s.fleetKeyPEM(ctx)
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
	frame := map[string]any{
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
		"key_pem":    key,
	}
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
	Job    string `json:"job"`
	OK     bool   `json:"ok"`
	Serial string `json:"serial"`
	Reason string `json:"reason"`
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
