// Package fleetkey handles the one adb key pair the AIO firmware trusts: validation of
// an uploaded pair, the fingerprint the dashboard and the apps show, and sealing the
// private key at rest. The private key is only ever held in memory while a request
// needs it; on disk it is AES-256-GCM under FLEET_ADB_KEY_SECRET.
package fleetkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MinBits is the smallest RSA key accepted. adb itself generates 2048.
const MinBits = 2048

// ValidatePrivate parses a PEM RSA private key (PKCS#1 or PKCS#8) and checks its size.
func ValidatePrivate(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText) + "\n"))
	if block == nil {
		return nil, errors.New("private_key_pem is not PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("private_key_pem: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("private_key_pem: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private_key_pem: not an RSA key")
		}
		key = rk
	default:
		return nil, fmt.Errorf("private_key_pem: unexpected PEM block %q", block.Type)
	}
	if key.N.BitLen() < MinBits {
		return nil, fmt.Errorf("private_key_pem: %d-bit RSA, need %d or more", key.N.BitLen(), MinBits)
	}
	return key, nil
}

// PublicBlob decodes adb's one-line public key ("<base64> user@host") to its binary form.
func PublicBlob(pub string) ([]byte, error) {
	f := strings.Fields(strings.TrimSpace(pub))
	if len(f) == 0 {
		return nil, errors.New("public_key is empty")
	}
	b, err := base64.StdEncoding.DecodeString(f[0])
	if err != nil || len(b) < 64 {
		return nil, errors.New("public_key is not adb's base64 public key")
	}
	return b, nil
}

// Fingerprint is the first 12 hex characters of SHA-256 over the decoded public key.
func Fingerprint(pub string) (string, error) {
	b, err := PublicBlob(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12], nil
}

func aead(secret string) (cipher.AEAD, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("FLEET_ADB_KEY_SECRET is not set")
	}
	k := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts text under the secret: base64(nonce || ciphertext).
func Seal(secret, text string) (string, error) {
	g, err := aead(secret)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := g.Seal(nonce, nonce, []byte(text), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Open reverses Seal.
func Open(secret, sealed string) (string, error) {
	g, err := aead(secret)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < g.NonceSize() {
		return "", errors.New("sealed key is corrupt")
	}
	text, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], nil)
	if err != nil {
		return "", errors.New("sealed key does not open with this FLEET_ADB_KEY_SECRET")
	}
	return string(text), nil
}
