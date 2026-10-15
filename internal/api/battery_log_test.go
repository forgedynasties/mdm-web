package api

import (
	"strings"
	"testing"
)

func batteryCols(s string) map[string]int {
	m := map[string]int{}
	for i, c := range strings.Split(s, ",") {
		m[c] = i
	}
	return m
}

func TestParseBatteryRow(t *testing.T) {
	col := batteryCols("t_ms,up_s,ev,mv,pct,status,plug,temp_c,screen,ichg_ma,iin_ma,flap,wlc,wlc_on")
	s, ok := parseBatteryRow(strings.Split("1760572800000,1,b,3800,40,2,1,30.0,0,1000,2000,0,-1,-1", ","), col)
	if !ok {
		t.Fatal("a well-formed row was rejected")
	}
	if s.T.UnixMilli() != 1760572800000 || s.UpS != 1 || s.Ev != "b" {
		t.Errorf("key fields: %v %d %q", s.T, s.UpS, s.Ev)
	}
	if s.MV == nil || *s.MV != 3800 || s.Pct == nil || *s.Pct != 40 || s.Status == nil || *s.Status != 2 || s.Plug == nil || *s.Plug != 1 {
		t.Errorf("readings: %+v", s)
	}
	if s.TempC == nil || *s.TempC != 30 || s.Screen == nil || *s.Screen || s.IChgMA == nil || *s.IChgMA != 1000 || s.Flap == nil || *s.Flap {
		t.Errorf("readings: %+v", s)
	}
	if s.Wlc != nil || s.WlcOn != nil {
		t.Errorf("-1 must read as unknown (nil): wlc=%v wlc_on=%v", s.Wlc, s.WlcOn)
	}
	s, _ = parseBatteryRow(strings.Split("1760572800000,7,s,3700,39,3,0,-100,1,-1,-1,0,1,1", ","), col)
	if s.TempC != nil || s.IChgMA != nil || s.IInMA != nil || s.Screen == nil || !*s.Screen || s.Wlc == nil || *s.Wlc != 1 {
		t.Errorf("sentinels: %+v", s)
	}
	// Columns are read by name, so a client may reorder or add them.
	col2 := batteryCols("ev,up_s,t_ms,new_thing,mv")
	s, ok = parseBatteryRow(strings.Split("s,9,1760572800000,x,3650", ","), col2)
	if !ok || s.Ev != "s" || s.UpS != 9 || s.MV == nil || *s.MV != 3650 || s.Pct != nil {
		t.Errorf("by name: ok=%v %+v", ok, s)
	}
	for _, bad := range []string{"", "x,1,b", "1760572800000,1,", "1760572800000,-,b", "0,1,b", "1760572800000,1,bb"} {
		if _, ok := parseBatteryRow(strings.Split(bad, ","), col); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}
