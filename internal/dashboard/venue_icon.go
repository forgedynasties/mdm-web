package dashboard

import (
	"context"
	"fmt"
	"hash/fnv"
	"html/template"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Venue icons (I1, picked 19 Oct 2026): a venue's icon shows wherever you pick or open a
// venue — the Venues list, the venue page header and Fleet's Restaurants list. Device
// rows, device pages and alerts keep the name alone. A venue with no icon shows its
// initial on a colour tile. Icons are set through the admin API (or, later, the venue's
// Edit page) and stored in the database: no page loads from the restaurant's site.

// RestaurantIconImage serves a venue's stored icon. The URL carries its version, so it
// can be cached for good.
func (h *Handler) RestaurantIconImage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, at, _, err := h.db.RestaurantIcon(r.Context(), id)
	if err != nil || b == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Last-Modified", at.UTC().Format(http.TimeFormat))
	w.Write(b)
}

// venueIconHTML draws a venue's icon (or its initial) at one of the sizes the pages use:
// "sm" 20px, "md" 32px.
func venueIconHTML(d *db.DB, id any, name, size string) template.HTML {
	var vid uuid.UUID
	switch v := id.(type) {
	case uuid.UUID:
		vid = v
	case *uuid.UUID:
		if v != nil {
			vid = *v
		}
	case string:
		vid, _ = uuid.Parse(v)
	default:
		vid, _ = uuid.Parse(fmt.Sprint(v))
	}
	if size != "sm" {
		size = "md"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ver, ok := d.VenueIconVersions(ctx)[vid]; ok && vid != uuid.Nil {
		return template.HTML(fmt.Sprintf(`<img class="vicon vicon-%s" src="/restaurants/%s/icon?v=%d" alt="" loading="lazy" decoding="async">`, size, vid, ver))
	}
	ini := "?"
	for _, r := range strings.TrimSpace(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			ini = strings.ToUpper(string(r))
			break
		}
	}
	f := fnv.New32a()
	f.Write([]byte(vid.String() + name))
	return template.HTML(fmt.Sprintf(`<span class="vicon vicon-%s vicon-ini" style="--vh:%d" aria-hidden="true">%s</span>`, size, f.Sum32()%360, template.HTMLEscapeString(ini)))
}
