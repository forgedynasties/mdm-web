package product

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// ValidateAdbApprove checks an adb_approve command before it is stored. The command makes
// a Standard-client device press Allow on Android's "Allow USB debugging?" dialog, which
// lets one computer into the device, so like adb_tcp it goes to exactly one named device,
// and it must name the key: the client presses only a dialog showing that fingerprint.
// Returns "" when the command may be sent, else the reason.
//
// Payload: {"fingerprint": "AB:CD:…", "always": true, "wait_s": 60}. fingerprint is the
// MD5 of the computer's adb public key as Android shows it (colons optional); always
// ticks "Always allow from this computer" (default true); wait_s is how long the device
// waits for the dialog, 5-240 (default 60).
func ValidateAdbApprove(targetType string, targets int, payload json.RawMessage) string {
	if targetType != "devices" || targets != 1 {
		return "adb_approve goes to exactly one named device (target_type devices, one target)"
	}
	p := struct {
		Fingerprint string `json:"fingerprint"`
		WaitS       *int   `json:"wait_s"`
	}{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &p); err != nil {
			return "payload must be {\"fingerprint\": \"AB:CD:…\", \"always\": true, \"wait_s\": N}"
		}
	}
	hex := strings.Map(func(r rune) rune {
		if r == ':' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, p.Fingerprint)
	if len(hex) != 32 || strings.Trim(strings.ToLower(hex), "0123456789abcdef") != "" {
		return "payload.fingerprint must be the adb key's MD5 fingerprint (32 hex digits, colons optional)"
	}
	if p.WaitS != nil && (*p.WaitS < 5 || *p.WaitS > 240) {
		return fmt.Sprintf("wait_s %d: use 5-240", *p.WaitS)
	}
	return ""
}
