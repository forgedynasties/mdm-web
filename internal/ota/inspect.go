// Package ota reads the metadata of an Android A/B OTA package straight from its URL,
// without downloading the (multi-GB) payload. An OTA zip carries a tiny, uncompressed
// META-INF/com/android/metadata text file listing the build fingerprints; we fetch the
// zip's central directory from the tail and then just that one entry via HTTP range
// requests, so an inspect costs a few KB regardless of image size.
package ota

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mdm/internal/safehttp"
)

// metadataPath is where the build info lives in every Android OTA zip.
const metadataPath = "META-INF/com/android/metadata"

// Metadata is what an inspect derives from an OTA package.
type Metadata struct {
	OTAType       string `json:"ota_type"`       // e.g. "AB"
	IsIncremental bool   `json:"is_incremental"` // true when the package has a pre-build (source)
	TargetBuild   string `json:"target_build"`   // ro.build.id the device reports after installing
	SourceBuild   string `json:"source_build"`   // ro.build.id an incremental applies onto ("" for full)
	Device        string `json:"device"`         // pre-device, e.g. "T7" — the hardware it's built for
	Fingerprint   string `json:"fingerprint"`    // full post-build fingerprint
	SizeBytes     int64  `json:"size_bytes"`     // total package size
}

// client is SSRF-hardened: an operator/dev-supplied update_url that resolves or
// redirects to an internal address is refused at dial time. This path reads response
// bytes back to the caller (the zip central directory), so the block matters more here.
var client = safehttp.Client(20 * time.Second)

// Inspect fetches an OTA package's metadata by URL using HTTP range requests. It never
// downloads the payload. Returns an error if the URL is unreachable, isn't a zip, or
// has no OTA metadata entry.
func Inspect(ctx context.Context, url string) (*Metadata, error) {
	if err := safehttp.CheckURL(url, false); err != nil {
		return nil, err
	}
	size, err := objectSize(ctx, url)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(&httpReaderAt{ctx: ctx, url: url, size: size}, size)
	if err != nil {
		return nil, fmt.Errorf("not a readable zip: %w", err)
	}
	var entry *zip.File
	for _, f := range zr.File {
		if f.Name == metadataPath {
			entry = f
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("no %s — not an Android OTA package", metadataPath)
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// The metadata entry is tiny (<2 KB); cap the read defensively.
	body, err := io.ReadAll(io.LimitReader(rc, 64*1024))
	if err != nil {
		return nil, err
	}
	kv := parseKV(string(body))
	m := &Metadata{
		OTAType:     kv["ota-type"],
		Device:      kv["pre-device"],
		Fingerprint: kv["post-build"],
		SizeBytes:   size,
	}
	pre, post := kv["pre-build"], kv["post-build"]
	m.IsIncremental = pre != ""
	m.TargetBuild = buildIDFromFingerprint(post)
	if pre != "" {
		m.SourceBuild = buildIDFromFingerprint(pre)
	}
	return m, nil
}

// parseKV parses the metadata file's `key=value` lines (one per line).
func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if i := strings.IndexByte(line, '='); i > 0 {
			out[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	return out
}

// buildIDFromFingerprint pulls ro.build.id out of an Android build fingerprint, whose
// shape is brand/product/device:release/ID/incremental:type/tags — the ID is the
// second element of the middle (colon-delimited) segment.
func buildIDFromFingerprint(fp string) string {
	parts := strings.SplitN(fp, ":", 3)
	if len(parts) < 2 {
		return ""
	}
	mid := strings.Split(parts[1], "/") // release / ID / incremental
	if len(mid) >= 2 {
		return mid[1]
	}
	return ""
}

// objectSize returns the total byte length of the URL's object, preferring a HEAD and
// falling back to a 1-byte ranged GET (some hosts don't answer HEAD).
func objectSize(ctx context.Context, url string) (int64, error) {
	if req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
				return resp.ContentLength, nil
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Content-Range: bytes 0-0/<total>
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil && n > 0 {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("could not determine package size (no Content-Length or Content-Range)")
}

// httpReaderAt implements io.ReaderAt over an HTTP object using range requests, so
// archive/zip can read the central directory + a single entry without a full download.
type httpReaderAt struct {
	ctx  context.Context
	url  string
	size int64
}

func (h *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= h.size {
		return 0, io.EOF
	}
	end := off + int64(len(p)) - 1
	short := false
	if end >= h.size {
		end = h.size - 1
		short = true
	}
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206 — the body is exactly our range
	case http.StatusOK: // server ignored Range; body starts at 0, so only offset 0 is safe
		if off != 0 {
			return 0, fmt.Errorf("host does not support range requests (HTTP 200) — cannot inspect without downloading the whole package")
		}
	default:
		return 0, fmt.Errorf("range request failed: HTTP %d", resp.StatusCode)
	}
	n, err := io.ReadFull(resp.Body, p[:end-off+1])
	if err == nil && short {
		return n, io.EOF
	}
	if err == io.ErrUnexpectedEOF {
		return n, io.EOF
	}
	return n, err
}
