package api

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mdm/internal/ota"

	"github.com/google/uuid"
)

// Install progress for legacy (otautil) devices.
//
// The otautil app reports nothing once it hands the package to update_engine, so
// the only way to follow an install is update_engine's own log. Where the device
// ALSO runs the MDM client (our firmware carries both on builds that predate the
// agent's OTA support), the client can stream logcat over its WebSocket — the
// same pipe the Logs tab uses. Start a filtered stream when the download
// finishes, read the percentages out of it, and stop at the first terminal line.
//
// Lines this reads, from a real T7 install:
//
//	update_engine: [INFO:delta_performer.cc(108)] Completed 1521/2618 operations
//	  (58%), 1172389888/1911124571 bytes downloaded (61%), overall progress 59%
//	update_engine: [INFO:partition_writer.cc(177)] Applying 48 operations to partition "boot"
//	update_engine: [INFO:vabc_partition_writer.cc(416)] Finalizing product COW image
//	update_engine: [INFO:update_attempter_android.cc(…)] Update successfully applied…
//
// A device without the MDM client simply has no stream: the row stays
// "installing" until it polls on the new build, which is the existing behaviour.

var (
	reOverall   = regexp.MustCompile(`overall progress (\d+)%`)
	rePartition = regexp.MustCompile(`Applying \d+ operations to partition "([a-z_]+)"`)
	reFinalize  = regexp.MustCompile(`Finalizing ([a-z_]+) COW image`)
	reErrCode   = regexp.MustCompile(`ErrorCode(?:::k|: )([A-Za-z0-9]+)`)
)

// legacyWatchTimeout bounds a watch: a 2 GB image on slow storage takes tens of
// minutes, and the device's own poll on the new build closes the row anyway.
const legacyWatchTimeout = 90 * time.Minute

// How often to ask a socket-less device for its update_engine log, and how many lines
// to ask for: an install logs a progress line every ~30s, so 60 lines covers the gap
// with room for the partition/finalize markers.
const (
	legacyPollEvery = 2 * time.Minute
	legacyPollLines = 60
)

var legacyWatching sync.Map // serial -> struct{}

// ResumeLegacyWatch re-attaches the watcher when a device turns up with a legacy
// install in flight. The watch needs a live WebSocket, and the moment the download
// finishes is exactly when a device on a weak link may not have one — without this
// the row sits at "installing" with no progress for the whole install. Wired to the
// hub's onConnect hook and to the check-in path, both of which are proof the device
// is reachable again. Cheap: one indexed lookup, and the watcher itself no-ops when
// one is already running for that serial.
func (h *Handler) ResumeLegacyWatch(ctx context.Context, deviceID uuid.UUID) {
	device, err := h.db.GetDeviceByID(ctx, deviceID)
	if err != nil || device == nil {
		return
	}
	dep, dev, err := h.db.ResolveLegacyDeployment(ctx, device.SerialNumber)
	if err != nil || dep == nil || dev == nil {
		return
	}
	switch dev.Status {
	case "installing", "verifying", "finalizing":
		h.watchLegacyInstall(device.SerialNumber, dep.ID)
	}
}

// watchLegacyInstall follows update_engine on one device, if it is reachable.
// Safe to call for any serial: it returns immediately when the device is not in
// the fleet, has no live WebSocket, or is already being watched.
func (h *Handler) watchLegacyInstall(serial string, depID int) {
	if h.logs == nil {
		return
	}
	if _, busy := legacyWatching.LoadOrStore(serial, struct{}{}); busy {
		return
	}
	go func() {
		defer legacyWatching.Delete(serial)
		ctx, cancel := context.WithTimeout(context.Background(), legacyWatchTimeout)
		defer cancel()

		device, err := h.db.GetDevice(ctx, serial)
		if err != nil || device == nil {
			return // legacy-only device: nothing to read from
		}
		if !h.hub.IsConnected(device.ID) {
			// No socket, but the client still checks in over HTTP and the command
			// queue rides along with it — so ask for a logcat dump every couple of
			// minutes instead of streaming. Slower, same answer.
			h.pollLegacyInstall(ctx, device.ID, serial, depID)
			return
		}
		reqID := uuid.NewString()
		ch := h.logs.Open(reqID)
		defer h.logs.Close(reqID)

		start, _ := json.Marshal(map[string]any{
			"type": "start_logcat_stream", "request_id": reqID,
			"tag": "update_engine", "level": "I", "buffer": "main", "tail": 0,
		})
		if !h.hub.Push(device.ID, start) {
			return
		}
		defer func() {
			stop, _ := json.Marshal(map[string]any{"type": "stop_logcat_stream", "request_id": reqID})
			h.hub.Push(device.ID, stop)
		}()
		log.Printf("[legacy-ota] watching update_engine on %s (deployment %d)", serial, depID)

		phase, lastPct := "installing", -1
		for {
			select {
			case <-ctx.Done():
				log.Printf("[legacy-ota] watch on %s timed out", serial)
				return
			case chunk, ok := <-ch:
				if !ok {
					return
				}
				for _, line := range strings.Split(chunk, "\n") {
					if line == "" {
						continue
					}
					done, newPhase, pct := parseUpdateEngineLine(line)
					if newPhase != "" {
						phase = newPhase
					}
					if pct >= 0 && pct != lastPct {
						lastPct = pct
						ota.Legacy.Set(serial, phase, pct)
						if depID > 0 {
							_ = h.db.SetLegacyDeploymentDevice(ctx, depID, serial, phase, pct, "", "")
						}
					}
					switch done {
					case "ok":
						// "Update successfully applied, waiting to reboot" — the payload is
						// on the inactive slot and nothing else happens until the device
						// reboots, which the legacy client does not do on its own.
						ota.Legacy.Set(serial, "awaiting_reboot", 100)
						_ = h.db.SetLegacyOTADeviceStatus(ctx, serial, "awaiting_reboot", "", "")
						if depID > 0 {
							_ = h.db.SetLegacyDeploymentDevice(ctx, depID, serial, "awaiting_reboot", 100, "", "")
						}
						log.Printf("[legacy-ota] %s: update applied, waiting for its reboot", serial)
						return
					case "fail":
						reason := newPhase
						if reason == "" {
							reason = "update_engine failed"
						}
						ota.Legacy.Clear(serial)
						_ = h.db.SetLegacyOTADeviceStatus(ctx, serial, "failed", "", reason)
						if depID > 0 {
							_ = h.db.SetLegacyDeploymentDevice(ctx, depID, serial, "failed", -1, reason, "")
						}
						log.Printf("[legacy-ota] %s: %s", serial, reason)
						return
					}
				}
			}
		}
	}()
}

// parseUpdateEngineLine reads one logcat line. done is "" (keep going), "ok" or
// "fail"; phase is the phase or, on failure, the reason; pct is -1 when the line
// carries no progress.
func parseUpdateEngineLine(line string) (done, phase string, pct int) {
	pct = -1
	// Only the WHOLE update finishing counts as done. update_engine logs
	// "ErrorCode::kSuccess" after every internal action — UpdateBootFlagsAction,
	// CleanupPreviousUpdateAction, InstallPlanAction — within seconds of starting,
	// so treating a bare kSuccess as the end marked devices "awaiting reboot" at 2%.
	switch {
	case strings.Contains(line, "Update successfully applied"),
		strings.Contains(line, "UPDATED_NEED_REBOOT"),
		strings.Contains(line, "onPayloadApplicationComplete") && strings.Contains(line, "ErrorCode::kSuccess"):
		return "ok", "", 100
	case strings.Contains(line, "onPayloadApplicationComplete") && strings.Contains(line, "ErrorCode"),
		strings.Contains(line, "Update failed"):
		if m := reErrCode.FindStringSubmatch(line); len(m) == 2 && m[1] != "kSuccess" && m[1] != "Success" {
			return "fail", "update_engine error " + m[1], -1
		}
		return "fail", "", -1
	}
	if m := reOverall.FindStringSubmatch(line); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			pct = n
		}
	}
	if m := rePartition.FindStringSubmatch(line); len(m) == 2 {
		phase = "installing " + m[1]
	} else if m := reFinalize.FindStringSubmatch(line); len(m) == 2 {
		phase = "finalizing"
	}
	return "", phase, pct
}


// pollLegacyInstall follows an install on a device with no live WebSocket by queueing
// a logcat request the device picks up on its next HTTP check-in. The result comes
// back through SubmitLogcat, where legacyProgressFromLogcat reads it — this loop only
// keeps asking. One request in flight at a time, and it stops as soon as the row
// leaves the installing states.
func (h *Handler) pollLegacyInstall(ctx context.Context, deviceID uuid.UUID, serial string, depID int) {
	log.Printf("[legacy-ota] polling update_engine on %s over check-ins (deployment %d)", serial, depID)
	ticker := time.NewTicker(legacyPollEvery)
	defer ticker.Stop()
	for {
		dev, err := h.db.GetLegacyOTADevice(ctx, serial)
		if err != nil || dev == nil {
			return
		}
		switch dev.Status {
		case "installing", "verifying", "finalizing":
		default:
			return // installed, awaiting reboot, failed — nothing left to follow
		}
		// The stream is better when it is available: if the socket came back, hand
		// over to it and stop polling.
		if h.hub.IsConnected(deviceID) {
			go func() {
				legacyWatching.Delete(serial)
				h.watchLegacyInstall(serial, depID)
			}()
			return
		}
		if _, err := h.db.CreateLogcatRequest(ctx, deviceID, "I", legacyPollLines, "update_engine"); err != nil {
			log.Printf("[legacy-ota] logcat request for %s: %v", serial, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// legacyProgressFromLogcat reads a logcat dump for a device that is mid legacy install
// and moves its row along. Called for every logcat result, whichever way it arrived,
// so a dump someone pulled by hand counts too. No-ops unless the device is installing.
func (h *Handler) legacyProgressFromLogcat(ctx context.Context, serial, content string) {
	if serial == "" || content == "" {
		return
	}
	dev, err := h.db.GetLegacyOTADevice(ctx, serial)
	if err != nil || dev == nil {
		return
	}
	switch dev.Status {
	case "installing", "verifying", "finalizing":
	default:
		return
	}
	depID := h.legacyDeploymentFor(ctx, serial)
	phase, pct, done, reason := "", -1, "", ""
	for _, line := range strings.Split(content, "\n") {
		d, ph, p := parseUpdateEngineLine(line)
		if ph != "" {
			phase = ph
		}
		if p >= 0 {
			pct = p
		}
		if d != "" {
			done, reason = d, ph
		}
	}
	switch done {
	case "ok":
		ota.Legacy.Set(serial, "awaiting_reboot", 100)
		_ = h.db.SetLegacyOTADeviceStatus(ctx, serial, "awaiting_reboot", "", "")
		if depID > 0 {
			_ = h.db.SetLegacyDeploymentDevice(ctx, depID, serial, "awaiting_reboot", 100, "", "")
		}
		log.Printf("[legacy-ota] %s: update applied (from a logcat dump), waiting for its reboot", serial)
		return
	case "fail":
		if reason == "" {
			reason = "update_engine failed"
		}
		ota.Legacy.Clear(serial)
		_ = h.db.SetLegacyOTADeviceStatus(ctx, serial, "failed", "", reason)
		if depID > 0 {
			_ = h.db.SetLegacyDeploymentDevice(ctx, depID, serial, "failed", -1, reason, "")
		}
		log.Printf("[legacy-ota] %s: %s (from a logcat dump)", serial, reason)
		return
	}
	if pct < 0 {
		return
	}
	if phase == "" {
		phase = "installing"
	}
	ota.Legacy.Set(serial, phase, pct)
	if depID > 0 {
		_ = h.db.SetLegacyDeploymentDevice(ctx, depID, serial, phase, pct, "", "")
	}
}
