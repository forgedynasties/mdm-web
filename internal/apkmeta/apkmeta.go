// Package apkmeta fetches lightweight metadata (size + ETag) for an APK URL at
// command-creation time and folds it into the install_apk command payload, so the
// device can verify download completeness and resume safely via HTTP Range.
//
// It is deliberately best-effort: the APK bytes live on an external host (a public
// S3 object), and a slow or unreachable host must never block an operator from
// queuing a command. On any failure the original payload is returned unchanged.
package apkmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"mdm/internal/safehttp"
)

// client issues a HEAD to the APK URL. The timeout is short because this runs
// inline on the command-creation request path. It follows redirects (S3 may 301 to a
// regional endpoint) but is SSRF-hardened: an operator-supplied apk_url that resolves
// (or redirects) to an internal address is refused at dial time.
var client = safehttp.Client(10 * time.Second)

// Fetch returns the object's size (Content-Length) and ETag via a HEAD request.
// The ETag is returned verbatim (including its surrounding quotes) so it can be
// echoed back in an If-Range header and byte-compared by the origin.
func Fetch(ctx context.Context, url string) (size int64, etag string, err error) {
	if err := safehttp.CheckURL(url, false); err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("HEAD %s: HTTP %d", url, resp.StatusCode)
	}
	return resp.ContentLength, strings.TrimSpace(resp.Header.Get("ETag")), nil
}

// Augment folds apk_size and apk_etag into an install_apk command payload. On any
// error it logs and returns the payload unchanged, so command creation is never
// blocked by a slow or unreachable file host.
func Augment(ctx context.Context, url string, payload json.RawMessage) json.RawMessage {
	size, etag, err := Fetch(ctx, url)
	if err != nil {
		log.Printf("apkmeta: HEAD %s failed: %v (queuing install without size/etag)", url, err)
		return payload
	}
	m := map[string]any{}
	if len(payload) > 0 {
		if e := json.Unmarshal(payload, &m); e != nil {
			m = map[string]any{} // payload wasn't a JSON object — start fresh
		}
	}
	if size > 0 {
		m["apk_size"] = size
	}
	if etag != "" {
		m["apk_etag"] = etag
	}
	b, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return json.RawMessage(b)
}
