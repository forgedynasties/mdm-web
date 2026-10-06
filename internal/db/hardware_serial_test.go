package db

import "testing"

func TestNormalizeHardwareSerial(t *testing.T) {
	for in, want := range map[string]string{
		"C45BCE30": "C45BCE30", " c45bce30\n": "C45BCE30", "0xC45BCE30": "C45BCE30",
		"": "", "0": "", "00000000": "", "xyz": "", "C45": "", "123456789ABCDEF01": "",
	} {
		if got := NormalizeHardwareSerial(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
