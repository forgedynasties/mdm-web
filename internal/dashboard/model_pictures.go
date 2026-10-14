package dashboard

import prod "mdm/internal/product"

// modelPicture is a hardware model's product shot and the few words aioapp.com says about
// it (static/model-images-demos.html, M3, picked 14 Oct). Shots are 480 px webp under
// static/img/models/<model key>.webp, trimmed from the product pages.
type modelPicture struct {
	Img, Tagline string
	Specs        []string
}

var modelPictures = map[string]modelPicture{
	// POS: the Qualcomm C8G board.
	"trinket": {Img: "/static/img/models/trinket.webp", Tagline: "Touchscreen built for the rush.",
		Specs: []string{"15.6″ FHD touchscreen", "4 GB RAM + 64 GB", "Qualcomm"}},
	// KDS: the Sunmi D2s on live.
	"d2s_kds_stgl": {Img: "/static/img/models/d2s_kds_stgl.webp", Tagline: "Built to handle steam, splashes and a slammed Saturday night."},
}

// modelPic returns the model's picture, nil when it has none.
func modelPic(key string) *modelPicture {
	if p, ok := modelPictures[prod.Normalize(key)]; ok {
		return &p
	}
	return nil
}

// rolePic is a role's picture: its model's, when the role has exactly one model. With two
// or more there is no one shot that is the role.
func rolePic(models []hwModel) *modelPicture {
	if len(models) != 1 {
		return nil
	}
	return modelPic(models[0].Key)
}
