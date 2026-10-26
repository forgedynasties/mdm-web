package apkstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The signing certificate of an APK, from its APK Signature Scheme v3 (else v2) block
// (26 Oct 2026). An APK pulled from a device into the library must be signed like the
// versions already there; this reads which certificate it names. It does not verify the
// signature: Android does that at install, so an APK that names our certificate without
// being signed by our key is refused by every device anyway.

var errNoSigningBlock = errors.New("no APK v2/v3 signing block")

const (
	sigBlockMagic = "APK Sig Block 42"
	sigV2ID       = 0x7109871a
	sigV3ID       = 0xf05368c0
)

// SignerSHA256 is the hex SHA-256 of the first signer's certificate of a stored APK,
// read with range requests (the end of the ZIP and the signing block only).
func (s *Store) SignerSHA256(ctx context.Context, key string) (string, error) {
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return "", fmt.Errorf("s3 head: %w", err)
	}
	size := aws.ToInt64(head.ContentLength)
	return SignerFromReaderAt(&s3ReaderAt{ctx: ctx, client: s.client, bucket: s.bucket, key: key, size: size}, size)
}

// SignerFromReaderAt finds the APK Signing Block before the ZIP central directory and
// returns the hex SHA-256 of its first signer's first certificate, v3 preferred.
func SignerFromReaderAt(r io.ReaderAt, size int64) (string, error) {
	cdOffset, err := centralDirOffset(r, size)
	if err != nil {
		return "", err
	}
	if cdOffset < 32 {
		return "", errNoSigningBlock
	}
	tail := make([]byte, 24)
	if _, err := r.ReadAt(tail, cdOffset-24); err != nil {
		return "", err
	}
	if string(tail[8:]) != sigBlockMagic {
		return "", errNoSigningBlock
	}
	blockSize := int64(binary.LittleEndian.Uint64(tail[:8]))
	start := cdOffset - blockSize - 8
	if blockSize < 24 || blockSize > 16<<20 || start < 0 {
		return "", errors.New("bad signing block size")
	}
	block := make([]byte, blockSize-24) // the ID-value pairs, after the leading size field
	if _, err := r.ReadAt(block, start+8); err != nil {
		return "", err
	}
	values := map[uint32][]byte{}
	for p := block; len(p) >= 12; {
		n := binary.LittleEndian.Uint64(p[:8])
		if n < 4 || n > uint64(len(p)-8) {
			return "", errors.New("bad signing block pair")
		}
		id := binary.LittleEndian.Uint32(p[8:12])
		values[id] = p[12 : 8+n]
		p = p[8+n:]
	}
	for _, id := range []uint32{sigV3ID, sigV2ID} {
		if v, ok := values[id]; ok {
			cert, err := firstCertificate(v)
			if err != nil {
				return "", err
			}
			sum := sha256.Sum256(cert)
			return hex.EncodeToString(sum[:]), nil
		}
	}
	return "", errNoSigningBlock
}

// firstCertificate walks signers → signer → signed data → (digests, certificates) → the
// first certificate, the same nesting in v2 and v3.
func firstCertificate(v []byte) ([]byte, error) {
	signers, err := lp(v)
	if err != nil {
		return nil, err
	}
	signer, err := lp(signers)
	if err != nil {
		return nil, err
	}
	signed, err := lp(signer)
	if err != nil {
		return nil, err
	}
	digests, err := lp(signed)
	if err != nil {
		return nil, err
	}
	certs, err := lp(signed[4+len(digests):])
	if err != nil {
		return nil, err
	}
	return lp(certs)
}

// lp reads one uint32-length-prefixed field from the start of b.
func lp(b []byte) ([]byte, error) {
	if len(b) < 4 {
		return nil, errors.New("short signing field")
	}
	n := binary.LittleEndian.Uint32(b[:4])
	if uint64(n) > uint64(len(b)-4) {
		return nil, errors.New("bad signing field length")
	}
	return b[4 : 4+n], nil
}

// centralDirOffset reads the End Of Central Directory record (within the last 64 KiB +
// 22 bytes) and returns where the central directory starts.
func centralDirOffset(r io.ReaderAt, size int64) (int64, error) {
	n := int64(65535 + 22)
	if n > size {
		n = size
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, size-n); err != nil && err != io.EOF {
		return 0, err
	}
	i := bytes.LastIndex(buf, []byte{0x50, 0x4b, 0x05, 0x06})
	if i < 0 || len(buf)-i < 22 {
		return 0, errors.New("not a ZIP: no end of central directory")
	}
	return int64(binary.LittleEndian.Uint32(buf[i+16 : i+20])), nil
}
