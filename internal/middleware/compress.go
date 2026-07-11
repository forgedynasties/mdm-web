package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

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
