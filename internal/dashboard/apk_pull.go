package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Save to library (26 Oct 2026; the user: "do option 2", then "also implement auto
// pulling from standard devices and keeping the library up to date", "need to pull
// latest available only"). A Standard-client device (0.11.0+, capability apk_pull)
// uploads an installed app's APK straight to the library's S3 bucket, the way the
// dashboard's own uploads go, and the server then adds it to the library:
//
//	command pull_apk {package, version}  →  device asks for an upload URL
//	(APKPullUploadURL) → PUTs the file → reports the key (APKPullRegister).
//
// RunAPKAutoPull asks for the newest build of each of our apps that the devices run
// whenever the library has neither it nor anything newer.

// apkPullRetry: a package@version is asked for once, then left alone this long.
const apkPullRetry = 6 * time.Hour

// apkPullPerTick caps how many pulls one run of the job starts.
const apkPullPerTick = 3

var packageNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

// pullCommand loads a pull_apk command and checks it is the device's to answer.
func (h *Handler) pullCommand(ctx context.Context, deviceID, commandID uuid.UUID) (*db.Command, string, error) {
	cmd, err := h.db.GetCommand(ctx, commandID)
	if err != nil || cmd == nil || cmd.Type != "pull_apk" {
		return nil, "", errors.New("no such pull")
	}
	if ok, err := h.db.CommandTargetsDevice(ctx, commandID, deviceID); err != nil || !ok {
		return nil, "", errors.New("command does not target device")
	}
	var p struct {
		Package string `json:"package"`
	}
	_ = json.Unmarshal(cmd.Payload, &p)
	if !packageNameRE.MatchString(p.Package) {
		return nil, "", errors.New("pull names no package")
	}
	return cmd, p.Package, nil
}

// APKPullUploadURL hands the device a presigned PUT for one pull's APK.
func (h *Handler) APKPullUploadURL(ctx context.Context, deviceID, commandID uuid.UUID) (string, string, error) {
	if h.apk == nil {
		return "", "", errors.New("APK storage is not configured")
	}
	if _, _, err := h.pullCommand(ctx, deviceID, commandID); err != nil {
		return "", "", err
	}
	key := h.apk.Key("pulled-" + uuid.NewString() + ".apk")
	url, err := h.apk.PresignPut(ctx, key, "application/vnd.android.package-archive", 30*time.Minute)
	if err != nil {
		return "", "", err
	}
	return url, key, nil
}

// APKPullRegister adds an uploaded APK to the library and says what happened, for the
// command's result. The APK must be the package the pull asked for.
func (h *Handler) APKPullRegister(ctx context.Context, deviceID, commandID uuid.UUID, key string) (string, error) {
	if h.apk == nil {
		return "", errors.New("APK storage is not configured")
	}
	cmd, pkg, err := h.pullCommand(ctx, deviceID, commandID)
	if err != nil {
		return "", err
	}
	// Only a key this server minted for a pull.
	if !strings.HasPrefix(key, h.apk.Key("pulled-")) || !strings.HasSuffix(key, ".apk") || strings.Contains(key, "..") {
		return "", errors.New("unknown upload")
	}
	meta, err := h.apk.Parse(ctx, key)
	if err != nil {
		return "", fmt.Errorf("could not read the uploaded APK: %w", err)
	}
	if meta.Package != pkg {
		return "", fmt.Errorf("the upload is %s, not %s", meta.Package, pkg)
	}
	apps, _ := h.db.ListApps(ctx)
	for _, a := range apps {
		if a.PackageName == meta.Package && a.VersionName == meta.VersionName {
			return fmt.Sprintf("%s %s is already in the library", meta.Package, meta.VersionName), nil
		}
	}
	// Signed like the package's versions already in the library, so a device can't put
	// something else in under our name (the library feeds automatic installs). The first
	// upload of a package sets its certificate.
	signer, err := h.apk.SignerSHA256(ctx, key)
	if err != nil {
		return "", fmt.Errorf("could not read the APK's signature: %w", err)
	}
	if want, err := h.librarySigner(ctx, meta.Package); err != nil {
		return "", err
	} else if want != "" && want != signer {
		log.Printf("[apk-pull] %s from device %s refused: signed %s…, the library's are %s…", meta.Package, deviceID, signer[:12], want[:12])
		return "", fmt.Errorf("signed with a different key than %s in the library; not added", meta.Package)
	}
	name := meta.Label
	if name == "" {
		name = meta.Package
	}
	base := ""
	if len(h.publicOrigins) > 0 {
		base = h.publicOrigins[0]
	}
	app, err := h.db.CreateS3App(ctx, name, meta.Package, meta.VersionName, key, base)
	if err != nil {
		return "", fmt.Errorf("could not save it to the library: %w", err)
	}
	_ = h.db.SetAppSigner(ctx, app.ID, signer)
	if icon := meta.IconPNGB64; icon != "" && len(icon) <= 128*1024 {
		_ = h.db.UpsertAppIcon(ctx, meta.Package, icon)
	}
	go func() {
		if err := h.apk.EnsureCached(context.Background(), key); err != nil {
			log.Printf("[apk-pull] cache %s: %v", key, err)
		}
	}()
	_ = h.db.InsertAuditDevice(ctx, orDefault(cmd.CreatedBy, "auto-pull"), "app.library_add", meta.Package,
		fmt.Sprintf("%s %s pulled from a device (app %s)", meta.Package, meta.VersionName, app.ID), deviceID)
	log.Printf("[apk-pull] %s %s added to the library from device %s", meta.Package, meta.VersionName, deviceID)
	return fmt.Sprintf("%s %s added to the library", meta.Package, meta.VersionName), nil
}

// librarySigner is the certificate the library's uploads of pkg are signed with, ""
// when it has none. Uploads stored before signers were kept are read now (and saved).
func (h *Handler) librarySigner(ctx context.Context, pkg string) (string, error) {
	rows, err := h.db.AppSigners(ctx, pkg)
	if err != nil {
		return "", err
	}
	for _, a := range rows {
		if a.Signer == "" && a.S3Key != "" {
			if s, err := h.apk.SignerSHA256(ctx, a.S3Key); err == nil {
				a.Signer = s
				_ = h.db.SetAppSigner(ctx, a.ID, s)
			}
		}
		if a.Signer != "" {
			return a.Signer, nil
		}
	}
	return "", nil
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// queuePull sends one device a pull_apk for pkg (version is what it reported, for the
// job's bookkeeping).
func (h *Handler) queuePull(ctx context.Context, deviceID uuid.UUID, pkg, version, by string) error {
	payload, _ := json.Marshal(map[string]string{"package": pkg, "version": version})
	cmd, err := h.db.CreateCommandBy(ctx, "pull_apk", "", payload, "devices", []uuid.UUID{deviceID}, by)
	if err != nil {
		return err
	}
	h.pushCommand(ctx, cmd, "devices", []uuid.UUID{deviceID})
	return nil
}

// RunAPKAutoPull keeps the library up to date from the Standard-client devices: for each
// of our apps, the newest version any of them runs is pulled when the library has
// neither it nor a newer one. Minute job; a few pulls per run, each asked for once in
// apkPullRetry.
func (h *Handler) RunAPKAutoPull(ctx context.Context) {
	if h.apk == nil {
		return
	}
	seenWithin := time.Duration(h.cfg.CheckinInterval()*3) * time.Second
	cands, err := h.db.APKPullCandidates(ctx, time.Now().Add(-seenWithin))
	if err != nil {
		log.Printf("[apk-pull] candidates: %v", err)
		return
	}
	if len(cands) == 0 {
		return
	}
	apps, err := h.db.ListApps(ctx)
	if err != nil {
		log.Printf("[apk-pull] library: %v", err)
		return
	}
	recent, err := h.db.RecentAPKPulls(ctx, apkPullRetry)
	if err != nil {
		log.Printf("[apk-pull] recent: %v", err)
		return
	}
	for _, j := range pickAutoPulls(cands, apps, recent, apkPullPerTick) {
		if err := h.queuePull(ctx, j.DeviceID, j.Package, j.Version, "auto-pull"); err != nil {
			log.Printf("[apk-pull] %s %s from %s: %v", j.Package, j.Version, j.Serial, err)
			continue
		}
		log.Printf("[apk-pull] asked %s for %s %s (newer than the library)", j.Serial, j.Package, j.Version)
	}
}

// pickAutoPulls chooses, per package, the newest version on the devices (cands come
// venue-less devices first, then freshest, so the first device seen with it is the
// one asked) when the
// library holds neither that version nor a newer one and nobody asked for it lately.
func pickAutoPulls(cands []db.PullCandidate, apps []db.App, recent map[string]bool, max int) []db.PullCandidate {
	newestLib := map[string]string{}
	have := map[string]bool{}
	for _, a := range apps {
		have[a.PackageName+"@"+a.VersionName] = true
		if v, ok := newestLib[a.PackageName]; !ok || versionNewer(a.VersionName, v) {
			newestLib[a.PackageName] = a.VersionName
		}
	}
	best := map[string]db.PullCandidate{}
	var order []string
	for _, c := range cands {
		cur, ok := best[c.Package]
		if !ok {
			order = append(order, c.Package)
		}
		if !ok || versionNewer(c.Version, cur.Version) {
			best[c.Package] = c
		}
	}
	var out []db.PullCandidate
	for _, pkg := range order {
		c := best[pkg]
		if have[c.Package+"@"+c.Version] || recent[c.Package+"@"+c.Version] {
			continue
		}
		if lib, ok := newestLib[c.Package]; ok && !versionNewer(c.Version, lib) {
			continue
		}
		out = append(out, c)
		if len(out) >= max {
			break
		}
	}
	return out
}

// DeviceSaveToLibrary is POST /devices/{serial}/apps/save: the device page's "Save to
// library" on one installed app.
func (h *Handler) DeviceSaveToLibrary(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	d, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || d == nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.commandTypeAllowed(h.role(r), "pull_apk") {
		http.Error(w, "Your role can't add apps to the library.", http.StatusForbidden)
		return
	}
	if !d.CapSet()[prod.CapApkPull] {
		http.Error(w, "This device's client can't upload apps (Standard client 0.11.0 or later).", http.StatusBadRequest)
		return
	}
	pkg := strings.TrimSpace(r.FormValue("package"))
	if !packageNameRE.MatchString(pkg) {
		http.Error(w, "Pick an app.", http.StatusBadRequest)
		return
	}
	version := ""
	if pkgs, err := h.db.GetDevicePackages(r.Context(), d.ID); err == nil {
		for _, p := range pkgs {
			if p.PackageName == pkg {
				version = p.VersionName
			}
		}
	}
	if err := h.queuePull(r.Context(), d.ID, pkg, version, h.currentUsername(r)); err != nil {
		http.Error(w, "Could not send it", http.StatusInternalServerError)
		return
	}
	h.auditDev(r, "device.save_to_library", d.ID, serial, pkg+" "+version)
	h.hxDoneToast(w, r, "/devices/"+serial+"?tab=apps", "Saving "+pkg+" to the library…", "success")
}
