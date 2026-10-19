package dashboard

import (
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestAdbSerialsFrom(t *testing.T) {
	v := url.Values{"serial": {"A", " B ", "A"}, "serials": {"C, B,,D"}}
	got := adbSerialsFrom(v)
	want := []string{"A", "B", "C", "D"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	many := url.Values{}
	for i := 0; i < 150; i++ {
		many.Add("serial", "S"+strconv.Itoa(i))
	}
	if n := len(adbSerialsFrom(many)); n != 100 {
		t.Fatalf("capped at 100, got %d", n)
	}
}

func TestAdbRequestOver(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		status   string
		hours    int
		finished time.Time
		want     bool
	}{
		{"completed", 2, now.AddDate(0, 0, -12), true},     // the 7 Oct request on 030
		{"completed", 2, now.Add(-2 * time.Minute), false}, // just done: give it time to report
		{"completed", 0, now.Add(-30 * time.Minute), true}, // "until off", yet not listening
		{"failed", 24, now.Add(-time.Hour), true},
		{"failed", 24, now.Add(-time.Minute), false},
		{"pending", 2, now.AddDate(0, 0, -1), false}, // still on its way
		{"received", 0, now.Add(-time.Hour), false},
	}
	for _, c := range cases {
		if got := adbRequestOver(c.status, c.hours, c.finished, now); got != c.want {
			t.Errorf("adbRequestOver(%s, %dh, %s ago) = %v, want %v", c.status, c.hours, now.Sub(c.finished), got, c.want)
		}
	}
}
