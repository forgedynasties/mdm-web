package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Release publishing over the admin API (X-API-Key), so a build machine can put a
// package on the fleet the moment it finishes signing it instead of someone
// retyping an S3 URL into the dashboard. These handlers call exactly what the
// dashboard forms call, including the "one active full image per release" rule —
// the API is another door onto the same room, never a second set of rules.

// releaseBody is the create-a-release payload. Version is the build id; product
// resolves through the catalog (empty means t7), and a release starts as a dev
// build only when the caller says so.
type releaseBody struct {
	Version   string `json:"version"`
	Product   string `json:"product"`
	Name      string `json:"name"`
	Changelog string `json:"changelog"`
	IsDev     *bool  `json:"is_dev"`
	// UpdateMeta lets a caller deliberately rewrite an EXISTING release's name,
	// changelog and dev flag. Off by default: a publisher that sends is_dev=true
	// on every run would otherwise pull a release someone already published back
	// into dev — invisible to operators — just by adding a second package to it.
	UpdateMeta bool `json:"update_meta"`
}

// CreateRelease is get-or-create on (version, product): a publisher that re-runs
// for the same build gets the same release back rather than an error, so the tool
// calling it does not have to ask first.
func (h *Handler) CreateRelease(w http.ResponseWriter, r *http.Request) {
	var body releaseBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	version := strings.TrimSpace(body.Version)
	if version == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version is required"})
		return
	}
	product := strings.TrimSpace(body.Product)
	existing, _ := h.db.GetReleaseByVersion(r.Context(), version, product)
	rel, err := h.db.GetOrCreateRelease(r.Context(), version, product)
	if err != nil || rel == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	created := existing == nil
	if created || body.UpdateMeta {
		if name, cl := strings.TrimSpace(body.Name), strings.TrimSpace(body.Changelog); name != "" || cl != "" {
			_ = h.db.SetReleaseMeta(r.Context(), rel.ID, name, cl)
		}
		if body.IsDev != nil {
			_ = h.db.SetReleaseDev(r.Context(), rel.ID, *body.IsDev)
		}
	}
	if created {
		_ = h.db.InsertAudit(r.Context(), "api", "release.create", version, "product="+rel.Product)
	}
	rel, _ = h.db.GetRelease(r.Context(), rel.ID)
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	// "created" tells the caller whether this run made the release or found one,
	// which is the difference between "my package is the baseline" and "I am
	// adding to someone else's release".
	writeJSON(w, code, map[string]any{"release": rel, "created": created})
}

// ListReleases is the roster a publisher picks from (and how it finds the id of a
// release it created earlier).
func (h *Handler) ListReleases(w http.ResponseWriter, r *http.Request) {
	rels, err := h.db.ListReleases(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

// GetRelease returns one release with its packages, so a caller can check what is
// already attached before adding another.
func (h *Handler) GetRelease(w http.ResponseWriter, r *http.Request) {
	rel, ok := h.releaseFromPath(w, r)
	if !ok {
		return
	}
	pkgs, err := h.db.ListPackagesByRelease(r.Context(), rel.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"release": rel, "packages": pkgs})
}

// packageBody is the add-a-package payload. The package itself is not uploaded
// here: MDM stores the URL the device will fetch, which is already the shape the
// build pipeline ends with.
type packageBody struct {
	Type          string `json:"type"` // full | incremental
	UpdateURL     string `json:"update_url"`
	SourceBuildID string `json:"source_build_id"` // required for an incremental
	Changelog     string `json:"changelog"`
}

// AddReleasePackage attaches an OTA package to a release. The target build is
// always the release's own version — an OTA that lands on a different build than
// the release claims is the one mistake this endpoint must not allow.
func (h *Handler) AddReleasePackage(w http.ResponseWriter, r *http.Request) {
	rel, ok := h.releaseFromPath(w, r)
	if !ok {
		return
	}
	var body packageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	typ := strings.TrimSpace(body.Type)
	if typ == "" {
		typ = "full"
	}
	if typ != "full" && typ != "incremental" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `type must be "full" or "incremental"`})
		return
	}
	url := strings.TrimSpace(body.UpdateURL)
	if url == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "update_url is required"})
		return
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "update_url must be an http(s) URL the devices can reach"})
		return
	}
	source := strings.TrimSpace(body.SourceBuildID)
	if typ == "incremental" && source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_build_id is required for an incremental package"})
		return
	}
	// Same rule the dashboard enforces: a release holds one full image (the
	// baseline) and any number of incrementals onto it.
	if typ == "full" {
		pkgs, _ := h.db.ListPackagesByRelease(r.Context(), rel.ID)
		for _, p := range pkgs {
			if p.Type == "full" && p.Status == "active" {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "this release already has an active full package"})
				return
			}
		}
	}
	pkg, err := h.db.CreateOTAPackage(r.Context(), typ, rel.Version, source, url,
		strings.TrimSpace(body.Changelog), rel.Product, time.Now().UTC())
	if err != nil {
		// A package's build_id is unique fleet-wide (an incremental's is
		// "<target>~from~<source>"), so the duplicate cases are exactly: this
		// same artifact is already attached — including when the previous one was
		// yanked rather than deleted, which the active-full check above cannot
		// see. That is a conflict the caller can act on, not a server fault.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			msg := "a full package for build " + rel.Version + " already exists (it may be yanked — delete it first)"
			if typ == "incremental" {
				msg = "an incremental from " + source + " to " + rel.Version + " already exists (it may be yanked — delete it first)"
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error: " + err.Error()})
		return
	}
	detail := typ + " " + rel.Version
	if source != "" {
		detail += " from " + source
	}
	_ = h.db.InsertAudit(r.Context(), "api", "release.package.add", strconv.Itoa(rel.ID), detail)
	writeJSON(w, http.StatusCreated, pkg)
}

// PublishRelease moves a release to "published" — the state a deployment can
// actually be built from. Separate from creating it, so a publisher can attach
// every package first and only then make the release deployable.
func (h *Handler) PublishRelease(w http.ResponseWriter, r *http.Request) {
	rel, ok := h.releaseFromPath(w, r)
	if !ok {
		return
	}
	if err := h.db.SetReleaseStatus(r.Context(), rel.ID, "published"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = h.db.InsertAudit(r.Context(), "api", "release.publish", strconv.Itoa(rel.ID), rel.Version)
	rel, _ = h.db.GetRelease(r.Context(), rel.ID)
	writeJSON(w, http.StatusOK, rel)
}

// releaseFromPath resolves {id} and answers the caller itself when it cannot.
func (h *Handler) releaseFromPath(w http.ResponseWriter, r *http.Request) (*db.Release, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid release id"})
		return nil, false
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil || rel == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "release not found"})
		return nil, false
	}
	// Normalize the product so a package created here lands on the same release
	// row the dashboard would have used.
	p, _ := prod.Resolve(rel.Product)
	rel.Product = p.Key
	return rel, true
}

// CancelDeployment stops an active deployment over the admin API. The dashboard has had
// this since deployments existed; the API had not, and the gap bites exactly when it
// hurts: a stalled OTA keeps its deployment 'active', RebootBlockedFor then refuses the
// reboot that would clear the device, and without a browser session there is no way out.
func (h *Handler) CancelDeployment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid deployment id"})
		return
	}
	if err := h.db.CancelDeployment(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployment": id, "status": "canceled"})
}
