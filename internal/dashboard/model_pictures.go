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
	// TSAI: our T7 tablet in its guest stand.
	"t7": {Img: "/static/img/models/t7.webp"},
	// POS: the Qualcomm C8G board.
	"trinket": {Img: "/static/img/models/trinket.webp", Tagline: "Touchscreen built for the rush.",
		Specs: []string{"15.6″ FHD touchscreen", "4 GB RAM + 64 GB", "Qualcomm"}},
	// Menu board stick: the HK1 RBOX D8 (Rockchip).
	"rockchip029": {Img: "/static/img/models/rockchip029.webp"},
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

// classPictures are pictures of a class itself, kept apart from its models' (a kiosk's
// models are boards; the class is the stand people see). Under static/img/classes.
var classPictures = map[string]modelPicture{
	"kiosk": {Img: "/static/img/classes/kiosk.webp"},
	// The Mega Kiosk shot from aioapp.com (the floor-standing display).
	"megakiosk": {Img: "/static/img/classes/megakiosk.webp"},
	// Composed (class-icons-demos.html, B1): a wall screen with our HDMI stick on it, until
	// there is a photo of one of our boards in a store.
	"dongle": {Img: "/static/img/classes/dongle.webp"},
}

// rolePic is a role's picture: the class's own, else its model's when the role has
// exactly one model. With two or more models and no class picture there is none.
func rolePic(class string, models []hwModel) *modelPicture {
	if p, ok := classPictures[class]; ok {
		return &p
	}
	if len(models) != 1 {
		return nil
	}
	return modelPic(models[0].Key)
}
