package dashboard

import "testing"

// The "did you mean" rows keep serials within two edits: a cut-off paste (one missing
// character) or a typo must qualify, an unrelated serial must not.
func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"AT070AABU0028", "AT070AABU00280", 1},  // cut-off paste
		{"AT070AABU00208", "AT070AABU00280", 2}, // swapped digits
		{"AT070AABU00280", "AT070AABU00280", 0},
		{"AT070AABU00280", "AT070AA2600030", 4},
		{"", "abc", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
