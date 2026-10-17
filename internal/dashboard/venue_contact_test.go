package dashboard

import "testing"

func TestVenueContactEmail(t *testing.T) {
	ok := map[string]string{"": "", "  ": "", "owner@nugget.com": "owner@nugget.com", " a.b@c.co ": "a.b@c.co"}
	for in, want := range ok {
		if got, err := venueContactEmail(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"not an email", "Owner <owner@nugget.com>", "a@b.com, c@d.com", "a@b.com;c@d.com", "@nugget.com"} {
		if _, err := venueContactEmail(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}
