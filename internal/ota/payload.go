package ota

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"strings"
)

// Payload is what the legacy otautil client needs to hand an A/B package to
// update_engine: where payload.bin sits inside the zip, how long it is, and the
// payload_properties.txt lines (FILE_HASH, FILE_SIZE, METADATA_HASH, METADATA_SIZE).
type Payload struct {
	Offset  int64    `json:"payload_offset"`
	Size    int64    `json:"payload_size"`
	Headers []string `json:"payload_headers"`
}

// PayloadInfo reads the payload location and properties of an OTA zip by URL using
// range requests, like Inspect: the central directory plus one tiny entry, never
// the payload itself.
func PayloadInfo(ctx context.Context, url string) (*Payload, error) {
	size, err := objectSize(ctx, url)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(&httpReaderAt{ctx: ctx, url: url, size: size}, size)
	if err != nil {
		return nil, fmt.Errorf("not a readable zip: %w", err)
	}
	var bin, props *zip.File
	for _, f := range zr.File {
		switch f.Name {
		case "payload.bin":
			bin = f
		case "payload_properties.txt":
			props = f
		}
	}
	if bin == nil {
		return nil, fmt.Errorf("no payload.bin — not an A/B OTA package")
	}
	if bin.Method != zip.Store {
		return nil, fmt.Errorf("payload.bin is compressed; update_engine needs it stored")
	}
	off, err := bin.DataOffset()
	if err != nil {
		return nil, err
	}
	p := &Payload{Offset: off, Size: int64(bin.CompressedSize64)}
	if props != nil {
		rc, err := props.Open()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(rc, 16*1024))
		rc.Close()
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(body), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				p.Headers = append(p.Headers, line)
			}
		}
	}
	return p, nil
}
