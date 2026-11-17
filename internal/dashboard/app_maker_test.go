package dashboard

import "testing"

func TestAppMaker(t *testing.T) {
	for pkg, want := range map[string]string{
		"com.google.android.gms": "Google", "com.android.chrome": "Android", "android": "Android",
		"woyou.aidlservice.jiuiv5": "Sunmi", "org.codeaurora.bluetooth": "Qualcomm", "com.whatsapp": "Whatsapp",
		"tv.danmaku.bili": "Danmaku", "aio.app.mdmclient.dpc": "AIO", "x": "X",
	} {
		if _, got := appMaker(pkg); got != want {
			t.Errorf("appMaker(%q) = %q, want %q", pkg, got, want)
		}
	}
}
