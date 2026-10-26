package product

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeLostMode(t *testing.T) {
	ok := []string{
		`{"on": true, "message": " Please hand it to staff. ", "phone": "+1 415 555 0100"}`,
		`{"on": true}`,
		`{"on": false, "message": "dropped"}`,
	}
	for _, in := range ok {
		if _, msg := NormalizeLostMode(json.RawMessage(in)); msg != "" {
			t.Errorf("%s: refused: %s", in, msg)
		}
	}
	bad := []string{``, `{}`, `{"on": "yes"}`, `{"on": true, "message": "` + strings.Repeat("x", 281) + `"}`,
		`{"on": true, "phone": "` + strings.Repeat("1", 41) + `"}`}
	for _, in := range bad {
		if _, msg := NormalizeLostMode(json.RawMessage(in)); msg == "" {
			t.Errorf("%s: accepted", in)
		}
	}
	out, _ := NormalizeLostMode(json.RawMessage(`{"on": false, "message": "dropped"}`))
	if string(out) != `{"on":false}` {
		t.Errorf("off payload = %s, want only on:false", out)
	}
}

func TestNormalizeWifiConfig(t *testing.T) {
	ok := []string{
		`{"action": "add", "ssid": "Nugget-Staff", "security": "wpa2", "password": "correcthorse"}`,
		`{"action": "add", "ssid": "Guest", "security": "open"}`,
		`{"action": "remove", "ssid": "Nugget-Old"}`,
		`{"action": "export"}`,
	}
	for _, in := range ok {
		if _, msg := NormalizeWifiConfig(json.RawMessage(in)); msg != "" {
			t.Errorf("%s: refused: %s", in, msg)
		}
	}
	bad := []string{
		`{"action": "add", "ssid": "", "security": "open"}`,
		`{"action": "add", "ssid": "X", "security": "wpa2", "password": "short"}`,
		`{"action": "add", "ssid": "X", "security": "open", "password": "notallowedhere"}`,
		`{"action": "add", "ssid": "X", "security": "wep", "password": "12345678"}`,
		`{"action": "join", "ssid": "X"}`,
		`{"action": "add", "ssid": "` + strings.Repeat("s", 33) + `", "security": "open"}`,
	}
	for _, in := range bad {
		if _, msg := NormalizeWifiConfig(json.RawMessage(in)); msg == "" {
			t.Errorf("%s: accepted", in)
		}
	}
}

// The password must never be in the stored half, and must come back for delivery.
func TestWifiSecretSplit(t *testing.T) {
	in, _ := NormalizeWifiConfig(json.RawMessage(`{"action": "add", "ssid": "Nugget-Staff", "security": "wpa3", "password": "correcthorse"}`))
	public, secret := SplitCommandSecret("wifi_config", in)
	if strings.Contains(string(public), "correcthorse") || strings.Contains(string(public), `"password"`) {
		t.Fatalf("stored payload carries the password: %s", public)
	}
	if !strings.Contains(string(public), `"password_set":true`) {
		t.Errorf("stored payload does not say a password is set: %s", public)
	}
	if !strings.Contains(string(secret), "correcthorse") {
		t.Fatalf("secret lost the password: %s", secret)
	}
	merged := MergeCommandSecret("wifi_config", public, secret)
	var m map[string]any
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatal(err)
	}
	if m["password"] != "correcthorse" || m["ssid"] != "Nugget-Staff" || m["security"] != "wpa3" {
		t.Errorf("delivered payload = %s", merged)
	}
	if _, ok := m["password_set"]; ok {
		t.Errorf("delivered payload keeps the dashboard's marker: %s", merged)
	}
	// An open network has no secret, and other types are untouched.
	open, _ := NormalizeWifiConfig(json.RawMessage(`{"action": "add", "ssid": "Guest", "security": "open"}`))
	if _, s := SplitCommandSecret("wifi_config", open); s != nil {
		t.Errorf("open network produced a secret: %s", s)
	}
	if _, s := SplitCommandSecret("shell", json.RawMessage(`{"password":"x"}`)); s != nil {
		t.Errorf("shell payload was split")
	}
}
