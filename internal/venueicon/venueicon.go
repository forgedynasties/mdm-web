// Package venueicon finds and normalises a venue's icon (I1, picked 19 Oct 2026).
//
// Fetch takes the restaurant's website and returns its icon as a 128px PNG: the largest
// icon the page declares (apple-touch-icon first, then rel=icon), else Google's favicon
// cache for the domain, which also covers sites that refuse non-browser requests. The
// result is stored in the database, so a dashboard page never loads anything from the
// restaurant's site.
package venueicon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/safehttp"
)

// Size is the stored icon's longest side.
const Size = 128

const (
	maxPage  = 1 << 20
	maxImage = 2 << 20
	ua       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
)

// ErrNoIcon means neither the site nor Google's cache had a usable icon.
var ErrNoIcon = errors.New("no usable icon found for that site")

// Fetch finds the icon for a website ("momsbiryanica.com" or a full URL). source says
// where it came from, for the edit page and the audit log.
func Fetch(ctx context.Context, site string) (icon []byte, source string, err error) {
	page, host, err := siteURL(site)
	if err != nil {
		return nil, "", err
	}
	client := safehttp.Client(10 * time.Second)
	for _, u := range pageIcons(ctx, client, page) {
		if b, err := get(ctx, client, u, maxImage); err == nil {
			if out, err := Normalize(b); err == nil {
				return out, u, nil
			}
		}
	}
	g := "https://www.google.com/s2/favicons?sz=256&domain=" + url.QueryEscape(host)
	if b, err := get(ctx, client, g, maxImage); err == nil {
		if out, err := Normalize(b); err == nil {
			return out, "Google's favicon cache for " + host, nil
		}
	}
	return nil, "", ErrNoIcon
}

// siteURL turns what someone typed into the page to read and its host.
func siteURL(site string) (string, string, error) {
	s := strings.TrimSpace(site)
	if s == "" {
		return "", "", errors.New("enter the venue's website")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", fmt.Errorf("%q is not a website address", site)
	}
	if err := safehttp.CheckURL(u.String(), false); err != nil {
		return "", "", err
	}
	return u.String(), strings.TrimPrefix(u.Hostname(), "www."), nil
}

var (
	linkTag = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	attr    = regexp.MustCompile(`(?is)\b(rel|href|sizes)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// pageIcons lists the icons a page declares, best first: apple-touch-icon, then the
// largest declared size.
func pageIcons(ctx context.Context, c *http.Client, page string) []string {
	b, err := get(ctx, c, page, maxPage)
	if err != nil {
		return nil
	}
	base, _ := url.Parse(page)
	type cand struct {
		u     string
		touch bool
		size  int
	}
	var out []cand
	for _, tag := range linkTag.FindAllString(string(b), -1) {
		var rel, href, sizes string
		for _, m := range attr.FindAllStringSubmatch(tag, -1) {
			v := m[3] + m[4] + m[5]
			switch strings.ToLower(m[1]) {
			case "rel":
				rel = strings.ToLower(v)
			case "href":
				href = v
			case "sizes":
				sizes = v
			}
		}
		if href == "" || !strings.Contains(rel, "icon") || strings.Contains(rel, "mask") {
			continue
		}
		ref, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			continue
		}
		c := cand{u: base.ResolveReference(ref).String(), touch: strings.Contains(rel, "apple-touch")}
		if f := strings.Fields(sizes); len(f) > 0 {
			c.size, _ = strconv.Atoi(strings.SplitN(strings.ToLower(f[len(f)-1]), "x", 2)[0])
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].touch != out[j].touch {
			return out[i].touch
		}
		return out[i].size > out[j].size
	})
	var urls []string
	seen := map[string]bool{}
	for _, c := range out {
		if !seen[c.u] {
			seen[c.u] = true
			urls = append(urls, c.u)
		}
	}
	return urls
}

func get(ctx context.Context, c *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	return safehttp.LimitedReadAll(io.Reader(resp.Body), limit)
}

// Normalize decodes a PNG, JPEG or GIF and returns it as a PNG no larger than Size on
// its longest side. A picture below 16px is refused: that is a placeholder, not a logo.
func Normalize(raw []byte) ([]byte, error) {
	if len(raw) > maxImage {
		return nil, errors.New("image is over 2 MB")
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("not a PNG, JPEG or GIF image")
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 16 || h < 16 {
		return nil, errors.New("image is too small")
	}
	dst := image.Image(src)
	if w > Size || h > Size {
		nw, nh := Size, Size
		if w > h {
			nh = max(1, h*Size/w)
		} else {
			nw = max(1, w*Size/h)
		}
		dst = shrink(src, nw, nh)
	}
	var out bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// shrink is an area-average downscale, good enough for logos and free of dependencies.
func shrink(src image.Image, nw, nh int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := y*h/nh, max((y+1)*h/nh, y*h/nh+1)
		for x := 0; x < nw; x++ {
			x0, x1 := x*w/nw, max((x+1)*w/nw, x*w/nw+1)
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					c := color.NRGBAModel.Convert(src.At(b.Min.X+sx, b.Min.Y+sy)).(color.NRGBA)
					// Weight colour by alpha so transparent edges don't darken.
					r += uint64(c.R) * uint64(c.A)
					g += uint64(c.G) * uint64(c.A)
					bl += uint64(c.B) * uint64(c.A)
					a += uint64(c.A)
					n++
				}
			}
			px := color.NRGBA{A: uint8(a / n)}
			if a > 0 {
				px.R, px.G, px.B = uint8(r/a), uint8(g/a), uint8(bl/a)
			}
			dst.SetNRGBA(x, y, px)
		}
	}
	return dst
}
