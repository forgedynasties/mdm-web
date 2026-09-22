package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"mdm/internal/config"
)

// Peer administration over the admin API. Deliberately not a dashboard form yet: the
// shape of a peer (name, URL, dashboard URL, key) is settled by whoever runs the two
// servers, and it is a one-line curl per environment rather than a page anyone visits
// twice. The peer key is write-only here — it is never read back, so a leaked listing
// cannot become a leaked secret.

type peerView struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	DashboardURL string `json:"dashboard_url,omitempty"`
	Enabled      bool   `json:"enabled"`
	HasKey       bool   `json:"has_key"`
}

// ListPeers shows what this server considers its neighbours, without their keys.
func (h *Handler) ListPeers(w http.ResponseWriter, r *http.Request) {
	out := []peerView{}
	for _, p := range h.cfg.Peers(false) {
		out = append(out, peerView{Name: p.Name, URL: p.URL, DashboardURL: p.DashboardURL,
			Enabled: p.Enabled, HasKey: p.Key != ""})
	}
	writeJSON(w, http.StatusOK, map[string]any{"self": h.cfg.SelfName(), "peers": out})
}

// SetPeers replaces the peer list, and optionally renames this server. Replacing rather
// than merging is the safer shape for a list this small: what you send is what runs,
// and there is no half-state where a peer someone meant to remove is still trusted.
func (h *Handler) SetPeers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Self  string        `json:"self"`
		Peers []config.Peer `json:"peers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	for i := range body.Peers {
		p := &body.Peers[i]
		p.Name = strings.TrimSpace(p.Name)
		p.URL = strings.TrimRight(strings.TrimSpace(p.URL), "/")
		p.DashboardURL = strings.TrimRight(strings.TrimSpace(p.DashboardURL), "/")
		p.Key = strings.TrimSpace(p.Key)
		if p.Name == "" || p.URL == "" || p.Key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "each peer needs a name, a url and a key"})
			return
		}
		if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "peer url must be http(s)"})
			return
		}
		// A short key is not a key. This one authenticates another server's claims
		// about where our devices are.
		if len(p.Key) < 24 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "peer key must be at least 24 characters"})
			return
		}
	}
	if s := strings.TrimSpace(body.Self); s != "" {
		if err := h.cfg.SetSelfName(s); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save name"})
			return
		}
	}
	if err := h.cfg.SetPeers(body.Peers); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save peers"})
		return
	}
	names := make([]string, 0, len(body.Peers))
	for _, p := range body.Peers {
		names = append(names, p.Name)
	}
	_ = h.db.InsertAudit(r.Context(), "api", "peers.set", h.cfg.SelfName(), strings.Join(names, " "))
	writeJSON(w, http.StatusOK, map[string]any{"self": h.cfg.SelfName(), "peers": len(body.Peers)})
}

// PingPeers calls every configured peer and reports what answered — the check to run
// straight after configuring both sides, so a typo in a key is found now rather than
// the next time a device moves.
func (h *Handler) PingPeers(w http.ResponseWriter, r *http.Request) {
	type result struct {
		Name    string `json:"name"`
		OK      bool   `json:"ok"`
		Replied string `json:"replied,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	ctx, cancel := timeoutCtx(r, 20*time.Second)
	defer cancel()
	out := []result{}
	for _, p := range h.cfg.Peers(true) {
		name, err := h.peers.Ping(ctx, p)
		if err != nil {
			out = append(out, result{Name: p.Name, Error: err.Error()})
			continue
		}
		out = append(out, result{Name: p.Name, OK: true, Replied: name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"self": h.cfg.SelfName(), "results": out})
}

// SweepPeers runs the reconciliation pass now instead of waiting for its loop, so a
// freshly configured pair converges while someone is watching.
func (h *Handler) SweepPeers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutCtx(r, 60*time.Second)
	defer cancel()
	n, err := h.peers.Sweep(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"filed": n})
}
