package product

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ── lost_mode and wifi_config payloads ────────────────────────────────────────
//
// Both clients build against one contract (17 Oct): checked here once, so every send
// path — the device page, bulk actions, the admin API — refuses the same bad payload at
// the door instead of as a failed delivery, and stores it in the same normalised shape.

const (
	LostModeMessageMax = 280
	LostModePhoneMax   = 40
	WifiSSIDMax        = 32 // bytes, as 802.11 counts them
)

// LostModePayload is {"on": true, "message": "…", "phone": "…"} or {"on": false}.
type LostModePayload struct {
	On      bool   `json:"on"`
	Message string `json:"message,omitempty"`
	Phone   string `json:"phone,omitempty"`
	// Beacon is the Bluetooth LE token for this lost session (18 Oct). The server sets it
	// when the command is created (db.CreateCommandBy); one a caller sends is dropped.
	Beacon string `json:"beacon,omitempty"`
}

// NormalizeLostMode checks a lost_mode payload and returns it in its stored shape
// (trimmed; message and phone dropped when turning it off). msg is "" when it may be sent.
func NormalizeLostMode(payload json.RawMessage) (json.RawMessage, string) {
	var p struct {
		On      *bool  `json:"on"`
		Message string `json:"message"`
		Phone   string `json:"phone"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &p) != nil || p.On == nil {
		return nil, `payload must be {"on": true|false, "message": "…", "phone": "…"}`
	}
	out := LostModePayload{On: *p.On}
	if out.On {
		out.Message = strings.TrimSpace(p.Message)
		out.Phone = strings.TrimSpace(p.Phone)
		if utf8.RuneCountInString(out.Message) > LostModeMessageMax {
			return nil, fmt.Sprintf("message is longer than %d characters", LostModeMessageMax)
		}
		if utf8.RuneCountInString(out.Phone) > LostModePhoneMax {
			return nil, fmt.Sprintf("phone is longer than %d characters", LostModePhoneMax)
		}
	}
	b, _ := json.Marshal(out)
	return b, ""
}

// NormalizeWifiConfig checks a wifi_config payload as the caller sent it (password
// included) and returns it normalised, password still in. Split it with
// SplitCommandSecret before storing.
func NormalizeWifiConfig(payload json.RawMessage) (json.RawMessage, string) {
	var p struct {
		Action   string `json:"action"`
		SSID     string `json:"ssid"`
		Security string `json:"security"`
		Password string `json:"password"`
		Hidden   bool   `json:"hidden"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &p) != nil {
		return nil, `payload must be {"action": "add"|"remove", "ssid": "…", "security": "open"|"wpa2"|"wpa3", "password": "…"}`
	}
	action := strings.ToLower(strings.TrimSpace(p.Action))
	ssid := p.SSID // an SSID's spaces are its own; only an all-blank one is refused
	if strings.TrimSpace(ssid) == "" {
		return nil, "ssid is required"
	}
	if len(ssid) > WifiSSIDMax {
		return nil, fmt.Sprintf("ssid is longer than %d bytes", WifiSSIDMax)
	}
	switch action {
	case "remove":
		b, _ := json.Marshal(map[string]string{"action": "remove", "ssid": ssid})
		return b, ""
	case "add":
	default:
		return nil, `action must be "add" or "remove"`
	}
	sec := strings.ToLower(strings.TrimSpace(p.Security))
	switch sec {
	case "open":
		if p.Password != "" {
			return nil, "an open network takes no password"
		}
	case "wpa2", "wpa3":
		if n := len(p.Password); n < 8 || n > 63 {
			return nil, "password must be 8 to 63 characters"
		}
	default:
		return nil, `security must be "open", "wpa2" or "wpa3"`
	}
	m := map[string]any{"action": "add", "ssid": ssid, "security": sec, "hidden": p.Hidden}
	if p.Password != "" {
		m["password"] = p.Password
	}
	b, _ := json.Marshal(m)
	return b, ""
}

// NormalizeCommandPayload runs the check for the types that have one. Other types come
// back unchanged with no message.
func NormalizeCommandPayload(cmdType string, payload json.RawMessage) (json.RawMessage, string) {
	switch cmdType {
	case "lost_mode":
		return NormalizeLostMode(payload)
	case "wifi_config":
		return NormalizeWifiConfig(payload)
	}
	return payload, ""
}

// secretKeys are the payload fields that must never be stored with the command: they
// go to command_secrets and are joined back in only on the way to the device.
var secretKeys = map[string][]string{
	"wifi_config": {"password"},
}

// CommandHasSecret reports whether a type can carry fields kept out of the commands table.
func CommandHasSecret(cmdType string) bool { return len(secretKeys[cmdType]) > 0 }

// SplitCommandSecret separates a payload into what is stored with the command (and so
// shown in every view of it) and the secret fields, which are not. secret is nil when
// there are none. A wifi_config's public half says password_set instead.
func SplitCommandSecret(cmdType string, payload json.RawMessage) (public, secret json.RawMessage) {
	keys := secretKeys[cmdType]
	if len(keys) == 0 || len(payload) == 0 {
		return payload, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return payload, nil
	}
	sec := map[string]json.RawMessage{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			sec[k] = v
			delete(m, k)
		}
	}
	if len(sec) == 0 {
		return payload, nil
	}
	if cmdType == "wifi_config" {
		m["password_set"] = json.RawMessage("true")
	}
	public, _ = json.Marshal(m)
	secret, _ = json.Marshal(sec)
	return public, secret
}

// MergeCommandSecret puts the secret fields back for delivery to the device. The
// password_set marker is the dashboard's, not the client's, so it goes.
func MergeCommandSecret(cmdType string, public, secret json.RawMessage) json.RawMessage {
	if len(secret) == 0 {
		return public
	}
	var m, sec map[string]json.RawMessage
	if json.Unmarshal(public, &m) != nil || json.Unmarshal(secret, &sec) != nil {
		return public
	}
	for k, v := range sec {
		m[k] = v
	}
	if cmdType == "wifi_config" {
		delete(m, "password_set")
	}
	b, _ := json.Marshal(m)
	return b
}
