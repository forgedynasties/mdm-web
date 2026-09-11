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
// ALSO runs the MDM client — our firmware carries both on builds that predate the
// agent's OTA support — we can ask for that log.
//
// One mechanism does it: a shell command, once a minute, until the install ends.
//
//	logcat -d -s update_engine | tail -n 60
//
// Commands reach a device either way it is connected, over its socket or on its
// next HTTP check-in, and the output comes back on the ack. An earlier version
// streamed logcat over the WebSocket instead and kept finding new ways to miss the
// answer: the stream only carries what is logged after it attaches, so an install
// that finished while nothing was watching showed its last percentage forever;
// "logcat -T" (the stream's history replay) returns nothing on the older builds;
// and a device with no socket had no stream at all. Polling is slower and duller
// and it works everywhere.
//
// Lines this reads, from a real T7 install:
//
//	update_engine: [INFO:delta_performer.cc(108)] Completed 1521/2618 operations
//	  (58%), 1172389888/1911124571 bytes downloaded (61%), overall progress 59%
//	update_engine: [INFO:partition_writer.cc(177)] Applying 48 operations to partition "boot"
//	update_engine: [INFO:vabc_partition_writer.cc(416)] Finalizing product COW image
//	update_engine: [INFO:update_attempter_android.cc(…)] Update successfully applied…
//
// A device that is not in the fleet has no client to ask: its row stays
// "installing" until it polls on the new build, which closes the rollout anyway.

var (
	reOverall   = regexp.MustCompile(`overall progress (\d+)%`)
	rePartition = regexp.MustCompile(`Applying \d+ operations to partition "([a-z_]+)"`)
	reFinalize  = regexp.MustCompile(`Finalizing ([a-z_]+) COW image`)
	reErrCode   = regexp.MustCompile(`ErrorCode(?:::k|: )([A-Za-z0-9]+)`)
)

const (
	// A 2 GB image on slow storage takes tens of minutes; the device's own poll on
	// the new build closes the row anyway, so this is only a backstop.
	legacyWatchTimeout = 90 * time.Minute
	// Once a minute: update_engine logs progress every ~30s, and 60 lines of tail
	// covers the gap with room for the partition, finalize and terminal markers.
	legacyPollEvery = time.Minute
	// Piped through tail, not "logcat -t": the client on these builds runs the
	// string through a shell and its logcat returns nothing at all for -t.
	legacyPollCmd = "logcat -d -s update_engine | tail -n 60"
)

// legacyProbes are the shell commands this package issued to read update_engine on a
// device with no socket, so their output can be recognised when it comes back on the
// command ack. commandID -> serial.
var legacyProbes sync.Map

var legacyWatching sync.Map // serial -> struct{}

// SettleLegacyAtBuild closes a legacy rollout for a device that is ALSO in the fleet,
// the moment its MDM check-in reports the build it was offered. Otherwise the row
// would keep saying "awaiting reboot" until the device's next legacy poll — up to
// fifteen minutes after it has already booted into the new build and told us so.
func (h *Handler) SettleLegacyAtBuild(ctx context.Context, serial, buildID string) {
	if serial == "" || buildID == "" {
		return
	}
	dev, err := h.db.GetLegacyOTADevice(ctx, serial)
	if err != nil {
		log.Printf("[legacy-ota] settle lookup for %s: %v", serial, err)
		return
	}
	if dev == nil || dev.Status == "idle" || dev.Status == "updated" {
		return
	}
	if dev.BuildID != buildID {
		_ = h.db.SetLegacyOTADeviceBuild(ctx, serial, buildID)
	}
	if dev.OfferedBuild != "" && dev.OfferedBuild != buildID {
		return // still on the old build: nothing to settle
	}
	_ = h.db.CompleteLegacyDeploymentsAtBuild(ctx, serial, buildID)
	_ = h.db.SetLegacyOTADeviceStatus(ctx, serial, "updated", "", "")
	ota.Legacy.Clear(serial)
	log.Printf("[legacy-ota] %s checked in on %s: rollout settled", serial, buildID)
}

// ResumeLegacyWatches re-attaches every in-flight legacy install after a restart.
// A watcher lives in memory, so a deploy in the middle of a 40-minute install
// otherwise leaves those rows frozen until the device's next 15-minute poll.
func (h *Handler) ResumeLegacyWatches(ctx context.Context) {
	devices, err := h.db.ListLegacyOTADevices(ctx)
	if err != nil {
		return
	}
	for _, d := range devices {
		switch d.Status {
		case "installing", "verifying", "finalizing":
			h.watchLegacyInstall(d.Serial, h.legacyDeploymentFor(ctx, d.Serial))
		}
	}
}

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

// watchLegacyInstall follows update_engine on one device for as long as its install
// runs. Safe to call for any serial: it no-ops when the device is not in the fleet
// (nothing to ask) or when a watch is already running for it.
func (h *Handler) watchLegacyInstall(serial string, depID int) {
	if _, busy := legacyWatching.LoadOrStore(serial, struct{}{}); busy {
		return
	}
	go func() {
		defer legacyWatching.Delete(serial)
		ctx, cancel := context.WithTimeout(context.Background(), legacyWatchTimeout)
		defer cancel()

		device, err := h.db.GetDevice(ctx, serial)
		if err != nil || device == nil {
			return // legacy-only device: no client of ours to ask
		}
		h.pollLegacyInstall(ctx, device.ID, serial, depID)
	}()
}

// pollLegacyInstall asks the device for its update_engine log once a minute until the
// install ends. The output comes back on the command ack — HTTP or WebSocket, both
// land in LegacyProbeOutput — so this loop only keeps asking.
func (h *Handler) pollLegacyInstall(ctx context.Context, deviceID uuid.UUID, serial string, depID int) {
	log.Printf("[legacy-ota] following update_engine on %s (deployment %d)", serial, depID)
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
		// A shell command, NOT a logcat request: logcat requests only ever go out over
		// the WebSocket, so on a device without one they sit pending forever (some on
		// this fleet from months ago). Commands ride the check-in response too, and
		// the output comes back on the ack — which is how the Shell page already
		// works on this same hardware.
		h.sendLegacyProbe(ctx, deviceID, serial)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sendLegacyProbe asks a device for its recent update_engine log with a shell
// command. Works on every build we manage, over a socket or over the check-in
// queue, which is why it is also fired the moment a stream attaches: "logcat -T"
// (what the stream uses to replay history) returns nothing at all on the older
// builds, so a device that finished while nothing was watching would otherwise
// stay on its last known percentage until it rebooted.
func (h *Handler) sendLegacyProbe(ctx context.Context, deviceID uuid.UUID, serial string) {
	payload, _ := json.Marshal(map[string]string{"cmd": legacyPollCmd})
	cmd, err := h.db.CreateCommandBy(ctx, "shell", "", payload, "devices", []uuid.UUID{deviceID}, "")
	if err != nil {
		log.Printf("[legacy-ota] progress probe for %s: %v", serial, err)
		return
	}
	legacyProbes.Store(cmd.ID, serial)
	time.AfterFunc(20*time.Minute, func() { legacyProbes.Delete(cmd.ID) })
	h.pushCommand(ctx, cmd, "devices", []uuid.UUID{deviceID})
}

// LegacyProbeOutput feeds a probe's output back in when its ack arrives. Returns
// false when the command was not one of ours, so the ack path can skip the work.
func (h *Handler) LegacyProbeOutput(ctx context.Context, cmdID uuid.UUID, output string) bool {
	v, ok := legacyProbes.Load(cmdID)
	if !ok {
		return false
	}
	legacyProbes.Delete(cmdID)
	serial, _ := v.(string)
	h.legacyProgressFromLogcat(ctx, serial, output)
	return true
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
