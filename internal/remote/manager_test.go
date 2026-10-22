package remote

import "testing"

func TestFromBrowserOK(t *testing.T) {
	for msg, want := range map[string]bool{
		`{"type":"input_event","action":"key","event":"down","keycode":61,"meta":1}`: true,
		`{"type":"input_event","action":"text","text":"Hé @"}`:                       true,
		`{"type":"wake_screen"}`:                                                     true,
		`{"type":"clipboard_get"}`:                                                   true,
		`{"type":"config","kiosk_enabled":false}`:                                    false,
		`{"type":"command","command_type":"shell"}`:                                  false,
		`not json`: false,
		`{}`:       false,
	} {
		if got := fromBrowserOK([]byte(msg)); got != want {
			t.Errorf("fromBrowserOK(%s) = %v, want %v", msg, got, want)
		}
	}
}
