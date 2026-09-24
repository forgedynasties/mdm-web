package api

import "testing"

func TestValidSerial(t *testing.T) {
	for _, s := range []string{"AT070AABU00429", "AT070AA2600037", "DP02253X11001", "android-9f1c2e3d4b5a6978",
		"msm-9f1c2e3d4b5a6978", "2df2f2c2", "R92X205MPKR"} {
		if !validSerial(s) {
			t.Errorf("%q should be a valid serial", s)
		}
	}
	for _, s := range []string{"androidboot.baseband=msm", "", "AT070 AABU00429", "a/b", "x=y", "serial."} {
		if validSerial(s) {
			t.Errorf("%q should be refused", s)
		}
	}
}
