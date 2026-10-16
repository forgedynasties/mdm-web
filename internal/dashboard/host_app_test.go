package dashboard

import (
	"encoding/json"
	"testing"
)

func TestHostAppView(t *testing.T) {
	// AT070AABU00064 on live, 16 Oct (client 1.11.0), plus a second variant that crashed.
	extra := json.RawMessage(`{"timezone":"Asia/Karachi","host_app":{"package":"aio.app.nugget.uatv2","pattern":"aio.app.nugget*","variants":[
	 {"pss_mb":77,"enabled":true,"package":"aio.app.nugget.uatv2","running":true,"exits_24h":{"other":2,"user_requested":2},"installer":"com.google.android.packageinstaller","last_exit":{"at":1792139124699,"reason":"other","description":"[TOO MANY EMPTY PROCS] too many empty"},"foreground":false,"updated_at":1791609197209,"rx_today_kb":4700,"tx_today_kb":700,"fg_today_min":67,"installed_at":1281667,"last_used_at":1792145280000,"version_code":47,"version_name":"47.0.0-uatv2"},
	 {"package":"aio.app.nugget","version_name":"17.0.0","version_code":17,"exits_24h":{"crash":2,"anr":1}}]}}`)
	v := hostAppView(extra)
	if v == nil {
		t.Fatal("nil view")
	}
	if v.Active.Env != "UAT v2" || v.State != "Running" || v.Memory != "77 MB" || v.Today != "1 h 07 m" {
		t.Errorf("active: %+v state=%q mem=%q today=%q", v.Active, v.State, v.Memory, v.Today)
	}
	if v.Problems != 0 || v.LastWhy != "trimmed by Android" || v.Network != "4.7 MB ↓ · 700 KB ↑" {
		t.Errorf("problems=%d why=%q net=%q", v.Problems, v.LastWhy, v.Network)
	}
	if v.Installed != "at first boot · by hand" {
		t.Errorf("installed=%q", v.Installed)
	}
	if len(v.Others) != 1 || v.Others[0].Env != "Production" || v.Others[0].Note != "3 crashes" || v.Others[0].NoteTone != "danger" {
		t.Errorf("others: %+v", v.Others)
	}
	// MDM-lite's host_app has no variants: no card.
	if hostAppView(json.RawMessage(`{"host_app":{"package":"x","version_name":"1"}}`)) != nil {
		t.Error("MDM-lite host_app drew a card")
	}
}
