package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
)

// WrappedPage renders "Fleet Wrapped" — a playful, full-screen year-in-review of
// the whole fleet (Spotify-Wrapped style). Open to any signed-in role.
func (h *Handler) WrappedPage(w http.ResponseWriter, r *http.Request) {
	me, _ := h.db.UserStats(r.Context(), h.currentUsername(r), legacyAdminActor)
	topUsers, _ := h.db.TopActors(r.Context(), 5, legacyAdminActor) // the old built-in login is not a person on the team
	wr, err := h.db.GetFleetWrapped(r.Context())
	if err != nil {
		log.Printf("[wrapped] compute: %v", err)
	}
	h.render(w, r, "wrapped.html", map[string]any{
		"Title":      "Fleet Wrapped",
		"W":          wr,
		"Me":         me,
		"TopUsers":   topUsers,
		"OnlineDays": wr.OnlineMinutes / 1440,
		"WorkerDays": int(wr.HardestWorker.Value) / 1440,
	})
}
