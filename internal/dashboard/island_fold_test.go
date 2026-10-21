package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestFoldedHeadersHideTheirText: a page that folds its header to zero height under the
// island (height:0;overflow:visible) must hide the header's text as well, or the name and
// counts show through the content below (venue page, 21 Oct 2026).
func TestFoldedHeadersHideTheirText(t *testing.T) {
	fold := regexp.MustCompile(`body\.has-island \.([a-z-]+)\{[^}]*height:0`)
	for _, f := range []string{"restaurant_detail.html", "group_detail.html"} {
		b, err := os.ReadFile("../../templates/" + f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, m := range fold.FindAllStringSubmatch(src, -1) {
			i := strings.Index(src, `class="cc-top `+m[1]+`"`)
			if i < 0 {
				t.Errorf("%s: no header with class %s", f, m[1])
				continue
			}
			head := src[i:]
			if j := strings.Index(head, `<div class="cc-actions">`); j > 0 {
				head = head[:j]
			}
			if !strings.Contains(head, `class="isl-dup-sr"`) {
				t.Errorf("%s: .%s folds to zero height under the island but its name and counts are not .isl-dup-sr", f, m[1])
			}
		}
	}
}
