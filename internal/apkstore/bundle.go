package apkstore

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Split APK bundles (XAPK, 4 Nov 2026; user: "can you make new build for standard mdm and
// firmware mdm and a server deploy that supports xapk"). An .xapk (APKPure), .apks
// (bundletool, SAI) or .apkm (APKMirror) is a ZIP of the app's APKs: the base, config
// splits (screen density, language, CPU type) and feature splits. The library stores it as
// it is, under a key ending in .xapk; the device's client unpacks it and installs every
// part in one PackageInstaller session. Package, version, icon and signer come from the
// base APK inside.

// BundleExt is the key suffix of a stored bundle.
const BundleExt = ".xapk"

// IsBundleName says whether an uploaded file's name is a split APK bundle.
func IsBundleName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.HasSuffix(n, ".xapk") || strings.HasSuffix(n, ".apks") || strings.HasSuffix(n, ".apkm")
}

// IsBundleKey says whether a library object is a bundle.
func IsBundleKey(key string) bool { return strings.HasSuffix(strings.ToLower(key), BundleExt) }

// ErrBundleOBB refuses a bundle with expansion files: they go to shared storage, which
// neither client writes.
var ErrBundleOBB = errors.New("this bundle carries expansion files (OBB), which the MDM can't install")

// BundleInfo describes a bundle's parts.
type BundleInfo struct {
	Base   string   // the base APK's entry name
	Splits []string // every other APK
}

// bundleParts lists a bundle's APKs and picks its base: the one manifest.json names, else
// base.apk, else the largest APK that isn't a config split.
func bundleParts(zr *zip.Reader) (BundleInfo, error) {
	var info BundleInfo
	var apks []*zip.File
	wantBase := ""
	for _, f := range zr.File {
		name := f.Name
		low := strings.ToLower(name)
		if strings.HasPrefix(low, "android/obb/") || strings.HasSuffix(low, ".obb") {
			return info, ErrBundleOBB
		}
		if low == "manifest.json" {
			var m struct {
				PackageName string `json:"package_name"`
			}
			if rc, err := f.Open(); err == nil {
				_ = json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&m)
				rc.Close()
			}
			if m.PackageName != "" {
				wantBase = m.PackageName + ".apk"
			}
		}
		if strings.HasSuffix(low, ".apk") && !strings.Contains(name, "/") {
			apks = append(apks, f)
		}
	}
	if len(apks) == 0 {
		return info, errors.New("no APKs inside: is it a split APK bundle?")
	}
	var base *zip.File
	for _, f := range apks {
		if (wantBase != "" && f.Name == wantBase) || (base == nil && f.Name == "base.apk") {
			base = f
		}
	}
	if base == nil {
		for _, f := range apks {
			low := strings.ToLower(f.Name)
			if strings.HasPrefix(low, "config.") || strings.HasPrefix(low, "split_config.") {
				continue
			}
			if base == nil || f.UncompressedSize64 > base.UncompressedSize64 {
				base = f
			}
		}
	}
	if base == nil {
		return info, errors.New("no base APK inside")
	}
	info.Base = base.Name
	for _, f := range apks {
		if f != base {
			info.Splits = append(info.Splits, f.Name)
		}
	}
	return info, nil
}

// BundleBaseFile copies a bundle's base APK out of a local bundle file into a temp file;
// the caller removes it.
func BundleBaseFile(bundle string) (string, BundleInfo, error) {
	zr, err := zip.OpenReader(bundle)
	if err != nil {
		return "", BundleInfo{}, fmt.Errorf("not a ZIP: %w", err)
	}
	defer zr.Close()
	info, err := bundleParts(&zr.Reader)
	if err != nil {
		return "", info, err
	}
	var base *zip.File
	for _, f := range zr.File {
		if f.Name == info.Base {
			base = f
		}
	}
	rc, err := base.Open()
	if err != nil {
		return "", info, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp("", "bundle-base-*.apk")
	if err != nil {
		return "", info, err
	}
	if _, err := io.Copy(tmp, io.LimitReader(rc, 2<<30)); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", info, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", info, err
	}
	return tmp.Name(), info, nil
}

// bundleBase brings a stored bundle into the local cache and copies its base APK out.
func (s *Store) bundleBase(ctx context.Context, key string) (string, BundleInfo, error) {
	if err := s.EnsureCached(ctx, key); err != nil {
		return "", BundleInfo{}, err
	}
	p, ok := s.CachedPath(key)
	if !ok {
		// Another fill of the same key is running; it finishes on its own.
		return "", BundleInfo{}, errors.New("the bundle is still being fetched, try again in a minute")
	}
	return BundleBaseFile(p)
}

// BundleMeta parses a stored bundle's base APK.
func (s *Store) BundleMeta(ctx context.Context, key string) (*Meta, BundleInfo, error) {
	tmp, info, err := s.bundleBase(ctx, key)
	if err != nil {
		return nil, info, err
	}
	defer os.Remove(tmp)
	m, err := ParseFile(tmp)
	return m, info, err
}

// bundleSigner is the signer of a stored bundle's base APK.
func (s *Store) bundleSigner(ctx context.Context, key string) ([]byte, error) {
	tmp, _, err := s.bundleBase(ctx, key)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return firstCertFromReaderAt(f, fi.Size())
}

// BundleFileName is the download name for a bundle of pkg.
func BundleFileName(pkg string) string { return path.Base(pkg) + BundleExt }
