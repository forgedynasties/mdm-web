package apkstore

import (
	"archive/zip"
	"bytes"
	"os"
	"testing"
)

func zipOf(t *testing.T, files map[string]int) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, n := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(make([]byte, n))
	}
	zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

func TestBundleParts(t *testing.T) {
	// APKPure's Gboard: no manifest.json, the base named after the package, feature splits.
	info, err := bundleParts(zipOf(t, map[string]int{"com.google.android.inputmethod.latin.apk": 900, "brella_feature_split.apk": 400, "config.xxhdpi.apk": 10, "icon.png": 5}))
	if err != nil || info.Base != "com.google.android.inputmethod.latin.apk" || len(info.Splits) != 2 {
		t.Errorf("largest non-config APK is the base: %+v %v", info, err)
	}
	// bundletool / SAI: base.apk even when a split is bigger.
	info, err = bundleParts(zipOf(t, map[string]int{"base.apk": 10, "split_config.arm64_v8a.apk": 900}))
	if err != nil || info.Base != "base.apk" {
		t.Errorf("base.apk wins: %+v %v", info, err)
	}
	if _, err := bundleParts(zipOf(t, map[string]int{"a.apk": 10, "Android/obb/x/main.1.x.obb": 10})); err != ErrBundleOBB {
		t.Errorf("OBB refused, got %v", err)
	}
	if _, err := bundleParts(zipOf(t, map[string]int{"readme.txt": 10})); err == nil {
		t.Error("a ZIP with no APKs is refused")
	}
}

// TestRealBundle reads a real bundle when XAPK names one (not in CI).
func TestRealBundle(t *testing.T) {
	p := os.Getenv("XAPK")
	if p == "" {
		t.Skip("set XAPK to a bundle file")
	}
	tmp, info, err := BundleBaseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	m, err := ParseFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(tmp)
	defer f.Close()
	fi, _ := f.Stat()
	cert, err := firstCertFromReaderAt(f, fi.Size())
	t.Logf("base %s · %d splits · %s %s %q · signer cert %d bytes, err %v", info.Base, len(info.Splits), m.Package, m.VersionName, m.Label, len(cert), err)
}
