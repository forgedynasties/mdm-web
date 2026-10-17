package dashboard

import (
	"net/url"
	"strconv"
	"testing"
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
