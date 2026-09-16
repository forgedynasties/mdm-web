// Package apkstore handles APK uploads to S3 and parsing APK metadata (package
// name, version, launcher icon). Uploads go browser -> S3 directly via presigned
// PUT URLs; the server only ever reads an object back to parse it.
package apkstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/shogo82148/androidbinary/apk"
)

// Store wraps an S3 bucket for APK storage.
type Store struct {
	client   *s3.Client
	presign  *s3.PresignClient
	bucket   string
	prefix   string
	cacheDir string
	filling  sync.Map // key -> struct{}: an in-flight cache fill, so two never corrupt the .part
}

// Meta is the parsed metadata of an uploaded APK.
type Meta struct {
	Package     string
	Label       string
	VersionName string
	IconPNGB64  string // base64 PNG launcher icon, "" if none
}

// New builds a Store from env: S3_BUCKET, S3_PREFIX, AWS_REGION. Credentials come
// from the default AWS chain (the container mounts ~/.aws). Returns (nil, nil) when
// S3_BUCKET is unset, so the feature is simply disabled rather than fatal.
func New(ctx context.Context) (*Store, error) {
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return nil, nil
	}
	region := os.Getenv("AWS_REGION")
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	client := s3.NewFromConfig(cfg)
	prefix := os.Getenv("S3_PREFIX")
	cacheDir := os.Getenv("APK_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "/app/data/apk-cache"
	}
	_ = os.MkdirAll(cacheDir, 0o755)
	return &Store{
		client:   client,
		presign:  s3.NewPresignClient(client),
		bucket:   bucket,
		prefix:   prefix,
		cacheDir: cacheDir,
	}, nil
}

// Key composes the object key for a given basename under the configured prefix.
func (s *Store) Key(name string) string { return s.prefix + name }

// cachePath maps an S3 key to its local cache file (slashes flattened).
func (s *Store) cachePath(key string) string {
	return filepath.Join(s.cacheDir, strings.ReplaceAll(key, "/", "_"))
}

// CachedPath returns the local cache path and whether a complete copy exists.
func (s *Store) CachedPath(key string) (string, bool) {
	p := s.cachePath(key)
	if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
		return p, true
	}
	return p, false
}

// EnsureCached downloads the object from S3 to the local cache if not already there.
// Writes to a temp file then renames, so a partial download never looks complete.
// Safe to call repeatedly / concurrently (best-effort; a racing duplicate just wastes
// one download). Meant to run in the background so device installs serve from LAN disk.
func (s *Store) EnsureCached(ctx context.Context, key string) error {
	final := s.cachePath(key)
	if fi, err := os.Stat(final); err == nil && fi.Size() > 0 {
		return nil
	}
	// Only one fill per key at a time (register + on-access both trigger this) — two
	// concurrent appends to the same .part would corrupt it.
	if _, busy := s.filling.LoadOrStore(key, struct{}{}); busy {
		return nil
	}
	defer s.filling.Delete(key)
	// Total size up front so we know when the (possibly resumed) download is complete.
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return err
	}
	total := aws.ToInt64(head.ContentLength)
	if total <= 0 {
		return errors.New("empty object")
	}
	// Stable per-key part file so retries resume from what's already downloaded (the
	// S3 link here drops mid-transfer; without resume a large APK never finishes).
	part := final + ".part"
	var last error
	for attempt := 0; attempt < 200; attempt++ {
		off := int64(0)
		if fi, e := os.Stat(part); e == nil {
			off = fi.Size()
		}
		if off >= total { // complete
			return os.Rename(part, final)
		}
		in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
		if off > 0 {
			in.Range = aws.String(fmt.Sprintf("bytes=%d-", off))
		}
		out, e := s.client.GetObject(ctx, in)
		if e != nil {
			last = e
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		f, e := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if e != nil {
			out.Body.Close()
			return e
		}
		n, copyErr := io.Copy(f, out.Body)
		f.Close()
		out.Body.Close()
		if copyErr != nil {
			last = copyErr
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if n == 0 { // no forward progress at all — avoid a hot spin
				continue
			}
			continue
		}
	}
	if last == nil {
		last = errors.New("cache fill did not complete")
	}
	return last
}

// PresignPut returns a presigned URL the browser uses to PUT the APK straight to S3.
func (s *Store) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	in := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	out, err := s.presign.PresignPutObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// Put writes an object straight from the server. Used for small control files the MDM
// owns (the legacy OTA discovery config), not for APKs — those are uploaded by the
// browser through PresignPut so the bytes never pass through here.
func (s *Store) Put(ctx context.Context, key, contentType string, body []byte) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	_, err := s.client.PutObject(ctx, in)
	return err
}

// PresignGet returns a presigned URL for downloading an object (used by the device
// install proxy so the bucket can stay private).
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	out, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// s3ReaderAt implements io.ReaderAt over an S3 object using HTTP range GETs, so a ZIP
// reader can read just the central directory + the entries it needs instead of pulling
// the whole (possibly hundreds-of-MB) APK. It fetches in large aligned BLOCKS and
// caches them: zip/bufio issue many tiny sequential ReadAts, which would otherwise be
// thousands of round-trip range requests — block caching collapses them to a handful.
const s3Block = 8 << 20 // 8 MiB

type s3ReaderAt struct {
	ctx    context.Context
	client *s3.Client
	bucket string
	key    string
	size   int64
	mu     sync.Mutex
	cache  map[int64][]byte // blockIndex -> bytes
}

func (r *s3ReaderAt) block(idx int64) ([]byte, error) {
	r.mu.Lock()
	if b, ok := r.cache[idx]; ok {
		r.mu.Unlock()
		return b, nil
	}
	r.mu.Unlock()

	begin := idx * s3Block
	end := begin + s3Block - 1
	if end > r.size-1 {
		end = r.size - 1
	}
	out, err := r.client.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket), Key: aws.String(r.key),
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", begin, end)),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	buf := make([]byte, end-begin+1)
	if _, err := io.ReadFull(out.Body, buf); err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[int64][]byte{}
	}
	if len(r.cache) > 16 { // keep memory bounded for huge APKs
		r.cache = map[int64][]byte{}
	}
	r.cache[idx] = buf
	r.mu.Unlock()
	return buf, nil
}

func (r *s3ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		blk, err := r.block(pos / s3Block)
		if err != nil {
			return n, err
		}
		start := pos - (pos/s3Block)*s3Block
		if start >= int64(len(blk)) {
			return n, io.EOF
		}
		n += copy(p[n:], blk[start:])
	}
	return n, nil
}

// Get opens the object for streaming to a client (the device install proxy),
// honoring an optional HTTP Range header so a device that lost its connection can
// RESUME instead of restarting — otherwise a truncated slow stream loops forever.
// contentRange is non-empty when the response is partial (send it + status 206).
// The caller must Close the body.
func (s *Store) Get(ctx context.Context, key, rangeHeader string) (body io.ReadCloser, size int64, contentType, contentRange string, err error) {
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if rangeHeader != "" {
		in.Range = aws.String(rangeHeader)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return nil, 0, "", "", err
	}
	ct := "application/vnd.android.package-archive"
	if out.ContentType != nil && *out.ContentType != "" {
		ct = *out.ContentType
	}
	cr := ""
	if out.ContentRange != nil {
		cr = *out.ContentRange
	}
	return out.Body, aws.ToInt64(out.ContentLength), ct, cr, nil
}

// Parse extracts package/label/version/icon WITHOUT downloading the whole APK: it
// range-reads the object from S3 (ZIP directory + AndroidManifest + icon only).
func (s *Store) Parse(ctx context.Context, key string) (*Meta, error) {
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 head: %w", err)
	}
	size := aws.ToInt64(head.ContentLength)
	if size <= 0 {
		return nil, errors.New("empty object")
	}
	reader := &s3ReaderAt{ctx: ctx, client: s.client, bucket: s.bucket, key: key, size: size}
	pkg, err := apk.OpenZipReader(reader, size)
	if err != nil {
		return nil, fmt.Errorf("parse apk: %w", err)
	}
	defer pkg.Close()

	m := &Meta{Package: pkg.PackageName()}
	if m.Package == "" {
		return nil, errors.New("apk has no package name")
	}
	if lbl, err := pkg.Label(nil); err == nil {
		m.Label = lbl
	}
	if mf := pkg.Manifest(); true {
		if v, err := mf.VersionName.String(); err == nil {
			m.VersionName = v
		}
	}
	if icon, err := pkg.Icon(nil); err == nil && icon != nil {
		var buf bytes.Buffer
		if png.Encode(&buf, icon) == nil {
			m.IconPNGB64 = base64.StdEncoding.EncodeToString(buf.Bytes())
		}
	}
	return m, nil
}
