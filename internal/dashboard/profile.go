package dashboard

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"log"
	"net/http"
	"strings"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// ProfilePage shows the signed-in user their own account details and footprint:
// actions, commands, devices touched, release work, recent activity.
func (h *Handler) ProfilePage(w http.ResponseWriter, r *http.Request) {
	h.renderProfile(w, r, h.currentUsername(r), false)
}

// UserProfilePage is an admin's view of another user's profile (Users → Stats).
func (h *Handler) UserProfilePage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	u, err := h.db.GetUser(r.Context(), id)
	if err != nil || u == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if u.Username == h.currentUsername(r) {
		h.renderProfile(w, r, u.Username, false)
		return
	}
	// Anyone signed in may look at a colleague's profile (stats, recent activity);
	// the management and access cards appear only for those who may manage them.
	canManage := roleManagesUsers(h.role(r)) && mayManageUser(h.role(r), u.Role, "")
	if canManage {
		// I1 (21 Oct 2026): for whoever may manage them, Profile and Manage are one page.
		h.renderUserI1(w, r, u)
		return
	}
	h.renderProfile(w, r, u.Username, true)
}

func (h *Handler) renderProfile(w http.ResponseWriter, r *http.Request, username string, viewingOther bool) {
	ctx := r.Context()
	stats, err := h.db.UserStats(ctx, username, legacyAdminActor)
	if err != nil {
		log.Printf("[profile] stats for %q: %v", username, err)
		stats = &db.UserStats{Username: username}
	}
	user, _ := h.db.GetUserByUsername(ctx, username) // nil for the env dashboard login
	var recent []db.AuditEntry
	if !viewingOther || h.role(r) == "admin" {
		recent, _ = h.db.ListAuditFiltered(ctx, username, 12)
	}
	display := username
	if user != nil {
		display = user.DisplayName()
	}
	title := "My profile"
	if viewingOther {
		title = display
	}
	// A restaurant owner's own profile is just their account and their venues;
	// none of the operator statistics apply to them.
	var ownerVenues []string
	if !viewingOther && roleIsCustomer(h.role(r)) {
		if pol, err := h.db.GetUserAccess(ctx, username); err == nil {
			rs, _ := h.db.ListRestaurants(ctx)
			byID := map[string]string{}
			for _, x := range rs {
				byID[x.ID.String()] = x.Name
			}
			for _, g := range pol.Grants {
				if g.Effect == "allow" && g.ScopeType == "restaurant" && g.ScopeID != nil {
					if n, ok := byID[g.ScopeID.String()]; ok {
						ownerVenues = append(ownerVenues, n)
					}
				}
			}
		}
	}
	enrolled, _ := h.db.RecentEnrollments(ctx, username, time.Time{}, 50)
	enrolled = keepVisibleEnrollments(enrolled, h.visibleSerialSet(r))
	h.render(w, r, "profile.html", map[string]any{
		"Title":          title,
		"User":           user,
		"Bubble":         userBubbleFn(username),
		"CanEditAvatar":  user != nil && h.mayEditAvatar(r, user),
		"Display":        display,
		"Username":       username,
		"Stats":          stats,
		"Recent":         recent,
		"Enrolled":       enrolled,
		"ViewingOther":   viewingOther,
		"CanSeeActivity": !viewingOther || h.role(r) == "admin", // only a super admin reads someone else's activity
		"OwnerSelf":      !viewingOther && roleIsCustomer(h.role(r)),
		"OwnerVenues":    ownerVenues,
		"DisplayPrefs":   h.profileDisplay(r, viewingOther),
	})
}

// UserMergeActor re-points past activity recorded under a username that no longer
// has an account (deleted after e.g. a Microsoft sign-in replaced it) to an
// existing user, so Activity, Actions history and release sign-offs show one
// person. Only orphaned usernames can be merged: merging a live account would
// silently rewrite history for someone who still signs in.
func (h *Handler) UserMergeActor(w http.ResponseWriter, r *http.Request) {
	from := strings.TrimSpace(r.FormValue("from"))
	toID, err := uuid.Parse(strings.TrimSpace(r.FormValue("to")))
	if from == "" || err != nil {
		http.Error(w, "Pick a username and a target user", http.StatusBadRequest)
		return
	}
	target, err := h.db.GetUser(r.Context(), toID)
	if err != nil {
		http.Error(w, "Target user not found", http.StatusNotFound)
		return
	}
	if u, err := h.db.GetUserByUsername(r.Context(), from); err == nil && u != nil {
		http.Error(w, "That username still has an account; delete it first if you really mean to merge it", http.StatusConflict)
		return
	}
	n, err := h.db.MergeActor(r.Context(), from, target.Username)
	if err != nil {
		log.Printf("[users] merge %q -> %q: %v", from, target.Username, err)
		http.Error(w, "Merge failed", http.StatusInternalServerError)
		return
	}
	h.audit(r, "user.merge", target.Username, fmt.Sprintf("%s → %s (%d rows)", from, target.Username, n))
	h.hxRedirect(w, r, "/users")
}

// userBubbleFn is the template resolver for avatar bubbles, set when the template
// func map is built; handlers use it for the layout chip and profile hero.
var userBubbleFn func(name string) any

// userIsAdminFn reports whether a command author (username or display name) is an
// admin/dev account; set alongside userBubbleFn.
var userIsAdminFn func(name string) bool

// ---------------------------------------------------------------- profile pictures

// avatarSize is the stored edge length; bubbles render at 18-64px so this is plenty.
const avatarSize = 128

// UserAvatar serves the stored PNG. Versioned URLs (?v=) make it safe to cache hard.
func (h *Handler) UserAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	png, ver, err := h.db.GetUserAvatar(r.Context(), id)
	if err != nil || len(png) == 0 {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"av-%d"`, ver)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", etag)
	w.Write(png)
}

// mayEditAvatar: your own picture, or any user's if you are an admin.
func (h *Handler) mayEditAvatar(r *http.Request, target *db.User) bool {
	if strings.EqualFold(h.currentUsername(r), target.Username) {
		return true
	}
	return h.role(r) == "admin"
}

// UserSetAvatar accepts an image upload (multipart field "avatar"), squares and
// downsizes it to avatarSize and stores it as PNG.
func (h *Handler) UserSetAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.mayEditAvatar(r, target) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "Image too large (8 MB max).", http.StatusRequestEntityTooLarge)
		return
	}
	f, _, err := r.FormFile("avatar")
	if err != nil {
		http.Error(w, "Choose an image first.", http.StatusBadRequest)
		return
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		http.Error(w, "That file is not a PNG, JPEG or GIF image.", http.StatusBadRequest)
		return
	}
	out := squareThumb(src, avatarSize)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		http.Error(w, "Could not encode image.", http.StatusInternalServerError)
		return
	}
	if err := h.db.SetUserAvatar(r.Context(), id, buf.Bytes()); err != nil {
		http.Error(w, "Could not save picture.", http.StatusInternalServerError)
		return
	}
	h.audit(r, "user.avatar.set", target.Username, "")
	if to := r.FormValue("redirect"); strings.HasPrefix(to, "/users/") && !strings.Contains(to, "//") {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/users/"+id.String()+"/profile", http.StatusSeeOther)
}

// UserClearAvatar removes the picture (back to the initial bubble).
func (h *Handler) UserClearAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target, err := h.db.GetUser(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.mayEditAvatar(r, target) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	_ = h.db.ClearUserAvatar(r.Context(), id)
	h.audit(r, "user.avatar.clear", target.Username, "")
	if to := r.FormValue("redirect"); strings.HasPrefix(to, "/users/") && !strings.Contains(to, "//") {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/users/"+id.String()+"/profile", http.StatusSeeOther)
}

// squareThumb center-crops src to a square and scales it to size×size with a
// box filter (average of the covered source pixels), which is smooth enough for a
// small avatar and needs no extra dependency.
func squareThumb(src image.Image, size int) *image.NRGBA {
	b := src.Bounds()
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for dy := 0; dy < size; dy++ {
		sy0 := y0 + dy*side/size
		sy1 := y0 + (dy+1)*side/size
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < size; dx++ {
			sx0 := x0 + dx*side/size
			sx1 := x0 + (dx+1)*side/size
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rs, gs, bs, as, n uint64
			for y := sy0; y < sy1; y++ {
				for x := sx0; x < sx1; x++ {
					cr, cg, cb, ca := src.At(x, y).RGBA()
					rs += uint64(cr >> 8)
					gs += uint64(cg >> 8)
					bs += uint64(cb >> 8)
					as += uint64(ca >> 8)
					n++
				}
			}
			i := dst.PixOffset(dx, dy)
			dst.Pix[i+0] = uint8(rs / n)
			dst.Pix[i+1] = uint8(gs / n)
			dst.Pix[i+2] = uint8(bs / n)
			dst.Pix[i+3] = uint8(as / n)
		}
	}
	return dst
}
