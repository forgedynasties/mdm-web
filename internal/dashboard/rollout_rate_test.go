package dashboard

import "testing"

func TestRolloutBatch(t *testing.T) {
	for _, c := range []struct{ online, floor, ceil, want int }{
		{0, 20, 250, 20},      // no sockets: the old fixed cap
		{200, 20, 250, 20},    // today's fleet: unchanged
		{2500, 20, 250, 50},   // 2 %
		{5000, 20, 250, 100},  // a 5,000 fleet rolls over in under an hour
		{50000, 20, 250, 250}, // ceiling
		{5000, 10, 100, 100},  // firmware ceiling
		{101, 1, 250, 3},      // rounds up
	} {
		if got := rolloutBatch(c.online, c.floor, c.ceil); got != c.want {
			t.Errorf("rolloutBatch(%d, %d, %d) = %d, want %d", c.online, c.floor, c.ceil, got, c.want)
		}
	}
}
