package dashboard

import "testing"

func TestLowerFirst(t *testing.T) {
	for in, want := range map[string]string{
		"USB debugging is on":      "USB debugging is on",
		"Developer options are on": "developer options are on",
		"":                         "",
		"X":                        "x",
	} {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}
