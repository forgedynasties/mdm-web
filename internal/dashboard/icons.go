package dashboard

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"mdm/internal/db"
)

// MaintenancePage is the notice non-admin users see while maintenance mode is on.
// Outside maintenance (or for an admin) it just goes home.
// iconStore holds decoded app icons by content hash for /icon/{sha}.png. Pages
// used to inline every icon as a base64 data: URI — ~120 KB of uncompressible
// bytes per device page, re-sent on every load. Registering the bytes at render
// time and referencing them by hash lets the browser cache each icon once
// (immutable, keyed by content). The store is in memory: after a restart a
// browser without a cached copy 404s until the next page render re-registers
// the icon, which is the same request that shows it. Bounded; icons are few.
var (
	iconStore   sync.Map // sha (hex) -> []byte
	iconStoreN  atomic.Int32
	iconStoreMx = int32(2000)
)

func iconSrc(b64 string) string {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return ""
	}
	if i := strings.Index(b64, ","); i >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[i+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "data:image/png;base64," + b64 // unknown shape: fall back to inline
	}
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:16])
	if _, loaded := iconStore.LoadOrStore(key, raw); !loaded {
		if iconStoreN.Add(1) > iconStoreMx { // crude bound: start over rather than grow forever
			iconStore.Range(func(k, _ any) bool { iconStore.Delete(k); return true })
			iconStoreN.Store(1)
			iconStore.Store(key, raw)
		}
	}
	return "/icon/" + key
}

// IconPNG serves an icon registered by iconSrc. Immutable cache: the URL is the
// content hash, so a changed icon is a different URL.
func (h *Handler) IconPNG(w http.ResponseWriter, r *http.Request) {
	v, ok := iconStore.Load(strings.TrimSuffix(r.PathValue("sha"), ".png"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	w.Write(v.([]byte))
}

// pkgToIcon maps package name → base64 icon for the app library (uninstall rows
// carry a package, not an APK URL).
func pkgToIcon(apps []db.App) map[string]string {
	m := make(map[string]string, len(apps))
	for _, a := range apps {
		if a.PackageName != "" && a.Icon != "" {
			m[a.PackageName] = a.Icon
		}
	}
	return m
}

// apkURLToIcon maps APK URL → base64 icon for the app library.
func apkURLToIcon(apps []db.App) map[string]string {
	m := make(map[string]string, len(apps))
	for _, a := range apps {
		if a.ApkURL != "" && a.Icon != "" {
			m[a.ApkURL] = a.Icon
		}
	}
	return m
}
