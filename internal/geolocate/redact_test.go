package geolocate

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestWithoutKey(t *testing.T) {
	err := withoutKey(&url.Error{Op: "Get", URL: "https://maps.googleapis.com/maps/api/geocode/json?address=x&key=AIzaSECRET", Err: errors.New("timeout")})
	if strings.Contains(err.Error(), "AIzaSECRET") || !strings.Contains(err.Error(), "key=REDACTED") {
		t.Fatalf("key not redacted: %v", err)
	}
	plain := errors.New("boom")
	if withoutKey(plain) != plain {
		t.Fatal("non-URL errors pass through unchanged")
	}
}
