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

// A wipe package must be recognised from either marker, and an ordinary one must not be.
// Deployments fall back to the full image for devices with no incremental, so a missed
// wipe flag is a fleet of silent factory resets.
func TestWipes(t *testing.T) {
	cases := []struct {
		name, meta, props string
		want              bool
	}{
		{"ordinary full image", realMeta, "FILE_HASH=x\nFILE_SIZE=1\n", false},
		{"ota-wipe in metadata", realMeta + "ota-wipe=yes\n", "FILE_HASH=x\n", true},
		{"POWERWASH in payload properties", realMeta, "FILE_HASH=x\nPOWERWASH=1\n", true},
		{"POWERWASH=0 is not a wipe", realMeta, "POWERWASH=0\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			f, err := os.Create(filepath.Join(dir, "update.zip"))
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(f)
			for name, body := range map[string]string{metadataPath: c.meta, "payload_properties.txt": c.props, "payload.bin": "x"} {
				w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
				if err != nil {
					t.Fatal(err)
				}
				w.Write([]byte(body))
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			f.Close()
			srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer srv.Close()
			got, err := Wipes(context.Background(), srv.URL+"/update.zip")
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("Wipes = %v, want %v", got, c.want)
			}
		})
	}
}
