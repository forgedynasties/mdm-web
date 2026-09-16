package dashboard

import "testing"

func TestCleanSerialToken(t *testing.T) {
	cases := map[string]string{
		"-AT070AABU00333":   "AT070AABU00333",
		"  - AT070AABU00230": "AT070AABU00230",
		"• AT070AABU00480":  "AT070AABU00480",
		"1. AT070AABU00455": "AT070AABU00455",
		"2) AT070AABU00148": "AT070AABU00148",
		"\"AT070AABU00104\",": "AT070AABU00104",
		"AT070AABU00460;":   "AT070AABU00460",
		"AT070AABU00056":    "AT070AABU00056",
		"com.skorra.agent":  "com.skorra.agent",
		"1.2.3":             "1.2.3",
		"—AT070AABU00646":   "AT070AABU00646",
	}
	for in, want := range cases {
		if got := cleanSerialToken(in); got != want {
			t.Errorf("cleanSerialToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSerialsFieldPastedList(t *testing.T) {
	blob := "-AT070AABU00333\n-AT070AABU00230\n-AT070AABU00480\n-AT070AABU00455\n-AT070AABU00148"
	got := parseSerialsField([]string{blob})
	if len(got) != 5 {
		t.Fatalf("got %d serials: %v", len(got), got)
	}
	for _, s := range got {
		if len(s) != 14 || s[:5] != "AT070" {
			t.Errorf("bad serial %q", s)
		}
	}
}
