package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Device moves to prod (16 Nov 2026; plan "Moving devices to prod"; see db/device_move.go).
//
// On mdm.dev (the server the fleet is on):
//
//	GET  /api/v1/admin/move/devices/{serial}         the device's bundle
//	POST /api/v1/admin/move/devices/{serial}[?now=1] import it on prod now, then move it when it can
//	POST /api/v1/admin/move/devices/{serial}?import_only=1   import it on prod only (no move filed)
//	                                                 (now=1: a firmware device at an open venue too)
//	POST /api/v1/admin/move/venues/{id}[?only=dpc|firmware]   the same for every device at a venue
//	POST /api/v1/admin/move/devices/{serial}/cancel  a move not under way
//	GET  /api/v1/admin/move/status[?state=&venue=]   every move and where it is
//
// On prod: POST /api/v1/admin/move/import takes a bundle (only where the server is prod).
//
// A move stays pending until the device is online (and, for a firmware device at a venue,
// the venue is closed: it reboots into the move). Just before it goes, the device is
// imported again, so prod has what mdm.dev has at that moment. mdm.dev then watches prod
// for the device's first check-in there.

// moveSourceHost is the server devices move away from; moveTargetURL where they go. The
// two share the admin key, so mdm.dev calls prod with its own; nothing goes in .env.
const (
	moveSourceHost = "mdm.dev.aioapp.com"
	moveTargetURL  = "https://" + fleetResetHost
)

// deviceMoveTarget is where this server moves devices, "" when it moves none. Elsewhere
// (stage, tests) MOVE_TARGET_URL with ALLOW_DEVICE_MOVE=1 names one.
var deviceMoveTarget = func() string {
	if ownHost() == moveSourceHost {
		return moveTargetURL
	}
	if os.Getenv("ALLOW_DEVICE_MOVE") == "1" {
		return strings.TrimRight(os.Getenv("MOVE_TARGET_URL"), "/")
	}
	return ""
}

// takesDeviceMoves: only prod imports devices (the fleet reset's rule).
func takesDeviceMoves() bool {
	return os.Getenv("ALLOW_FLEET_RESET") == "1" || ownHost() == fleetResetHost
}

// moveSourceName names this server in prod's custody of a waiting device.
func moveSourceName() string {
	switch h := ownHost(); h {
	case moveSourceHost:
		return "mdm.dev"
	case "":
		return "the old server"
	default:
		return h
	}
}

func thisOrigin() string {
	return strings.TrimRight(strings.TrimSpace(strings.Split(os.Getenv("PUBLIC_ORIGIN"), ",")[0]), "/")
}

var moveHTTP = &http.Client{Timeout: 30 * time.Second}

// moveCall calls the target's admin API with this server's admin key.
func (h *Handler) moveCall(ctx context.Context, method, target, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", h.adminAPIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := moveHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s answered %d: %s", target, resp.StatusCode, e.Error)
		}
		return fmt.Errorf("%s answered %d", target, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// importOnTarget sends a device's bundle to the target and returns its answer.
func (h *Handler) importOnTarget(ctx context.Context, target, serial string) (*db.DeviceMoveImport, error) {
	b, err := h.db.ExportDeviceMove(ctx, serial, thisOrigin())
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("no device %s here", serial)
	}
	var res db.DeviceMoveImport
	if err := h.moveCall(ctx, http.MethodPost, target, "/api/v1/admin/move/import", b, &res); err != nil {
		return nil, err
	}
	if res.Kind != "firmware" && res.EnrollToken == "" {
		return nil, fmt.Errorf("%s gave no enrollment token for a Standard device", target)
	}
	return &res, nil
}

// MoveExport is GET /api/v1/admin/move/devices/{serial}.
func (h *Handler) MoveExport(w http.ResponseWriter, r *http.Request) {
	b, err := h.db.ExportDeviceMove(r.Context(), r.PathValue("serial"), thisOrigin())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if b == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such device"})
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// MoveImport is POST /api/v1/admin/move/import (prod only).
func (h *Handler) MoveImport(w http.ResponseWriter, r *http.Request) {
	if !takesDeviceMoves() {
		http.NotFound(w, r)
		return
	}
	var b db.DeviceMoveBundle
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Serial == "" || len(b.Device) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send a bundle from GET /api/v1/admin/move/devices/{serial}"})
		return
	}
	from := strings.TrimRight(b.From, "/")
	if from == "" || strings.EqualFold(from, thisOrigin()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the bundle must come from another server"})
		return
	}
	name := from
	if i := strings.Index(name, "://"); i >= 0 {
		name = name[i+3:]
	}
	if name == moveSourceHost {
		name = "mdm.dev"
	}
	res, err := h.db.ImportDeviceMove(r.Context(), &b, name, from, newEnrollToken)
	var missing db.ErrMoveVenueMissing
	switch {
	case errors.As(err, &missing):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	case err != nil:
		log.Printf("[move] import %s: %v", b.Serial, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	verb := "updated"
	if res.Created {
		verb = "created"
	}
	_ = h.db.InsertAudit(r.Context(), "admin API", "device.move_import", b.Serial, verb+", waiting for it to arrive from "+from)
	writeJSON(w, http.StatusOK, res)
}

func newEnrollToken() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return "enr_" + hex.EncodeToString(raw)
}

type moveResult struct {
	Serial string `json:"serial"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
}

// startMove imports one device on the target and files its move as pending.
func (h *Handler) startMove(ctx context.Context, target, serial, by string) moveResult {
	if m, err := h.db.GetDeviceMove(ctx, serial); err == nil && m != nil && (m.State == db.MoveSent || m.State == db.MoveArrived) {
		return moveResult{Serial: serial, State: m.State, Error: "already " + m.State}
	}
	res, err := h.importOnTarget(ctx, target, serial)
	if err != nil {
		return moveResult{Serial: serial, Error: err.Error()}
	}
	ok, err := h.db.PutDeviceMove(ctx, serial, target, res.EnrollToken, by)
	if err != nil {
		return moveResult{Serial: serial, Error: err.Error()}
	}
	if !ok {
		return moveResult{Serial: serial, Error: "a move is already under way"}
	}
	_ = h.db.InsertAudit(ctx, by, "device.move_to", serial, target)
	return moveResult{Serial: serial, State: db.MovePending}
}

// MoveDevice is POST /api/v1/admin/move/devices/{serial}.
func (h *Handler) MoveDevice(w http.ResponseWriter, r *http.Request) {
	target := deviceMoveTarget()
	if target == "" {
		http.NotFound(w, r)
		return
	}
	serial := r.PathValue("serial")
	if dev, err := h.db.GetDevice(r.Context(), serial); err != nil || dev == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	if r.URL.Query().Get("import_only") == "1" {
		// Import only (19 Nov 2026): the device appears on prod in its venue, marked as still
		// reporting here (custody "migrate": no offline alerts, no commands), and no move is
		// filed. For a client too old to move itself, or a box that has been dark for weeks, so
		// prod's venue lists every device it will have. A later real move imports it again.
		if _, err := h.importOnTarget(r.Context(), target, serial); err != nil {
			writeJSON(w, http.StatusConflict, moveResult{Serial: serial, Error: err.Error()})
			return
		}
		_ = h.db.InsertAudit(r.Context(), "admin API", "device.import_to", serial, target)
		writeJSON(w, http.StatusOK, moveResult{Serial: serial, State: "imported"})
		return
	}
	res := h.startMove(r.Context(), target, serial, "admin API")
	status := http.StatusOK
	if res.Error != "" {
		status = http.StatusConflict
	} else if r.URL.Query().Get("now") == "1" {
		// Move it now: no waiting for the venue to close (a firmware device reboots).
		if ok, err := h.db.MoveNow(r.Context(), serial); err == nil && ok {
			_ = h.db.InsertAudit(r.Context(), "admin API", "device.move_now", serial, target)
			res.State = db.MovePending + " · now"
		}
	}
	writeJSON(w, status, res)
}

// MoveVenue is POST /api/v1/admin/move/venues/{id}: every device at the venue, or only
// its Standard (only=dpc) or firmware (only=firmware) devices.
func (h *Handler) MoveVenue(w http.ResponseWriter, r *http.Request) {
	target := deviceMoveTarget()
	if target == "" {
		http.NotFound(w, r)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid venue id"})
		return
	}
	only := r.URL.Query().Get("only")
	if only != "" && only != "dpc" && only != "firmware" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only is dpc or firmware"})
		return
	}
	ctx := r.Context()
	serials, err := h.db.VenueDeviceSerials(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var out []moveResult
	for _, s := range serials {
		if only != "" {
			d, err := h.db.GetDevice(ctx, s)
			if err != nil || d == nil || (only == "dpc") != d.IsDPC() {
				continue
			}
		}
		out = append(out, h.startMove(ctx, target, s, "admin API"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"venue": id, "devices": out})
}

// MoveCancel is POST /api/v1/admin/move/devices/{serial}/cancel.
func (h *Handler) MoveCancel(w http.ResponseWriter, r *http.Request) {
	if deviceMoveTarget() == "" {
		http.NotFound(w, r)
		return
	}
	serial := r.PathValue("serial")
	ok, err := h.db.CancelDeviceMove(r.Context(), serial)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no move to cancel (none, or already sent)"})
		return
	}
	_ = h.db.InsertAudit(r.Context(), "admin API", "device.move_cancel", serial, "")
	writeJSON(w, http.StatusOK, map[string]any{"serial": serial, "state": db.MoveCanceled})
}

// MoveStatus is GET /api/v1/admin/move/status.
func (h *Handler) MoveStatus(w http.ResponseWriter, r *http.Request) {
	var states []string
	if s := r.URL.Query().Get("state"); s != "" {
		states = strings.Split(s, ",")
	}
	var venue *uuid.UUID
	if v := r.URL.Query().Get("venue"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid venue id"})
			return
		}
		venue = &id
	}
	moves, err := h.db.ListDeviceMoves(r.Context(), states, venue)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	counts := map[string]int{}
	for _, m := range moves {
		counts[m.State]++
	}
	if moves == nil {
		moves = []db.DeviceMove{}
	}
	// Online the way the job counts it; came_back and failed moves are what needs a person.
	var attention []string
	for i := range moves {
		m := &moves[i]
		m.Online = m.LastSeenAt != nil && time.Since(*m.LastSeenAt) <= moveOnlineWithin
		if m.State == db.MoveCameBack || m.State == db.MoveFailed {
			attention = append(attention, m.Serial)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": deviceMoveTarget(), "counts": counts, "attention": attention, "moves": moves})
}

// Move timing.
const (
	moveOnlineWithin = 10 * time.Minute // heard from this recently = online, the move can go
	moveCameBackGap  = 5 * time.Minute  // heard here this long after the move = it came back
	moveSendsPerTick = 10
	// A move whose command still says "running" this long after the device said it was enrolling on
	// the target, while the device keeps checking in here, did not happen: the device never finished
	// enrolling (23 Nov 2026: ADRB0AAMY00029 was called "came back" with its command still running).
	moveStallAfter = 10 * time.Minute
	// A failed or came-back move is tried again by itself, this many times in all, waiting
	// attempts x moveRetryEvery between tries. After that it stays for a person, with the reason.
	moveMaxAttempts = 3
	moveRetryEvery  = 30 * time.Minute
)

// RunDeviceMoves is the minute job on the server devices leave: it sends pending moves to
// devices that can take them now and follows sent ones until they arrive or come back.
func (h *Handler) RunDeviceMoves(ctx context.Context) {
	target := deviceMoveTarget()
	if target == "" {
		return
	}
	moves, err := h.db.ListDeviceMoves(ctx, []string{db.MovePending, db.MoveSent, db.MoveArrived, db.MoveCameBack, db.MoveFailed}, nil)
	if err != nil {
		log.Printf("[move] list: %v", err)
		return
	}
	sent := 0
	for _, m := range moves {
		if ctx.Err() != nil {
			return
		}
		switch m.State {
		case db.MovePending:
			if sent >= moveSendsPerTick {
				continue
			}
			if h.sendMove(ctx, m) {
				sent++
			}
		case db.MoveSent:
			h.followMove(ctx, m)
		case db.MoveArrived:
			if m.LastSeenAt != nil && m.ArrivedAt != nil && m.LastSeenAt.After(m.ArrivedAt.Add(moveCameBackGap)) {
				log.Printf("[move] %s checks in here again after arriving on %s", m.Serial, m.Target)
				_ = h.db.SetDeviceMoveState(ctx, m.Serial, db.MoveCameBack, "checked in here again after it arrived", nil)
				_ = h.db.InsertAudit(ctx, "device move", "device.move_came_back", m.Serial, "checked in here again after it arrived")
			}
		case db.MoveFailed:
			h.retryMove(ctx, m)
		case db.MoveCameBack:
			// Try again by itself when a few tries are left.
			if h.retryMove(ctx, m) {
				continue
			}
			// Taken for back too early (or back, then gone again): once the target has heard
			// from it more recently than this server has, it is there.
			if m.SentAt != nil && m.LastSeenAt != nil {
				if at := h.seenOnTarget(ctx, m); at != nil && at.After(*m.LastSeenAt) {
					h.markArrived(ctx, m, *at)
				}
			}
		}
	}
}

// moveWait says why a pending move can't go now, or "".
func (h *Handler) moveWait(ctx context.Context, m db.DeviceMove) string {
	if m.LastSeenAt == nil || time.Since(*m.LastSeenAt) > moveOnlineWithin {
		return "waiting for it to come online"
	}
	if m.Kind != "firmware" || m.VenueID == nil || m.SendNow {
		return ""
	}
	// A firmware device reboots into the move: at a venue, only while the venue is closed.
	w, ok, err := h.db.GetRestaurantServiceWindow(ctx, *m.VenueID)
	if err == nil && !ok {
		w, err = h.db.GetFleetServiceWindow(ctx)
	}
	if err != nil {
		return "waiting: could not read the venue's hours"
	}
	tz := ""
	if v, err := h.db.GetRestaurant(ctx, *m.VenueID); err == nil && v != nil {
		tz = v.Timezone
	}
	if w.InService(time.Now(), tz) {
		return "waiting for " + m.Venue + " to close (it reboots into the move)"
	}
	return ""
}

// sendMove imports the device again and sends it the move. Reports whether it went.
func (h *Handler) sendMove(ctx context.Context, m db.DeviceMove) bool {
	if why := h.moveWait(ctx, m); why != "" {
		_ = h.db.NoteDeviceMove(ctx, m.Serial, why)
		return false
	}
	// Online (and the venue closed, for firmware): now the MDM client must be current.
	if why := h.clientWait(ctx, m); why != "" {
		_ = h.db.NoteDeviceMove(ctx, m.Serial, why)
		return false
	}
	if why := h.sizeWait(ctx, m); why != "" {
		_ = h.db.NoteDeviceMove(ctx, m.Serial, why)
		return false
	}
	res, err := h.importOnTarget(ctx, m.Target, m.Serial)
	if err != nil {
		log.Printf("[move] %s: import: %v", m.Serial, err)
		_ = h.db.NoteDeviceMove(ctx, m.Serial, "import failed, trying again: "+err.Error())
		return false
	}
	_ = h.db.MoveImported(ctx, m.Serial, res.EnrollToken)
	ids := []uuid.UUID{m.DeviceID}
	var first *db.Command
	if m.Kind == "firmware" {
		payload, _ := json.Marshal(map[string]string{"cmd": "setprop persist.sys.mdm.url " + m.Target})
		first, err = h.db.CreateCommandBy(ctx, "shell", "", payload, "devices", ids, "device move")
		if err == nil {
			h.pushCommand(ctx, first, "devices", ids)
			// Queued behind the setprop: the queue delivers in order and stops at a reboot.
			var reboot *db.Command
			if reboot, err = h.db.CreateCommandBy(ctx, "reboot", "", nil, "devices", ids, "device move"); err == nil {
				h.pushCommand(ctx, reboot, "devices", ids)
			} else if cerr := h.db.CancelDeviceCommand(ctx, first.ID, m.DeviceID); cerr != nil {
				// The setprop must not stand alone: a device holding the new address with
				// no reboot would move on any later restart, with no custody or token.
				log.Printf("[move] %s: could not withdraw the setprop after the reboot failed: %v", m.Serial, cerr)
			}
		}
	} else {
		payload, _ := json.Marshal(map[string]string{"server_url": m.Target, "enroll_token": res.EnrollToken})
		first, err = h.db.CreateCommandBy(ctx, "move_server", "", payload, "devices", ids, "device move")
		if err == nil {
			h.pushCommand(ctx, first, "devices", ids)
		}
	}
	if err != nil || first == nil {
		log.Printf("[move] %s: queue: %v", m.Serial, err)
		_ = h.db.NoteDeviceMove(ctx, m.Serial, "could not queue the move, trying again")
		return false
	}
	_ = h.db.SetDeviceMoveState(ctx, m.Serial, db.MoveSent, "sent", &first.ID)
	// Filed as moving (a suspicion until it arrives): no offline alerts, no new commands.
	if _, err := h.db.SetDeviceCustody(ctx, m.DeviceID, "prod", m.Target, time.Time{}, "move"); err != nil {
		log.Printf("[move] %s: custody: %v", m.Serial, err)
	}
	log.Printf("[move] %s (%s) sent to %s", m.Serial, m.Kind, m.Target)
	return true
}

// seenOnTarget is the device's last check-in on the target, or nil.
func (h *Handler) seenOnTarget(ctx context.Context, m db.DeviceMove) *time.Time {
	var there struct {
		Device struct {
			LastSeenAt *time.Time `json:"last_seen_at"`
		} `json:"device"`
	}
	if err := h.moveCall(ctx, http.MethodGet, m.Target, "/api/v1/devices/"+m.Serial, nil, &there); err != nil {
		return nil
	}
	return there.Device.LastSeenAt
}

func (h *Handler) markArrived(ctx context.Context, m db.DeviceMove, at time.Time) {
	_ = h.db.SetDeviceMoveState(ctx, m.Serial, db.MoveArrived, "checked in on "+m.Target, nil)
	if _, err := h.db.SetDeviceCustody(ctx, m.DeviceID, "prod", m.Target, at, db.CustodyMigrate); err != nil {
		log.Printf("[move] %s: custody: %v", m.Serial, err)
	}
	_, _ = h.db.CancelPendingForCustody(ctx, m.DeviceID)
	_ = h.db.InsertAudit(ctx, "device move", "device.move_arrived", m.Serial, m.Target)
	log.Printf("[move] %s arrived on %s", m.Serial, m.Target)
}

// followMove checks a sent move: arrived on the target, failed on the device, back here,
// or never delivered (pending again).
func (h *Handler) followMove(ctx context.Context, m db.DeviceMove) {
	if m.SentAt == nil {
		return
	}
	if at := h.seenOnTarget(ctx, m); at != nil && at.After(*m.SentAt) {
		h.markArrived(ctx, m, *at)
		return
	}
	// When the device left: its acknowledgement of the move, else the send. A device offline
	// when the move went out collects it later, and its acknowledgement is heard here then
	// (16 Nov 2026: ADRB0AAK600022 took it 13 minutes late and was taken for back).
	left := *m.SentAt
	if m.CommandID != nil {
		status, out, at, err := h.db.MoveCommandStatus(ctx, *m.CommandID, m.DeviceID)
		switch {
		case err != nil:
		case status == "failed":
			_ = h.db.SetDeviceMoveState(ctx, m.Serial, db.MoveFailed, strings.TrimSpace("the device refused the move: "+out), nil)
			_ = h.db.InsertAudit(ctx, "device move", "device.move_failed", m.Serial, strings.TrimSpace(out))
			_ = h.db.ClearDeviceCustody(ctx, m.DeviceID)
			return
		case status == "" && time.Since(*m.SentAt) > time.Hour:
			// Never delivered: the command has expired. Try again when it is back.
			_ = h.db.SetDeviceMoveState(ctx, m.Serial, db.MovePending, "the move was never delivered, trying again", nil)
			_ = h.db.ClearDeviceCustody(ctx, m.DeviceID)
			return
		case status == "running" && time.Since(at) > moveStallAfter && m.LastSeenAt != nil && m.LastSeenAt.After(at.Add(2*time.Minute)):
			// The device said it was enrolling on the target and never finished, and it is still here.
			why := "the device began enrolling on the new server and never finished"
			if o := strings.TrimSpace(out); o != "" {
				why += " (its last word: " + o + ")"
			}
			_ = h.db.SetDeviceMoveState(ctx, m.Serial, db.MoveFailed, why, nil)
			_ = h.db.InsertAudit(ctx, "device move", "device.move_failed", m.Serial, why)
			_ = h.db.ClearDeviceCustody(ctx, m.DeviceID)
			return
		case status != "" && at.After(left):
			left = at
		}
	}
	if m.LastSeenAt != nil && m.LastSeenAt.After(left.Add(moveCameBackGap)) {
		why := "still checks in here after the move"
		if m.Kind == "firmware" {
			why += " (its client may not read the server setting)"
		} else {
			why += " (it went back by itself: the new server did not take it)"
		}
		_ = h.db.SetDeviceMoveState(ctx, m.Serial, db.MoveCameBack, why, nil)
		_ = h.db.InsertAudit(ctx, "device move", "device.move_came_back", m.Serial, why)
		log.Printf("[move] %s came back: %s", m.Serial, why)
	}
}

// retryMove puts a failed or came-back move back to pending after a wait that grows with each
// try, up to moveMaxAttempts. It reports whether it did. The next send imports the device again and
// makes a fresh token, so a stale one is no reason to give up.
func (h *Handler) retryMove(ctx context.Context, m db.DeviceMove) bool {
	if m.Attempts >= moveMaxAttempts || time.Since(m.UpdatedAt) < time.Duration(m.Attempts)*moveRetryEvery {
		return false
	}
	note := fmt.Sprintf("trying again (try %d of %d); last time: %s", m.Attempts+1, moveMaxAttempts, strings.TrimSpace(m.Note))
	if err := h.db.SetDeviceMoveState(ctx, m.Serial, db.MovePending, note, nil); err != nil {
		return false
	}
	_ = h.db.InsertAudit(ctx, "device move", "device.move_retry", m.Serial, note)
	_ = h.db.ClearDeviceCustody(ctx, m.DeviceID)
	log.Printf("[move] %s: %s", m.Serial, note)
	return true
}
