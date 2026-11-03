package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// IslandJobs: GET /island/jobs, the top island's progress slot (D3, picked 3 Nov 2026): the
// commands this person sent in the last six hours that are still running somewhere, the
// newest first. Polled by layout.html while the tab is open; a short command (a reboot of
// one device) usually never shows.
func (h *Handler) IslandJobs(w http.ResponseWriter, r *http.Request) {
	type job struct {
		Label  string `json:"label"`
		Done   int    `json:"done"`
		Total  int    `json:"total"`
		Failed int    `json:"failed"`
		Href   string `json:"href"`
	}
	out := struct {
		Jobs []job `json:"jobs"`
	}{Jobs: []job{}}
	if user := h.currentUsername(r); user != "" {
		js, err := h.db.RunningJobsBy(r.Context(), user, 3)
		if err == nil {
			for _, j := range js {
				// Quiet for the first few seconds: a one-device command is usually done by then.
				if j.Total == 1 && time.Since(j.CreatedAt) < 8*time.Second {
					continue
				}
				label := cmdTypeLabel(j.Type)
				if j.Total > 1 {
					label = fmt.Sprintf("%s · %d devices", label, j.Total)
				}
				out.Jobs = append(out.Jobs, job{Label: label, Done: j.Done, Total: j.Total, Failed: j.Failed, Href: "/commands/" + j.ID.String()})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
