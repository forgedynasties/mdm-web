package dashboard

import (
	"strings"
	"testing"

	"mdm/internal/db"
)

// No alert reads as its code (user, 30 Oct 2026: "no_command_channel … should be in normal english").
func TestAlertWordsPlain(t *testing.T) {
	h := humanizeAlert(db.Alert{Type: "no_command_channel", Detail: []byte(`{"minutes":125,"http_commands":true}`)})
	if h.Headline != "Reporting, but commands are delayed" || strings.Contains(string(h.Sentence), "WebSocket") {
		t.Errorf("with HTTP fallback: %q / %q", h.Headline, h.Sentence)
	}
	h = humanizeAlert(db.Alert{Type: "no_command_channel", Detail: []byte(`{"minutes":30,"http_commands":false}`)})
	if h.Headline != "Reporting, but not taking commands" || !strings.Contains(string(h.Sentence), "won't run until it reconnects") {
		t.Errorf("socket only: %q / %q", h.Headline, h.Sentence)
	}
	if got := alertTypeLabel("no_command_channel"); got != "Reporting, but commands are delayed" {
		t.Errorf("label = %q", got)
	}
	if got := alertTypeLabel("some_new_type"); got != "Some new type" {
		t.Errorf("fallback label = %q", got)
	}
}
