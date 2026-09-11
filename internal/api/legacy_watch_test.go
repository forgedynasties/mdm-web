package api

import "testing"

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

func TestParseUpdateEnginePhase(t *testing.T) {
	if _, phase, _ := parseUpdateEngineLine(`update_engine: [INFO:partition_writer.cc(177)] Applying 48 operations to partition "boot"`); phase != "installing boot" {
		t.Errorf("phase = %q, want %q", phase, "installing boot")
	}
	if _, phase, _ := parseUpdateEngineLine("update_engine: [INFO:vabc_partition_writer.cc(416)] Finalizing product COW image"); phase != "finalizing" {
		t.Errorf("phase = %q, want %q", phase, "finalizing")
	}
}
