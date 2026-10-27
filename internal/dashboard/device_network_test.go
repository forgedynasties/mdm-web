package dashboard

import (
	"encoding/json"
	"testing"

	"mdm/internal/db"
)

// The Network card leads with the connection, IP included (27 Oct 2026).
func TestWifiConnOf(t *testing.T) {
	d := &db.Device{LatestExtra: json.RawMessage(`{"ip_address":"192.168.100.106","wifi_band":"5","wifi_link_mbps":180,"wifi_bssid":"a4:2b:b0:11:22:33"}`)}
	c := wifiConnOf(d, "UniFi - HW Team", true, -46)
	if c.Network != "UniFi - HW Team" || c.Signal != "-46 dBm · strong" || c.Link != "5 GHz · 180 Mb/s" || c.IP != "192.168.100.106" || c.BSSID != "a4:2b:b0:11:22:33" {
		t.Errorf("conn = %+v", c)
	}
	// Details from another network than the one it is on now are left out; the IP stays.
	if c := wifiConnOf(d, "AIO-Office", false, 0); c.Signal != "" || c.Link != "" || c.IP == "" {
		t.Errorf("stale conn = %+v", c)
	}
}
