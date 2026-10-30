package helper

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sloccy/morphe-tv-patcher/internal/apk"
	"github.com/sloccy/morphe-tv-patcher/internal/apk/apktest"
)

var (
	perms = []string{Permission, "android.permission.RECEIVE_BOOT_COMPLETED"}
	comps = []string{".MainActivity", ".BootReceiver", ".KeepOnJob", ".WatchJob"}
)

func TestLoadAcceptsTheHelperAndReadsItsVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.apk")
	_ = os.WriteFile(p, apktest.Zip(map[string][]byte{"AndroidManifest.xml": apktest.Manifest(Package, 20261022, perms, comps)}), 0o600)
	b, err := Load(p)
	if err != nil || b.Version != "20261022" {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestCheckRejectsAnythingElse(t *testing.T) {
	cases := map[string]*apk.Manifest{
		"wrong package":      {Package: "com.other", VersionCode: 1, Permissions: perms, Components: comps},
		"no version":         {Package: Package, Permissions: perms, Components: comps},
		"no secure settings": {Package: Package, VersionCode: 1, Permissions: perms[1:], Components: comps},
		"no boot permission": {Package: Package, VersionCode: 1, Permissions: perms[:1], Components: comps},
		"no receiver":        {Package: Package, VersionCode: 1, Permissions: perms, Components: []string{".MainActivity", ".KeepOnJob", ".WatchJob"}},
		"no watch job":       {Package: Package, VersionCode: 1, Permissions: perms, Components: comps[:3]},
	}
	for name, m := range cases {
		if Check(m) == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

// BOOTADB_APK points at the APK that CI just built; without it this test is skipped.
func TestTheBuiltAPKIsTheHelper(t *testing.T) {
	path := os.Getenv("BOOTADB_APK")
	if path == "" {
		t.Skip("BOOTADB_APK not set")
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

// The helper only works if the host checks the settings the app is there to keep, so the two
// tables must not drift apart silently.
func TestWantedCoversTheKeyExpiry(t *testing.T) {
	want := map[string]string{"adb_wifi_enabled": "1", "adb_allowed_connection_time": "0"}
	if len(Wanted) != len(want) {
		t.Fatalf("%+v", Wanted)
	}
	for _, w := range Wanted {
		if want[w.Key] != w.Value || w.What == "" {
			t.Errorf("%+v", w)
		}
	}
}
