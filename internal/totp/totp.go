// Package totp computes RFC 6238 TOTP codes for the offline kiosk-exit feature.
// The server displays the current code for an admin to read to an on-site
// technician; the device verifies the same code locally (see the client Totp.java).
package totp

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultDigits = 6
	DefaultPeriod = 60 // unlock code rotates every 60s
)

// Code returns the TOTP code for a base32 seed at time t.
func Code(base32Seed string, t time.Time, digits, period int) (string, error) {
	if digits <= 0 {
		digits = DefaultDigits
	}
	if period <= 0 {
		period = DefaultPeriod
	}
	key, err := decodeBase32(base32Seed)
	if err != nil {
		return "", err
	}
	counter := uint64(t.Unix() / int64(period))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod), nil
}

// SecondsRemaining is how long the current code stays valid.
func SecondsRemaining(t time.Time, period int) int {
	if period <= 0 {
		period = DefaultPeriod
	}
	return period - int(t.Unix()%int64(period))
}

func decodeBase32(s string) ([]byte, error) {
	s = strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(s, " ", "")))
	if pad := len(s) % 8; pad != 0 {
		s += strings.Repeat("=", 8-pad)
	}
	return base32.StdEncoding.DecodeString(s)
}
