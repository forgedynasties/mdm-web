package ai

import "testing"

// The model (or a gateway) occasionally wraps the report JSON in stray text;
// ParseReport must still find the object.
func TestParseReportSalvage(t *testing.T) {
	cases := []string{
		`{"status":"ok","headline":"h"}`,
		"{\n{\"status\":\"ok\",\"headline\":\"h\"}",
		"Here you go:\n{\"status\":\"ok\",\"headline\":\"h\"}\nthanks",
		"```json\n{\"status\":\"ok\",\"headline\":\"h\"}\n```",
		"{\"note\":1}\n{\"status\":\"ok\",\"headline\":\"h\"}",
	}
	for _, in := range cases {
		r, ok := ParseReport(in)
		if !ok || r.Headline != "h" {
			t.Fatalf("failed to salvage %q -> %+v ok=%v", in, r, ok)
		}
	}
	if _, ok := ParseReport("{ nope"); ok {
		t.Fatal("accepted junk")
	}
	if _, ok := ParseReport("plain prose summary"); ok {
		t.Fatal("accepted prose")
	}
}
