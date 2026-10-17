package dashboard

import "testing"

func TestLostModePhone(t *testing.T) {
	ok := map[string]string{"": "", "+1 415 555 0100": "+1 415 555 0100", "  (415)  555-0100 ": "(415) 555-0100"}
	for in, want := range ok {
		if got, err := lostModePhone(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"call us", "12345", "+1 415 555 0100 <script>", "1234567890123456789012345678901234567890123"} {
		if _, err := lostModePhone(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}
