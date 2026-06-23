package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

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
			r.Body = io.NopCloser(gz)
			r.Header.Del("Content-Encoding")
			r.ContentLength = -1 // length now refers to the compressed bytes; unknown after inflate
		}
		next.ServeHTTP(w, r)
	})
}
