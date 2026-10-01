package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeGuestWifi(t *testing.T) {
	cases := []struct {
		name    string
		in      RestaurantGuestWifi
		want    RestaurantGuestWifi
		wantErr bool
	}{
		{"wpa", RestaurantGuestWifi{SSID: " Diner-Guest ", Password: "pizza1234", Security: "wpa2"},
			RestaurantGuestWifi{SSID: "Diner-Guest", Password: "pizza1234", Security: "WPA"}, false},
		{"default security is WPA", RestaurantGuestWifi{SSID: "x", Password: "12345678"},
			RestaurantGuestWifi{SSID: "x", Password: "12345678", Security: "WPA"}, false},
		{"no password is open", RestaurantGuestWifi{SSID: "Free", Security: "WPA"},
			RestaurantGuestWifi{SSID: "Free", Security: "nopass"}, false},
		{"open drops a password", RestaurantGuestWifi{SSID: "Free", Password: "leftover", Security: "open"},
			RestaurantGuestWifi{SSID: "Free", Security: "nopass"}, false},
		{"no network clears it", RestaurantGuestWifi{Password: "pizza1234", Security: "WEP", AppPackage: "aio.app.nugget"},
			RestaurantGuestWifi{Security: "WPA", AppPackage: "aio.app.nugget"}, false},
		{"wep", RestaurantGuestWifi{SSID: "Old", Password: "abcde", Security: "wep"},
			RestaurantGuestWifi{SSID: "Old", Password: "abcde", Security: "WEP"}, false},
		{"64 hex psk", RestaurantGuestWifi{SSID: "Hex", Password: strings.Repeat("ab", 32)},
			RestaurantGuestWifi{SSID: "Hex", Password: strings.Repeat("ab", 32), Security: "WPA"}, false},
		{"short wpa password", RestaurantGuestWifi{SSID: "x", Password: "short"}, RestaurantGuestWifi{}, true},
		{"long ssid", RestaurantGuestWifi{SSID: strings.Repeat("s", 33)}, RestaurantGuestWifi{}, true},
		{"unknown security", RestaurantGuestWifi{SSID: "x", Password: "12345678", Security: "EAP"}, RestaurantGuestWifi{}, true},
		{"bad package", RestaurantGuestWifi{AppPackage: "not a package"}, RestaurantGuestWifi{}, true},
		{"one-part package", RestaurantGuestWifi{AppPackage: "nugget"}, RestaurantGuestWifi{}, true},
	}
	for _, c := range cases {
		got, err := NormalizeGuestWifi(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestNormalizeTableLabel(t *testing.T) {
	if got := NormalizeTableLabel("  Table 12 "); got != "Table 12" {
		t.Errorf("trim: %q", got)
	}
	long := strings.Repeat("é", MaxTableLabel+5)
	got := NormalizeTableLabel(long)
	if n := len([]rune(got)); n != MaxTableLabel {
		t.Errorf("cut to %d runes, want %d", n, MaxTableLabel)
	}
	if !strings.HasPrefix(long, got) {
		t.Errorf("cut mid-rune: %q", got)
	}
}

func TestGuestFrame(t *testing.T) {
	g := GuestInfo{RestaurantName: "Diner", TableLabel: "Table 4", WifiSSID: "Guest", WifiPassword: "p@ss;word", WifiSecurity: "WPA"}
	var msg struct {
		Type  string         `json:"type"`
		Guest map[string]any `json:"guest"`
	}
	if err := json.Unmarshal(g.Frame(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "guest" {
		t.Errorf("type = %q", msg.Type)
	}
	// Every key is sent even when empty, so a value cleared on the dashboard clears on
	// the tablet rather than being kept from an earlier frame.
	for _, k := range []string{"restaurant_name", "table_label", "wifi_ssid", "wifi_password", "wifi_security", "guest_app_package"} {
		if _, ok := msg.Guest[k]; !ok {
			t.Errorf("guest.%s missing", k)
		}
	}
	if msg.Guest["wifi_password"] != "p@ss;word" {
		t.Errorf("password = %v", msg.Guest["wifi_password"])
	}
}
