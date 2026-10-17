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

// ChangelogLatest renders just the newest release as an HTML fragment for the
// top-bar "What's new" popup (lazy-loaded via htmx when the popup opens).
func (h *Handler) ChangelogLatest(w http.ResponseWriter, r *http.Request) {
	var latest *version.Entry
	if len(version.Changelog) > 0 {
		latest = &version.Changelog[0]
	}
	h.render(w, r, "changelog_latest.html", map[string]any{
		"Entry":   latest,
		"Version": version.Current(),
	})
}
