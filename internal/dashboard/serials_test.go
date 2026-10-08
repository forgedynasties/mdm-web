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
		"aio.app.mdmclient.dpc":  "aio.app.mdmclient.dpc",
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

// A pasted list arrives in whatever shape the operator copied it: one per line,
// space separated out of a spreadsheet row, tab separated, semicolons, repeats.
// All of it must come back as individual serials — the group add used to split on
// newlines and commas only and silently matched nothing for the rest.
func TestParseSerialsFieldSeparators(t *testing.T) {
	cases := map[string]int{
		"AT070AABU00269 AT070AABU00182 AT070AABU00376":   3,
		"AT070AABU00269\tAT070AABU00182":                 2,
		"AT070AABU00269;AT070AABU00182; AT070AABU00376":  3,
		"AT070AABU00269\r\n\r\nAT070AABU00182":           2,
		"AT070AABU00269, AT070AABU00269, AT070AABU00182": 2, // dedup
		"at070aabu00269 AT070AABU00269":                  1, // dedup is case-insensitive
	}
	for in, want := range cases {
		if got := parseSerialsField([]string{in}); len(got) != want {
			t.Errorf("parseSerialsField(%q) = %v (%d), want %d", in, got, len(got), want)
		}
	}
}
