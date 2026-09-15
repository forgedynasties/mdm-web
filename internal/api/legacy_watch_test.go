package api

import (
	"os"
	"strings"
	"testing"
)

// Lines taken from real T7 installs. The point of this test is the false positives:
// update_engine reports ErrorCode::kSuccess for each of its internal actions long
// before the update is applied, and a device that "finished" at 2% is worse than no
// progress at all.
func TestParseUpdateEngineLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		done string
		pct  int
	}{
		{"boot flags action", "update_engine: [INFO:action_processor.cc(116)] ActionProcessor: finished UpdateBootFlagsAction with code ErrorCode::kSuccess", "", -1},
		{"cleanup action", "update_engine: [INFO:action_processor.cc(116)] ActionProcessor: finished CleanupPreviousUpdateAction with code ErrorCode::kSuccess", "", -1},
		{"install plan action", "update_engine: [INFO:action_processor.cc(116)] ActionProcessor: finished InstallPlanAction with code ErrorCode::kSuccess", "", -1},
		{"verifier action", "update_engine: [INFO:action_processor.cc(116)] ActionProcessor: finished FilesystemVerifierAction with code ErrorCode::kSuccess", "", -1},
		{"postinstall action", "update_engine: [INFO:action_processor.cc(116)] ActionProcessor: finished last action PostinstallRunnerAction with code ErrorCode::kSuccess", "", -1},
		{"progress", "update_engine: [INFO:delta_performer.cc(108)] Completed 253/2603 operations (9%), 179765248/1893987101 bytes downloaded (9%), overall progress 8%", "", 8},
		{"applied", "update_engine: [INFO:update_attempter_android.cc(711)] Update successfully applied, waiting to reboot.", "ok", 100},
		{"payload complete ok", "update_engine: onPayloadApplicationComplete(ErrorCode::kSuccess (0))", "ok", 100},
		{"payload complete error", "update_engine: onPayloadApplicationComplete(ErrorCode::kDownloadTransferError (9))", "fail", -1},
		{"resume warning is not a failure", "update_engine: [WARNING:delta_performer.cc(1388)] Failed to resume update update-state-next-operation invalid: -1", "", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			done, _, pct := parseUpdateEngineLine(c.line)
			if done != c.done {
				t.Errorf("done = %q, want %q", done, c.done)
			}
			if pct != c.pct {
				t.Errorf("pct = %d, want %d", pct, c.pct)
			}
		})
	}
}

// Phases must be one of the statuses the rollout pages score. Partition names and
// the per-partition "Finalizing <p> COW image" are write progress, not phases.
func TestParseUpdateEnginePhase(t *testing.T) {
	cases := []struct{ line, phase string }{
		{`update_engine: [INFO:partition_writer.cc(177)] Applying 48 operations to partition "boot"`, "installing"},
		{"update_engine: [INFO:partition_writer_factory_android.cc(36)] Virtual AB Compression Enabled, using VABC Partition Writer for `product`", "installing"},
		{"update_engine: [INFO:vabc_partition_writer.cc(416)] Finalizing product COW image", "installing"},
		{"update_engine: [INFO:filesystem_verifier_action.cc(380)] Hashing partition 0 (boot) on device /dev/block/bootdevice/by-name/boot_a", "verifying"},
		{"update_engine: [INFO:action_processor.cc(143)] ActionProcessor: starting PostinstallRunnerAction", "finalizing"},
		{"update_engine: [INFO:postinstall_runner_action.cc(482)] All post-install commands succeeded", "finalizing"},
		// Logged again while verifying and in postinstall: must not read as the write.
		{"update_engine: [INFO:snapshot.cpp(2641)] Mapped COW image for vendor_a at vendor_a-cow-img", ""},
		{"update_engine: [INFO:snapshot.cpp(2826)] MapAllSnapshots succeeded.", ""},
	}
	for _, c := range cases {
		if _, phase, _ := parseUpdateEngineLine(c.line); phase != c.phase {
			t.Errorf("%s\n  phase = %q, want %q", c.line, phase, c.phase)
		}
	}
}

// TestParseUpdateEngineDumpReplay replays a real install (AT070AABU00169, 2026-09-15)
// the way the watcher sees it: a tail of the log at each moment, including a tiny
// buffer that has rotated out every marker but the newest. Before the fix this read
// "finalizing" for most of the write (the tail's newest marker was the previous
// partition's "Finalizing odm COW image") and "installing recovery" /
// "installing vbmeta_system", which the rollout page could not score.
func TestParseUpdateEngineDumpReplay(t *testing.T) {
	raw, err := os.ReadFile("testdata/update_engine_t7_v2.0.3_to_v2.0.96l.log")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	stamp := func(l string) string { return l[:18] } // "09-15 03:47:36.940"
	const (
		verifyAt  = "09-15 03:47:36.940" // Hashing partition 0 (boot)
		postAt    = "09-15 03:49:32.533" // starting PostinstallRunnerAction
		appliedAt = "09-15 03:49:41.786" // Update successfully applied
	)
	for _, tail := range []int{3, 8, 60} {
		sawVerify, sawFinal := false, false
		for i := range lines {
			from := i + 1 - tail
			if from < 0 {
				from = 0
			}
			done, phase, pct := parseUpdateEngineDump(strings.Join(lines[from:i+1], "\n"))
			at := stamp(lines[i])
			want := "installing"
			switch {
			case at >= appliedAt:
				want = "ok"
			case at >= postAt:
				want = "finalizing"
			case at >= verifyAt:
				want = "verifying"
			}
			got := phase
			if done != "" {
				got = done
			}
			if done == "" && phase == "" && pct < 0 {
				continue // nothing to read in this window: the watcher leaves the row as it was
			}
			if got != want {
				t.Fatalf("tail %d at %s: got done=%q phase=%q pct=%d, want %s\n  %s", tail, at, done, phase, pct, want, lines[i])
			}
			if (want == "verifying" || want == "finalizing") && pct != 100 {
				t.Errorf("tail %d at %s: %s pct = %d, want 100", tail, at, want, pct)
			}
			sawVerify = sawVerify || want == "verifying"
			sawFinal = sawFinal || want == "finalizing"
		}
		if !sawVerify || !sawFinal {
			t.Errorf("tail %d: replay never reached verifying (%v) / finalizing (%v)", tail, sawVerify, sawFinal)
		}
	}
}
