package fleetkey

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
)

func TestValidateSealFingerprint(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	if _, err := ValidatePrivate(pemText); err != nil {
		t.Fatal(err)
	}
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	smallPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(small)}))
	if _, err := ValidatePrivate(smallPEM); err == nil {
		t.Fatal("1024-bit key accepted")
	}
	if _, err := ValidatePrivate("not pem"); err == nil {
		t.Fatal("garbage accepted")
	}
	pub := base64.StdEncoding.EncodeToString(make([]byte, 524)) + " user@host"
	fp, err := Fingerprint(pub)
	if err != nil || len(fp) != 12 {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
	if _, err := Fingerprint("nope"); err == nil {
		t.Fatal("bad public key accepted")
	}
	sealed, err := Seal("secret", pemText)
	if err != nil {
		t.Fatal(err)
	}
	if sealed == pemText || len(sealed) == 0 {
		t.Fatal("not sealed")
	}
	got, err := Open("secret", sealed)
	if err != nil || got != pemText {
		t.Fatalf("open: %v", err)
	}
	if _, err := Open("other", sealed); err == nil {
		t.Fatal("opened with the wrong secret")
	}
	if _, err := Seal("", pemText); err == nil {
		t.Fatal("sealed with no secret")
	}
}
