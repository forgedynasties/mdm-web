package ota

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"mdm/internal/safehttp"
)

// Wipes reports whether installing the OTA package at url factory-resets the device.
// ota_from_target_files --wipe_user_data marks it twice, and either is enough:
// ota-wipe=yes in META-INF/com/android/metadata, and POWERWASH=1 in
// payload_properties.txt (what update_engine acts on). Like Inspect it reads only the
// zip's central directory and those two small entries, never the payload.
func Wipes(ctx context.Context, url string) (bool, error) {
	if err := safehttp.CheckURL(url, false); err != nil {
		return false, err
	}
	size, err := objectSize(ctx, url)
	if err != nil {
		return false, err
	}
	zr, err := zip.NewReader(&httpReaderAt{ctx: ctx, url: url, size: size}, size)
	if err != nil {
		return false, fmt.Errorf("not a readable zip: %w", err)
	}
	read := func(f *zip.File) (string, error) {
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 64*1024))
		return string(b), err
	}
	for _, f := range zr.File {
		switch f.Name {
		case metadataPath:
			body, err := read(f)
			if err != nil {
				return false, err
			}
			if parseKV(body)["ota-wipe"] == "yes" {
				return true, nil
			}
		case "payload_properties.txt":
			body, err := read(f)
			if err != nil {
				return false, err
			}
			for _, line := range strings.Split(body, "\n") {
				if strings.TrimSpace(line) == "POWERWASH=1" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// WipeStore is where RecordWipe keeps the answer (db.DB).
type WipeStore interface {
	SetOTAPackageWipe(ctx context.Context, id int, wipe bool) error
}

// RecordWipe reads whether package id at url factory-resets the device and stores it,
// trusting a publisher's claim too. When the zip cannot be read and nobody claimed a
// wipe, the package stays unchecked for the startup backfill rather than being recorded
// as safe. Returns what was recorded.
func RecordWipe(ctx context.Context, st WipeStore, id int, url string, claimed bool) bool {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	detected, err := Wipes(cctx, url)
	if err != nil && !claimed {
		log.Printf("[ota] package %d: wipe flag not read (%v); left for the backfill", id, err)
		return false
	}
	wipe := detected || claimed
	_ = st.SetOTAPackageWipe(ctx, id, wipe)
	return wipe
}
