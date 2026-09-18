package dashboard

import (
	"testing"

	"mdm/internal/db"
)

// TestHumanizeAuditReadsAsASentence checks the thing the feature exists for: an
// activity line must read as something a person did, with the actor as the subject.
func TestHumanizeAuditReadsAsASentence(t *testing.T) {
	cases := []struct {
		action, target, detail string
		want                   string
	}{
		{"device.wlc_charging", "AT070", "disabled", "disabled wireless charging"},
		{"device.wlc_charging", "AT070", "enabled", "enabled wireless charging"},
		{"device.kiosk", "AT070", "enabled (single_app)", "enabled kiosk mode (single_app)"},
		{"device.kiosk", "AT070", "disabled", "disabled kiosk mode"},
		{"device.nickname", "AT070", "Register 3", `named the device "Register 3"`},
		{"device.nickname", "AT070", "", "cleared the device name"},
		{"device.hide", "AT070", "", "hid the device"},
		{"device.restaurant", "AT070", "", "removed the device from its restaurant"},
		{"device.restaurant", "AT070", "b5a907ba", "assigned the device to a restaurant"},
		{"deployment.retry", "AT070", "46", "retried the update (rollout 46)"},
		{"command.send", "install_apk", "", "sent a install apk command"},
		{"device.queue_clear", "AT070", "", "cleared the command queue"},
		// Unknown actions still render, rather than vanishing from the feed.
		{"release.publish", "10203", "v2.1.012", "release publish"},
	}
	for _, c := range cases {
		got := humanizeAudit(db.AuditEntry{Action: c.action, Target: c.target, Detail: c.detail})
		if got != c.want {
			t.Errorf("%s/%q = %q, want %q", c.action, c.detail, got, c.want)
		}
	}
}

// TestHumanizeAuditToggleWithoutDirection covers rows written before the toggles
// recorded a word. Guessing "disabled" there would put a claim in the log that the log
// does not support.
func TestHumanizeAuditToggleWithoutDirection(t *testing.T) {
	got := humanizeAudit(db.AuditEntry{Action: "device.wlc_charging", Detail: ""})
	if got != "changed wireless charging" {
		t.Errorf("got %q, want a statement that asserts no direction", got)
	}
}
