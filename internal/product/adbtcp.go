package product

import (
	"encoding/json"
	"fmt"
)

// ValidateAdbTcp checks an adb_tcp command before it is stored. Wireless adb opens the
// device to the network (adbd still demands an authorized key), so it goes to exactly
// one named device at a time — never "all" and never a group, where one mistyped id
// would open a fleet. Returns "" when the command may be sent, else the reason.
//
// Payload: {"port": 5555, "hours": 24}. port 0 turns it off; hours 0 keeps it on until
// told otherwise. The client enforces the same bounds; checking here stops a bad
// request at the door instead of as a failed delivery.
func ValidateAdbTcp(targetType string, targets int, payload json.RawMessage) string {
	if targetType != "devices" || targets != 1 {
		return "adb_tcp goes to exactly one named device (target_type devices, one target)"
	}
	p := struct {
		Port  *int `json:"port"`
		Hours *int `json:"hours"`
	}{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &p); err != nil {
			return "payload must be {\"port\": N, \"hours\": N}"
		}
	}
	if p.Port != nil && *p.Port != 0 && (*p.Port < 1024 || *p.Port > 65535) {
		return fmt.Sprintf("port %d: use 0 (off) or 1024-65535", *p.Port)
	}
	if p.Hours != nil && (*p.Hours < 0 || *p.Hours > 24*30) {
		return fmt.Sprintf("hours %d: use 0 (stays on) to 720", *p.Hours)
	}
	return ""
}
