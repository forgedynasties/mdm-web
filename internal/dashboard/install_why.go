package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// "Why did it fail?" (19 Nov 2026; user approved). A failed install often says only
// `install failed: status=1 legacy=-999`; the reason (INSTALL_FAILED_DEPRECATED_SDK_VERSION …)
// is in the device's logcat. A person with install rights can ask for it: that sends ONE
// fixed shell command, built here from a validated package name (nothing typed by anyone
// goes into it), and the answer is read back and phrased when a page is drawn.
//
// Security note: raw shell is super-admin only, but this command is allowed to be sent by
// install-rights roles because it is fixed and read-only (logcat -d | grep | tail). It
// still needs shell enabled by an administrator, an online device whose agent can run
// shell, and the person's access to that device. Each ask is audited as
// command.diagnose_install.

// InstallReason is a device's own words for why an install failed, and ours.
type InstallReason struct {
	Code    string // INSTALL_FAILED_… ("" when the log has none)
	Message string // the platform's text after the code, as logged
	Plain   string // a sentence for people
	Raw     string // the log lines it was read from
}

var (
	installCodeRe = regexp.MustCompile(`(INSTALL_(?:PARSE_)?FAILED_[A-Z0-9_]+)(?::\s*([^\r\n]*))?`)
	sdkTargetRe   = regexp.MustCompile(`must target at least SDK version (\d+), but found (\d+)`)
	minSDKRe      = regexp.MustCompile(`(?i)requires? (?:an )?(?:Android )?(?:SDK|API)(?: level| version)? (\d+)|minSdkVersion[^\d]*(\d+)`)
	packageNameRe = regexp.MustCompile(`^[A-Za-z0-9._]{1,255}$`)
	maxInstallRaw = 4096
)

// ParseInstallFailure reads an install failure out of a client result or a logcat dump.
// The last INSTALL_FAILED_ code wins (the log is oldest first), preferring a line that
// names pkg; a log with the SDK complaint but no code still gets its sentence.
func ParseInstallFailure(output, pkg string) InstallReason {
	var r InstallReason
	lines := strings.Split(strings.ReplaceAll(output, "\r", ""), "\n")
	best := -1
	for i, ln := range lines {
		if !installCodeRe.MatchString(ln) {
			continue
		}
		if best < 0 || pkg == "" || strings.Contains(ln, pkg) || !strings.Contains(lines[best], pkg) {
			best = i
		}
	}
	if best >= 0 {
		m := installCodeRe.FindStringSubmatch(lines[best])
		r.Code, r.Message = m[1], strings.TrimSpace(m[2])
	}
	// The detail is often on the PackageManager line next to the code, not after it.
	detail := r.Message
	if m := sdkTargetRe.FindStringSubmatch(output); m != nil && (r.Code == "" || r.Code == "INSTALL_FAILED_DEPRECATED_SDK_VERSION") {
		if r.Code == "" {
			r.Code = "INSTALL_FAILED_DEPRECATED_SDK_VERSION"
		}
		detail = m[0]
	}
	r.Plain = installPlain(r.Code, detail)
	r.Raw = trimInstallRaw(output)
	return r
}

func trimInstallRaw(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
	if len(s) > maxInstallRaw {
		s = "…" + s[len(s)-maxInstallRaw:]
	}
	return s
}

// installPlain is the sentence for an INSTALL_FAILED_ code. Unknown codes return "" so the
// caller shows the code and its message as logged.
func installPlain(code, detail string) string {
	switch strings.TrimPrefix(strings.TrimPrefix(code, "INSTALL_PARSE_"), "INSTALL_") {
	case "":
		return ""
	case "FAILED_DEPRECATED_SDK_VERSION":
		if m := sdkTargetRe.FindStringSubmatch(detail); m != nil {
			return "The app targets an Android version that is too old (SDK " + m[2] + "; this device needs " + m[1] + " or newer)."
		}
		return "The app targets an Android version that is too old for this device."
	case "FAILED_UPDATE_INCOMPATIBLE", "FAILED_SIGNATURE_MISMATCH":
		return "The app is signed with a different key than the copy already on the device."
	case "FAILED_VERSION_DOWNGRADE":
		return "The app is older than the version already installed."
	case "FAILED_INSUFFICIENT_STORAGE":
		return "The device does not have enough free storage."
	case "FAILED_NO_MATCHING_ABIS":
		return "The app has no native code for this device's processor type."
	case "FAILED_MISSING_SHARED_LIBRARY":
		return "The app needs a shared library this device does not have."
	case "FAILED_DUPLICATE_PERMISSION":
		return "The app declares a permission another installed app already owns."
	case "FAILED_TEST_ONLY":
		return "The app is marked test-only and cannot be installed as a normal app."
	case "FAILED_OLDER_SDK":
		if m := minSDKRe.FindStringSubmatch(detail); m != nil {
			for _, v := range m[1:] {
				if v != "" {
					return "The app needs a newer Android than this device runs (SDK " + v + " or newer)."
				}
			}
		}
		return "The app needs a newer Android than this device runs."
	case "FAILED_ALREADY_EXISTS":
		return "The app is already installed."
	case "FAILED_INVALID_APK", "FAILED_BAD_MANIFEST", "FAILED_NOT_APK", "FAILED_UNEXPECTED_EXCEPTION", "FAILED_NO_CERTIFICATES":
		return "The file is not a valid, signed app package."
	case "FAILED_CPU_ABI_INCOMPATIBLE":
		return "The app's native code does not fit this device's processor."
	case "FAILED_USER_RESTRICTED":
		return "Installing apps is restricted on this device."
	case "FAILED_VERIFICATION_FAILURE", "FAILED_VERIFICATION_TIMEOUT":
		return "The device's app verification (Play Protect) stopped the install."
	case "FAILED_INVALID_INSTALL_LOCATION", "FAILED_MEDIA_UNAVAILABLE":
		return "The device cannot reach the storage the app asked to install on."
	case "FAILED_CONFLICTING_PROVIDER":
		return "Another installed app already uses one of this app's content providers."
	case "FAILED_UID_CHANGED":
		return "The app's user id changed since it was first installed."
	}
	return ""
}

// genericInstallOutput reports a failure text that names no reason: status numbers only.
func genericInstallOutput(out string) bool {
	if installCodeRe.MatchString(out) || transientInstall(strings.TrimSpace(out)) {
		return false
	}
	return true
}

// diagnoseInstallCommand is the one command this feature sends. pkg must pass
// validInstallPackage; the result contains nothing else a caller could vary.
func diagnoseInstallCommand(pkg string) (string, bool) {
	if !packageNameRe.MatchString(pkg) {
		return "", false
	}
	esc := strings.ReplaceAll(pkg, ".", `\.`)
	return `logcat -d -b all | grep -E "PackageInstallerSession|PackageManager" | grep -iE "` + esc +
		`|INSTALL_FAILED|must target|signature|downgrade|Failed" | tail -n 25`, true
}

// installWhy is what a page draws beside a failed install.
type installWhy struct {
	Serial  string
	Package string
	Command string // the failed install command id, "" from the Apps tab's box
	Plain   string // the sentence, "" when none known
	Code    string
	Message string
	Raw     string
	CanAsk  bool // show the "Why did it fail?" button
	Asking  bool // asked, the device has not answered yet
	Nothing bool // asked, the answer held nothing about it
}

// Has reports there is something to draw.
func (w installWhy) Has() bool {
	return w.Plain != "" || w.Code != "" || w.CanAsk || w.Asking || w.Nothing
}

// canDiagnose: install rights, shell on, an online device whose agent runs shell, and the
// person's own access to the device.
func (h *Handler) canDiagnose(r *http.Request, d *db.Device, online bool) bool {
	return h.commandTypeAllowed(h.role(r), "install_apk") && h.cfg.ShellEnabled() && online &&
		d.Supports("shell") && h.access(r).decide("install_apk", &d.ID).Allowed
}

// installWhyOf builds the view for one failed install's output and optional diagnosis.
func (h *Handler) installWhyOf(r *http.Request, d *db.Device, online bool, pkg, cmdID, out string, diag *db.InstallDiagnosis, failedAt time.Time) installWhy {
	v := installWhy{Serial: d.SerialNumber, Package: pkg, Command: cmdID}
	if diag != nil && diag.CreatedAt.After(failedAt) {
		switch {
		case diag.Answered:
			res := ParseInstallFailure(diag.Output, pkg)
			if res.Code != "" {
				v.Plain, v.Code, v.Message, v.Raw = res.Plain, res.Code, res.Message, res.Raw
				return v
			}
			v.Nothing, v.Raw = true, res.Raw
			v.CanAsk = h.canDiagnose(r, d, online)
			return v
		case time.Since(diag.CreatedAt) < 15*time.Minute:
			v.Asking = true
			return v
		}
	}
	if res := ParseInstallFailure(out, pkg); res.Code != "" {
		v.Plain, v.Code, v.Message = res.Plain, res.Code, res.Message
		return v
	}
	v.CanAsk = pkg != "" && genericInstallOutput(out) && h.canDiagnose(r, d, online)
	return v
}

// DeviceInstallWhy is POST /devices/{serial}/install-why {command | package}: sends the
// fixed logcat command for a failed install of one package on this device.
func (h *Handler) DeviceInstallWhy(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	r.ParseForm()
	if writeCommandAuthzError(w, h.authorizeCommand(h.role(r), "install_apk")) {
		return
	}
	if !h.cfg.ShellEnabled() {
		http.Error(w, "Shell commands are disabled by an administrator.", http.StatusForbidden)
		return
	}
	d, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if !h.requireDeviceAction(w, r, "install_apk", d.ID) {
		return
	}
	if !d.Supports("shell") {
		http.Error(w, "This device's agent can't run the log read.", http.StatusBadRequest)
		return
	}
	if !h.deviceOnline(d) {
		http.Error(w, d.SerialNumber+" is offline. Ask again when it is back.", http.StatusConflict)
		return
	}
	var failedID *uuid.UUID
	pkg := strings.TrimSpace(r.FormValue("package"))
	if c := strings.TrimSpace(r.FormValue("command")); c != "" {
		id, err := uuid.Parse(c)
		if err != nil {
			http.Error(w, "Invalid command ID", http.StatusBadRequest)
			return
		}
		cmd, err := h.db.GetCommand(r.Context(), id)
		if err != nil || cmd.Type != "install_apk" || h.commandHiddenFrom(r, *cmd) {
			http.Error(w, "Command not found", http.StatusNotFound)
			return
		}
		if was, err := h.db.CommandTargetsDevice(r.Context(), id, d.ID); err != nil || !was {
			http.Error(w, "That command was not sent to this device.", http.StatusBadRequest)
			return
		}
		pkg = h.db.InstallCommandPackage(r.Context(), id)
		failedID = &id
	} else if fails, _ := h.db.WantFailures(r.Context(), d.ID); fails[pkg].Count == 0 {
		http.Error(w, "No failed install of that app on this device.", http.StatusBadRequest)
		return
	}
	text, ok := diagnoseInstallCommand(pkg)
	if !ok {
		http.Error(w, "No package to look up.", http.StatusBadRequest)
		return
	}
	payload, _ := json.Marshal(map[string]string{"cmd": text})
	cmd, err := h.db.CreateCommandBy(r.Context(), "shell", "", payload, "devices", []uuid.UUID{d.ID}, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.RecordInstallDiagnosis(r.Context(), d.ID, pkg, failedID, cmd.ID, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.pushCommand(r.Context(), cmd, "devices", []uuid.UUID{d.ID})
	h.audit(r, "command.diagnose_install", pkg, fmt.Sprintf("device=%s, cmd=%s", serial, cmd.ID))
	back := "/devices/" + serial + "?tab=apps"
	if failedID != nil {
		back = "/commands/" + failedID.String()
	}
	h.hxDoneToastEvents(w, r, back, "Asking the device why · the answer shows here in a moment", "info", "device-updated", "command-updated")
}

// diagnosisFor is the diagnosis (if any) of one failed command on one device.
func diagnosisFor(m map[uuid.UUID]db.InstallDiagnosis, dev uuid.UUID) *db.InstallDiagnosis {
	if x, ok := m[dev]; ok {
		return &x
	}
	return nil
}

// commandInstallWhys builds the command page's per-device views, keyed by serial. Only
// failed install_apk deliveries get one.
func (h *Handler) commandInstallWhys(ctx context.Context, r *http.Request, cmd *db.Command, ds []db.CommandDelivery) map[string]installWhy {
	if cmd == nil || cmd.Type != "install_apk" {
		return nil
	}
	pkg := h.db.InstallCommandPackage(ctx, cmd.ID)
	diags, _ := h.db.InstallDiagnosesForCommand(ctx, cmd.ID)
	out := map[string]installWhy{}
	for _, dl := range ds {
		if dl.Status != "failed" {
			continue
		}
		dev, err := h.db.GetDevice(ctx, dl.SerialNumber)
		if err != nil || dev == nil {
			continue
		}
		v := h.installWhyOf(r, dev, h.deviceOnline(dev), pkg, cmd.ID.String(), dl.Output, diagnosisFor(diags, dl.DeviceID), time.Time{})
		if v.Has() {
			out[dl.SerialNumber] = v
		}
	}
	return out
}

// Line is the sentence without its full stop, or the code and message as logged.
func (w installWhy) Line() string {
	switch {
	case w.Plain != "":
		return strings.TrimSuffix(w.Plain, ".")
	case w.Code != "" && w.Message != "":
		return w.Code + ": " + w.Message
	}
	return w.Code
}

// attachWantsWhy adds the reason (and the Why did it fail? button) to the "On its way"
// rows that couldn't install.
func (h *Handler) attachWantsWhy(r *http.Request, d *db.Device, rows []wantRow) {
	var fails map[string]db.WantFailure
	var diags map[string]db.InstallDiagnosis
	online := h.deviceOnline(d)
	for i := range rows {
		if rows[i].State != "failed" || rows[i].Remove {
			continue
		}
		if fails == nil {
			fails, _ = h.db.WantFailures(r.Context(), d.ID)
			diags, _ = h.db.LatestInstallDiagnoses(r.Context(), d.ID)
		}
		f := fails[rows[i].Package]
		var diag *db.InstallDiagnosis
		if x, ok := diags[rows[i].Package]; ok {
			diag = &x
		}
		v := h.installWhyOf(r, d, online, rows[i].Package, "", f.Output, diag, f.At)
		rows[i].Why = v
		if line := v.Line(); line != "" {
			rows[i].Text = "Couldn't install · " + line
			if f.Count > 1 {
				rows[i].Text += fmt.Sprintf(" · tried %d times today", f.Count)
			}
		}
	}
}
