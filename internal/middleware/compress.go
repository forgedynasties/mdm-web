package middleware

import (
	"bufio"
	"compress/gzip"
	"net"
	"io"
	"net/http"
	"strings"
)

// gzipResponseWriter wraps an http.ResponseWriter so writes go through a gzip.Writer.
// Deliberately does NOT implement Flush/Hijack passthroughs — CompressStatic is only
// used for the static file server (plain byte responses), never for streaming/SSE
// handlers, so there's no long-lived-connection flushing to worry about here.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.Header().Del("Content-Length") // compressed length differs from the original
	g.Header().Set("Content-Encoding", "gzip")
	g.Header().Add("Vary", "Accept-Encoding")
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.gz.Write(b)
}

// compressibleExt gates compression to text-based static assets. Fonts/images are
// already compressed formats — gzipping them again wastes CPU for no size benefit
// (and can occasionally grow slightly).
var compressibleExt = map[string]bool{
	".css": true, ".js": true, ".html": true, ".svg": true, ".json": true, ".txt": true,
}

// CompressStatic gzip-encodes the static file server's response body when the client
// advertises gzip support. static/style.css and the vendored JS bundles (chart.js,
// xterm.js, app-info-parser) are hundreds of KB served raw; gzip typically cuts
// text-based CSS/JS 70-80% in transfer size. Cache-Control on these responses is
// already "immutable, max-age=1w" (set by the caller), so this mainly pays off on a
// cold cache or a cache-busted asset version.
func CompressStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		// http.FileServer honors byte-Range requests; a gzip-compressed byte range
		// wouldn't correspond to the original file's bytes, so skip compression
		// rather than risk a corrupt partial response.
		if r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		dot := strings.LastIndex(r.URL.Path, ".")
		if dot < 0 || !compressibleExt[strings.ToLower(r.URL.Path[dot:])] {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	})
}

// maxInflatedBody caps how many bytes we will inflate from a gzip request body,
// so a small "decompression bomb" can't expand without bound in memory. Reads
// past this return EOF (the JSON decode then fails with a 400).
const maxInflatedBody = 16 << 20 // 16 MiB

// MaxBytes limits how many bytes a handler will read from the request body,
// responding 413 past the cap. Use on unauthenticated/device endpoints so a
// single client can't exhaust memory with a huge (or huge-when-inflated) body.
func MaxBytes(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next.ServeHTTP(w, r)
	})
}

// DecompressRequest transparently inflates gzip-encoded request bodies so handlers
// can read JSON without knowing the client compressed it. It is a no-op when the
// request has no "Content-Encoding: gzip" header, so plain-JSON clients keep working
// unchanged — devices only gzip their (larger) check-in payloads to cut radio-on time.
func DecompressRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "invalid gzip request body", http.StatusBadRequest)
				return
			}
			defer gz.Close()
			// Cap inflation so a compressed bomb can't expand without bound.
			r.Body = io.NopCloser(io.LimitReader(gz, maxInflatedBody))
			r.Header.Del("Content-Encoding")
			r.ContentLength = -1 // length now refers to the compressed bytes; unknown after inflate
		}
		next.ServeHTTP(w, r)
	})
}

// htmlGzipWriter compresses a response only once the handler reveals a text/html
// Content-Type; everything else (JSON partials, SSE streams, redirects, WebSocket
// upgrades) passes through untouched. Flush and Hijack are forwarded so SSE and
// WS handlers behind the same mux keep working.
type htmlGzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
	gzip    bool
}

func (g *htmlGzipWriter) decide() {
	if g.decided {
		return
	}
	g.decided = true
	ct := g.Header().Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") && g.Header().Get("Content-Encoding") == "" {
		g.gzip = true
		g.Header().Del("Content-Length")
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Add("Vary", "Accept-Encoding")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
}

func (g *htmlGzipWriter) WriteHeader(code int) {
	if code == http.StatusNoContent || code == http.StatusNotModified || (code >= 300 && code < 400) {
		g.decided = true // no body to compress
	}
	g.decide()
	g.ResponseWriter.WriteHeader(code)
}

func (g *htmlGzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.decide()
	}
	if g.gzip {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *htmlGzipWriter) Flush() {
	if g.gzip {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *htmlGzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := g.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (g *htmlGzipWriter) close() {
	if g.gzip && g.gz != nil {
		g.gz.Close()
	}
}

// CompressHTML gzip-encodes text/html page responses for clients that accept it.
// Dashboard pages are large (a device page is ~0.5–1.2 MB of markup, inline
// script and chart data) and compress about 3:1, which is most of the transfer
// time on anything slower than a LAN. Static files have their own middleware.
func CompressHTML(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		g := &htmlGzipWriter{ResponseWriter: w}
		defer g.close()
		next.ServeHTTP(g, r)
	})
}
