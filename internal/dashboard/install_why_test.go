package dashboard

import (
	"strings"
	"testing"
)

const realLog = `11-19 10:22:01.100  1000  812  830 W PackageManager: App package must target at least SDK version 24, but found 17
11-19 10:22:01.123  1000  812  830 E PackageInstallerSession: Marking session 1234 as failed: INSTALL_FAILED_DEPRECATED_SDK_VERSION: App package must target at least SDK version 24, but found 17
`

func TestParseInstallFailureRealLog(t *testing.T) {
	r := ParseInstallFailure(realLog, "com.cam.app")
	if r.Code != "INSTALL_FAILED_DEPRECATED_SDK_VERSION" {
		t.Fatalf("code %q", r.Code)
	}
	if !strings.Contains(r.Plain, "SDK 17") || !strings.Contains(r.Plain, "24 or newer") {
		t.Fatalf("plain %q", r.Plain)
	}
	if !strings.Contains(r.Raw, "PackageInstallerSession") {
		t.Fatalf("raw lost: %q", r.Raw)
	}
}

func TestParseInstallFailureWithoutCode(t *testing.T) {
	r := ParseInstallFailure("W PackageManager: App package must target at least SDK version 24, but found 17", "")
	if r.Code != "INSTALL_FAILED_DEPRECATED_SDK_VERSION" || r.Plain == "" {
		t.Fatalf("%+v", r)
	}
}

func TestParseInstallFailurePrefersLastAndPackage(t *testing.T) {
	log := "E PackageInstallerSession: Marking session 1 as failed: INSTALL_FAILED_VERSION_DOWNGRADE: Downgrade detected for com.a.b\n" +
		"E PackageInstallerSession: Marking session 2 as failed: INSTALL_FAILED_INSUFFICIENT_STORAGE: no room for com.cam.app\n" +
		"E PackageInstallerSession: Marking session 3 as failed: INSTALL_FAILED_TEST_ONLY: other\n"
	if r := ParseInstallFailure(log, "com.cam.app"); r.Code != "INSTALL_FAILED_INSUFFICIENT_STORAGE" {
		t.Fatalf("package line should win: %q", r.Code)
	}
	if r := ParseInstallFailure(log, ""); r.Code != "INSTALL_FAILED_TEST_ONLY" {
		t.Fatalf("last should win: %q", r.Code)
	}
}

func TestInstallPlainCodes(t *testing.T) {
	for code, want := range map[string]string{
		"INSTALL_FAILED_UPDATE_INCOMPATIBLE":    "different key",
		"INSTALL_FAILED_VERSION_DOWNGRADE":      "older",
		"INSTALL_FAILED_INSUFFICIENT_STORAGE":   "storage",
		"INSTALL_FAILED_NO_MATCHING_ABIS":       "processor",
		"INSTALL_FAILED_MISSING_SHARED_LIBRARY": "library",
		"INSTALL_FAILED_DUPLICATE_PERMISSION":   "permission",
		"INSTALL_FAILED_TEST_ONLY":              "test-only",
		"INSTALL_FAILED_OLDER_SDK":              "newer Android",
	} {
		if got := installPlain(code, ""); !strings.Contains(got, want) {
			t.Errorf("%s -> %q, want %q", code, got, want)
		}
	}
	if got := installPlain("INSTALL_FAILED_SOMETHING_NEW", "x"); got != "" {
		t.Errorf("unknown code should have no sentence, got %q", got)
	}
	r := ParseInstallFailure("install failed: INSTALL_FAILED_SOMETHING_NEW: odd thing", "")
	if r.Code != "INSTALL_FAILED_SOMETHING_NEW" || r.Message != "odd thing" || r.Plain != "" {
		t.Fatalf("%+v", r)
	}
}

func TestGenericInstallOutput(t *testing.T) {
	if !genericInstallOutput("install failed: status=1 legacy=-999") {
		t.Error("status=1 legacy=-999 is generic")
	}
	if genericInstallOutput("install failed: INSTALL_FAILED_TEST_ONLY") {
		t.Error("a coded failure is not generic")
	}
	if genericInstallOutput("SocketTimeoutException") || genericInstallOutput("") {
		t.Error("a transient or empty result is not worth a log read")
	}
}

func TestDiagnoseInstallCommandIsFixed(t *testing.T) {
	cmd, ok := diagnoseInstallCommand("aio.app.camera")
	if !ok || !strings.HasPrefix(cmd, "logcat -d -b all | grep -E") || !strings.Contains(cmd, `aio\.app\.camera`) || !strings.HasSuffix(cmd, "tail -n 25") {
		t.Fatalf("%v %q", ok, cmd)
	}
	for _, bad := range []string{"", "a b", "a;rm -rf /", `a"b`, "a$(x)", "a|b", "a`b`", strings.Repeat("a", 256), "a\nb"} {
		if _, ok := diagnoseInstallCommand(bad); ok {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestShortReasonUsesCode(t *testing.T) {
	got := shortReason("install failed: INSTALL_FAILED_VERSION_DOWNGRADE")
	if !strings.Contains(got, "older") {
		t.Fatalf("%q", got)
	}
}
