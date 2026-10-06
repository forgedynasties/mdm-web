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
		got := classifySerial(c.serial, c.dev, ps, nil)
		if got.Class != c.want {
			t.Errorf("%s: class %q, want %q", c.name, got.Class, c.want)
		}
	}
	if got := classifySerial("AT070AABU00875", nil, ps, nil); got.Production != "T7 batch BU" || got.Model != "07" {
		t.Errorf("production details missing: %+v", got)
	}
	// A fleet device that also sits in a batch reports the batch too.
	if got := classifySerial("AT070AABU00231", &db.Device{EnrollmentStatus: "auto"}, ps, nil); got.Class != "fleet" || got.Production != "T7 batch BU" {
		t.Errorf("fleet+batch: %+v", got)
	}
	// No productions defined at all: nothing can look like ours.
	if got := classifySerial("AT070AABU00875", nil, nil, nil); got.Class != "other" {
		t.Errorf("no productions: %+v", got)
	}
}

func sample(serial, class, mfr, model string) db.FamilySample {
	return db.FamilySample{Serial: serial, DeviceClass: class, Manufacturer: mfr, Model: model}
}

func TestBuildFamiliesLearnsSchemeAndClass(t *testing.T) {
	fs := buildFamilies([]db.FamilySample{
		sample("DK19248T41010", "kds", "SUNMI", "D2s_KDS_STGL"),
		sample("DK19248T41022", "kds", "SUNMI", "D2s_KDS_STGL"),
		sample("DK19248T41033", "kds", "SUNMI", "D2s_KDS_STGL"),
		sample("D3P20230411", "pos", "SUNMI", "D3 Pro"),                // single device: trusted for half
		sample("android-8f3a", "", "Acme", "Thing"),                     // fabricated: ignored
		sample("msm-123456", "", "Acme", "Thing"),                       // corrupt: ignored
		sample("AB", "", "Tiny", "Short"),                               // prefix too short to say anything
		sample("ZZ12345678", "", "NoModel", ""),                         // no model: cannot group
	})
	if len(fs) != 2 {
		t.Fatalf("want 2 families, got %d: %+v", len(fs), fs)
	}
	kds := matchFamily("DK19248T41099", fs)
	if kds == nil || kds.Label != "SUNMI D2s_KDS_STGL" || kds.DeviceClass != "kds" || kds.Count != 3 {
		t.Fatalf("D2s family not learned: %+v", kds)
	}
	if kds.Prefix != "DK19248T410" {
		t.Errorf("prefix %q, want the serials' common prefix", kds.Prefix)
	}
	if matchFamily("DK19248T4109", fs) != nil {
		t.Errorf("a serial of another length must not match the family")
	}
	if matchFamily("XX19248T41099", fs) != nil {
		t.Errorf("a different prefix must not match")
	}
	pos := matchFamily("D3P20239999", fs)
	if pos == nil || pos.DeviceClass != "pos" {
		t.Errorf("single-device family should still recognise its model: %+v", pos)
	}
}

func TestClassifyPrefersFleetThenProductionThenFamily(t *testing.T) {
	fs := []serialFamily{{Label: "SUNMI D2s", Prefix: "DK1924", Length: 13, DeviceClass: "kds", Count: 3}}
	ps := []db.Production{prodT7()}
	if got := classifySerial("DK19248T41099", nil, ps, fs); got.Class != "family" || got.DevClass != "kds" || got.Family != "SUNMI D2s" {
		t.Errorf("family: %+v", got)
	}
	if got := classifySerial("DK19248T41099", &db.Device{EnrollmentStatus: "auto", DeviceClass: "kds"}, ps, fs); got.Class != "fleet" {
		t.Errorf("already in the MDM must beat a family guess: %+v", got)
	}
	if got := classifySerial("AT070AABU00875", nil, ps, fs); got.Class != "production" {
		t.Errorf("a production batch must beat a family guess: %+v", got)
	}
	if got := classifySerial("18121FDF60022T", nil, ps, fs); got.Class != "other" {
		t.Errorf("unrelated phone: %+v", got)
	}
}

func TestDeviceTitle(t *testing.T) {
	mk := func(extra string) *db.Device { return &db.Device{LatestExtra: []byte(extra)} }
	cases := []struct{ extra, want string }{
		{`{"manufacturer":"AIO","model":"T7"}`, "AIO T7"},
		{`{"manufacturer":"SUNMI","model":"SUNMI D2s"}`, "SUNMI D2s"}, // not "SUNMI SUNMI D2s"
		{`{"model":"Pixel 6"}`, "Pixel 6"},
		{`{"manufacturer":"AIO"}`, ""},
		{``, ""},
	}
	for _, c := range cases {
		if got := deviceTitle(mk(c.extra)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.extra, got, c.want)
		}
	}
	got := classifySerial("AT070AA2600030", &db.Device{EnrollmentStatus: "auto", DeviceClass: "t7", LatestExtra: []byte(`{"manufacturer":"AIO","model":"T7"}`)}, nil, nil)
	if got.Class != "fleet" || got.Name != "AIO T7" || got.DevClass != "t7" {
		t.Errorf("fleet answer should carry model and class: %+v", got)
	}
}
