package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"net/http"

	"mdm/internal/version"
)

// Changelog renders the "What's new" page from the in-binary version.Changelog.
func (h *Handler) Changelog(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "changelog.html", map[string]any{
		"Title":     "What's new",
		"Changelog": version.Changelog,
	})
}
