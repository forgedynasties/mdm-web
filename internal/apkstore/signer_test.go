package apkstore

import (
	"os"
	"testing"
)

// APKSTORE_TEST_APK names a real APK and APKSTORE_TEST_SIGNER its certificate's SHA-256
// as `apksigner verify --print-certs` prints it (lowercase hex, no colons).
func TestSignerFromReaderAt(t *testing.T) {
	path, want := os.Getenv("APKSTORE_TEST_APK"), os.Getenv("APKSTORE_TEST_SIGNER")
	if path == "" {
		t.Skip("APKSTORE_TEST_APK not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	got, err := SignerFromReaderAt(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("signer = %s, want %s", got, want)
	}
}

func TestSignerRejectsNonZip(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not a zip at all")
	if _, err := SignerFromReaderAt(f, 16); err == nil {
		t.Fatal("want an error for a non-ZIP")
	}
}
