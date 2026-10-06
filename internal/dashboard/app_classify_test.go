package dashboard

import (
	"testing"

	"mdm/internal/db"
)

func prodT7() db.Production {
	return db.Production{Name: "T7 batch BU", ProductCode: "AT", ModelCode: "07", Variant: "0", SKU: "AA", Batch: "BU", StartSequence: 1, EndSequence: 1000}
}

func TestSerialMatchesProduction(t *testing.T) {
	p := prodT7()
	cases := []struct {
		serial string
		want   bool
	}{
		{"AT070AABU00231", true},   // inside the range
		{"AT070AABU00001", true},   // first
		{"AT070AABU01000", true},   // last
		{"AT070AABU01001", false},  // one past the range
		{"AT070AABU00000", false},  // below the range
		{"AT070AA2600030", false},  // same product, different batch
		{"AT070AABU0023", false},   // 13 chars
		{"AT070AABU002311", false}, // 15 chars
		{"AT070AABUABCDE", false},  // sequence not numeric
		{"AT070AABU+0231", false},  // sign characters are not digits
		{"XX070AABU00231", false},  // other product
	}
	for _, c := range cases {
		if got := serialMatchesProduction(c.serial, p); got != c.want {
			t.Errorf("%s: got %v, want %v", c.serial, got, c.want)
		}
	}
}

func TestClassifySerial(t *testing.T) {
	ps := []db.Production{prodT7()}
	known := &db.Device{SerialNumber: "AT070AA2600030", EnrollmentStatus: "auto", DeviceClass: "t7"}
	tests := []struct {
		name   string
		serial string
		dev    *db.Device
		want   string
	}{
		{"already in the MDM", "AT070AA2600030", known, "fleet"},
		{"in a batch, not checked in yet", "AT070AABU00875", nil, "production"},
		{"our product, no batch covers it", "AT070AA2600030", nil, "lookalike"},
		{"another vendor's phone", "18121FDF60022T", nil, "other"},
		{"short junk", "abc", nil, "other"},
	}
	for _, c := range tests {
		got := classifySerial(c.serial, c.dev, ps)
		if got.Class != c.want {
			t.Errorf("%s: class %q, want %q", c.name, got.Class, c.want)
		}
	}
	if got := classifySerial("AT070AABU00875", nil, ps); got.Production != "T7 batch BU" || got.Model != "07" {
		t.Errorf("production details missing: %+v", got)
	}
	// A fleet device that also sits in a batch reports the batch too.
	if got := classifySerial("AT070AABU00231", &db.Device{EnrollmentStatus: "auto"}, ps); got.Class != "fleet" || got.Production != "T7 batch BU" {
		t.Errorf("fleet+batch: %+v", got)
	}
	// No productions defined at all: nothing can look like ours.
	if got := classifySerial("AT070AABU00875", nil, nil); got.Class != "other" {
		t.Errorf("no productions: %+v", got)
	}
}
