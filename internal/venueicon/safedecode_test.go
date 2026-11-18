package venueicon

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"testing"
)

func pngChunk(t string, d []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(len(d)))
	b.WriteString(t)
	b.Write(d)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(t), d...)))
	return b.Bytes()
}

// A header claiming 40000 x 40000 is refused before anything is allocated.
func TestSafeDecodeRefusesHugeDimensions(t *testing.T) {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 40000)
	binary.BigEndian.PutUint32(ihdr[4:], 40000)
	ihdr[8] = 8
	raw := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...)
	raw = append(raw, pngChunk("IEND", nil)...)
	if _, err := SafeDecode(raw); err == nil {
		t.Fatal("a 1600-megapixel image was accepted")
	}
}

func TestSafeDecodeAcceptsNormalImage(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeDecode(b.Bytes()); err != nil {
		t.Fatalf("64px PNG refused: %v", err)
	}
}
