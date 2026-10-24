package dashboard

import (
	"context"

	"mdm/internal/product"
)

// prodTag says which product a release is Lineup's prod firmware for (audit, 24 Oct
// 2026): prod is decided on Lineup and shipped from Releases, and Releases now says so
// and links back to the model on Lineup.
type prodTag struct {
	Label string // the product (role), "Tableside AI"
	Model string // the model's own name, "T7"
	Href  string
}

// releaseProdFor maps release id → the products it is prod firmware for.
func (h *Handler) releaseProdFor(ctx context.Context) map[int][]prodTag {
	out := map[int][]prodTag{}
	chans, err := h.db.ListFirmwareChannels(ctx)
	if err != nil {
		return out
	}
	for _, c := range chans {
		if c.ReleaseID == nil {
			continue
		}
		t := prodTag{Label: productLabel(c.Model), Model: productLabel(c.Model), Href: "/lineup"}
		if p, ok := product.Resolve(c.Model); ok && p.Class != "" {
			t.Label = product.ClassLabel(p.Class)
			t.Href = "/lineup?role=" + p.Class + "#model-" + product.Normalize(c.Model)
		}
		out[*c.ReleaseID] = append(out[*c.ReleaseID], t)
	}
	return out
}
