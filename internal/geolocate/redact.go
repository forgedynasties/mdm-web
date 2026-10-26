package geolocate

import (
	"errors"
	"net/url"
)

// withoutKey drops the API key from a failed request's error. Go's *url.Error prints
// the whole URL, ?key= included, and these errors are logged and shown on Settings'
// usage card (security audit, 26 Oct 2026).
func withoutKey(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	if u, perr := url.Parse(ue.URL); perr == nil {
		q := u.Query()
		if q.Has("key") {
			q.Set("key", "REDACTED")
			u.RawQuery = q.Encode()
			ue.URL = u.String()
		}
	}
	return err
}
