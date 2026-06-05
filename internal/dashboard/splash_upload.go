package dashboard

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoder for image.Decode
	_ "image/png"  // register PNG decoder for image.Decode
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// SplashStoreDir is the directory generated splash.img files are written to and
// served from (see the /splash-img/ route in cmd/server/main.go). It should be
// backed by a persistent volume so a device that is offline when the command is
// issued can still fetch the image when it next checks in. Override with
// SPLASH_DIR; defaults to data/splash relative to the working directory.
func SplashStoreDir() string {
	if d := strings.TrimSpace(os.Getenv("SPLASH_DIR")); d != "" {
		return d
	}
	return "data/splash"
}

// splashFillerSize is the zero-filler prefix the legacy Qualcomm splash layout
// places before the BMP (see vendor splash_logo_gen.py). The bootloader skips
// it and parses the BMP at this offset; the device pre-check and init broker
// both expect the "BM" signature here.
const splashFillerSize = 0x4000

// uploadSizeLimit caps how much of an upload we read into memory.
const uploadSizeLimit = 64 << 20 // 64 MiB

// wrapSplash builds a splash.img: splashFillerSize bytes of zero filler
// followed by the raw BMP — byte-for-byte the format splash_logo_gen.py emits.
func wrapSplash(bmp []byte) []byte {
	out := make([]byte, splashFillerSize+len(bmp))
	copy(out[splashFillerSize:], bmp)
	return out
}

// splashBMP returns raw BMP bytes for an uploaded image. A BMP is passed through
// untouched so the operator keeps full control of the bit depth (matching
// logo_gen.py); PNG/JPEG are decoded and re-encoded to a 24-bit bottom-up BMP.
func splashBMP(data []byte) ([]byte, error) {
	if len(data) >= 2 && data[0] == 'B' && data[1] == 'M' {
		return data, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unsupported image (need BMP, PNG or JPEG): %w", err)
	}
	return encodeBMP24(img), nil
}

// encodeBMP24 encodes img as an uncompressed 24-bit (BGR) BMP with a
// BITMAPINFOHEADER and bottom-up rows — the layout the legacy bootloader and
// the known-good splash.img use (BI_RGB, positive height, 4-byte row padding).
func encodeBMP24(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	rowSize := (w*3 + 3) &^ 3 // pad each scanline to a 4-byte boundary
	pixels := rowSize * h
	const fileHdr, infoHdr = 14, 40
	offset := fileHdr + infoHdr
	buf := make([]byte, offset+pixels)

	// BITMAPFILEHEADER
	buf[0], buf[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(buf[2:], uint32(offset+pixels))
	binary.LittleEndian.PutUint32(buf[10:], uint32(offset))
	// BITMAPINFOHEADER
	binary.LittleEndian.PutUint32(buf[14:], infoHdr)
	binary.LittleEndian.PutUint32(buf[18:], uint32(w))
	binary.LittleEndian.PutUint32(buf[22:], uint32(h)) // positive => bottom-up
	binary.LittleEndian.PutUint16(buf[26:], 1)         // color planes
	binary.LittleEndian.PutUint16(buf[28:], 24)        // bits per pixel
	binary.LittleEndian.PutUint32(buf[30:], 0)         // BI_RGB (uncompressed)
	binary.LittleEndian.PutUint32(buf[34:], uint32(pixels))

	// Pixel data: BGR triples, bottom scanline first.
	p := offset
	for y := h - 1; y >= 0; y-- {
		px := p
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			buf[px] = byte(bl >> 8)
			buf[px+1] = byte(g >> 8)
			buf[px+2] = byte(r >> 8)
			px += 3
		}
		p += rowSize
	}
	return buf
}

// generateSplashFromUpload reads an uploaded BMP/PNG/JPEG from the "splash_file"
// field, wraps it into a splash.img, writes it under SplashStoreDir(), and
// returns an absolute URL the device can download it from. It returns ("", nil)
// when no file was uploaded, so the caller can fall back to a pasted URL.
func (h *Handler) generateSplashFromUpload(r *http.Request) (string, error) {
	f, _, err := r.FormFile("splash_file")
	if err != nil {
		// No file part (or not multipart) — let the caller use splash_url.
		return "", nil
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, uploadSizeLimit))
	if err != nil {
		return "", fmt.Errorf("read upload: %w", err)
	}
	if len(data) == 0 {
		return "", nil
	}
	bmp, err := splashBMP(data)
	if err != nil {
		return "", err
	}

	dir := SplashStoreDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create splash dir: %w", err)
	}
	name := uuid.New().String() + ".img"
	if err := os.WriteFile(filepath.Join(dir, name), wrapSplash(bmp), 0o644); err != nil {
		return "", fmt.Errorf("write splash image: %w", err)
	}
	return publicBaseURL(r) + "/splash-img/" + name, nil
}

// publicBaseURL returns the scheme://host a device should use to download
// server-generated assets. PUBLIC_BASE_URL overrides it (set this when the
// dashboard host differs from the host devices reach); otherwise it is derived
// from the request, honouring an X-Forwarded-Proto from a TLS terminator.
func publicBaseURL(r *http.Request) string {
	if b := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")); b != "" {
		return strings.TrimRight(b, "/")
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
