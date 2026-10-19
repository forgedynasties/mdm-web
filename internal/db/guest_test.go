package db

import (
	"encoding/json"
	"testing"
)

func TestGuestFrame(t *testing.T) {
	g := GuestInfo{RestaurantName: "Diner"}
	var msg struct {
		Type  string         `json:"type"`
		Guest map[string]any `json:"guest"`
	}
	if err := json.Unmarshal(g.Frame(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "guest" || msg.Guest["restaurant_name"] != "Diner" {
		t.Errorf("frame = %+v", msg)
	}
	// Guest Wi-Fi and the guest app were removed (19 Oct), the table label earlier (14
	// Oct): none is sent, and the client reads a missing one as cleared.
	for _, k := range []string{"wifi_ssid", "wifi_password", "wifi_security", "guest_app_package", "table_label"} {
		if _, ok := msg.Guest[k]; ok {
			t.Errorf("guest.%s still sent", k)
		}
	}
}
