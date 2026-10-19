package venueicon

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngOf(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: 200, G: 40, B: 40, A: 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestNormalize(t *testing.T) {
	out, err := Normalize(pngOf(400, 200))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != Size || b.Dy() != Size/2 {
		t.Fatalf("400x200 became %dx%d, want %dx%d", b.Dx(), b.Dy(), Size, Size/2)
	}
	if c := color.NRGBAModel.Convert(img.At(10, 10)).(color.NRGBA); c.R != 200 || c.A != 255 {
		t.Fatalf("colour lost in the shrink: %+v", c)
	}
	if out, _ := Normalize(pngOf(64, 64)); out == nil {
		t.Fatal("a 64px icon is kept as it is")
	}
	if _, err := Normalize(pngOf(8, 8)); err == nil {
		t.Fatal("an 8px image is a placeholder, not a logo")
	}
	if _, err := Normalize([]byte("<html>")); err == nil {
		t.Fatal("HTML is not an image")
	}
}

func TestSiteURL(t *testing.T) {
	for in, host := range map[string]string{
		"momsbiryanica.com":                   "momsbiryanica.com",
		"https://www.flightsrestaurants.com/": "flightsrestaurants.com",
		" huanchacoperuvian.com ":             "huanchacoperuvian.com",
	} {
		if _, h, err := siteURL(in); err != nil || h != host {
			t.Errorf("siteURL(%q) = %q, %v; want %q", in, h, err, host)
		}
	}
	// A private address is refused when the client dials it (safehttp), not here.
	for _, bad := range []string{"", "ftp://x.com"} {
		if _, _, err := siteURL(bad); err == nil {
			t.Errorf("siteURL(%q) accepted", bad)
		}
	}
}
