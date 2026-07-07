package ota

import (
	"archive/zip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// realMeta is the metadata printed from an actual full A/B OTA (a T7 GMS build).
const realMeta = `ota-property-files=payload_metadata.bin:4292:150901,payload.bin:4292:1900225064,metadata:69:671
ota-required-cache=0
ota-streaming-property-files=payload.bin:4292:1900225064,metadata:69:671
ota-type=AB
post-build=AIOAPPInc/TSAI/T7:15/GMS-v1.0/20260106:user/release-keys
post-build-incremental=eng.hwpc02.00000000.000000
post-sdk-level=35
post-security-patch-level=2026-03-05
post-timestamp=1783320806
pre-device=T7
`

// writeOTAZip builds a zip that mimics an OTA package: the metadata entry followed by a
// large "payload.bin". Both are Stored (uncompressed), like a real OTA.
func writeOTAZip(t *testing.T, meta string, payloadSize int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "update.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	mw, err := zw.CreateHeader(&zip.FileHeader{Name: metadataPath, Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	mw.Write([]byte(meta))
	pw, err := zw.CreateHeader(&zip.FileHeader{Name: "payload.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	pw.Write(make([]byte, payloadSize))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInspectFull(t *testing.T) {
	dir := writeOTAZip(t, realMeta, 8<<20) // 8 MB dummy payload
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	m, err := Inspect(context.Background(), srv.URL+"/update.zip")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if m.OTAType != "AB" {
		t.Errorf("OTAType = %q, want AB", m.OTAType)
	}
	if m.IsIncremental {
		t.Errorf("IsIncremental = true, want false (full image has no pre-build)")
	}
	if m.TargetBuild != "GMS-v1.0" {
		t.Errorf("TargetBuild = %q, want GMS-v1.0", m.TargetBuild)
	}
	if m.SourceBuild != "" {
		t.Errorf("SourceBuild = %q, want empty", m.SourceBuild)
	}
	if m.Device != "T7" {
		t.Errorf("Device = %q, want T7", m.Device)
	}
}

func TestInspectIncremental(t *testing.T) {
	meta := realMeta + "pre-build=AIOAPPInc/TSAI/T7:15/GMS-2026.05.01/20260501:user/release-keys\n" +
		"pre-build-incremental=20260501\n"
	dir := writeOTAZip(t, meta, 4<<20)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	m, err := Inspect(context.Background(), srv.URL+"/update.zip")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !m.IsIncremental {
		t.Errorf("IsIncremental = false, want true (has pre-build)")
	}
	if m.SourceBuild != "GMS-2026.05.01" {
		t.Errorf("SourceBuild = %q, want GMS-2026.05.01", m.SourceBuild)
	}
	if m.TargetBuild != "GMS-v1.0" {
		t.Errorf("TargetBuild = %q, want GMS-v1.0", m.TargetBuild)
	}
}

func TestInspectNotOTA(t *testing.T) {
	dir := t.TempDir()
	// a zip without the metadata entry
	f, _ := os.Create(filepath.Join(dir, "x.zip"))
	zw := zip.NewWriter(f)
	w, _ := zw.Create("hello.txt")
	w.Write([]byte("hi"))
	zw.Close()
	f.Close()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	if _, err := Inspect(context.Background(), srv.URL+"/x.zip"); err == nil {
		t.Error("expected error for a zip without OTA metadata")
	}
}
