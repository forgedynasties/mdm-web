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

var legacyWatching sync.Map // serial -> struct{}

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
			return // legacy-only device: nothing to stream from
		}
		if !h.hub.IsConnected(device.ID) {
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
						ota.Legacy.Set(serial, "installed", 100)
						_ = h.db.SetLegacyOTADeviceStatus(ctx, serial, "installing", "", "")
						if depID > 0 {
							_ = h.db.SetLegacyDeploymentDevice(ctx, depID, serial, "installing", 100, "", "")
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
	switch {
	case strings.Contains(line, "Update successfully applied"),
		strings.Contains(line, "UPDATED_NEED_REBOOT"),
		strings.Contains(line, "ErrorCode::kSuccess"):
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
