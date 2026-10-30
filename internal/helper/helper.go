// Package helper describes the boot helper: a tiny Android app (see bootadb/) that switches
// Wireless debugging back on after a TV restarts (or after Android switches it off by itself)
// and stops its pairing keys expiring. tv-patcher installs it on each TV.
package helper

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sloccy/morphe-tv-patcher/internal/apk"
)

const (
	// Package is the helper app's package name.
	Package = "com.sloccy.bootadb"
	// Activity opens the helper once, which takes the app out of Android's "stopped" state
	// (a stopped app never receives the boot broadcast).
	Activity = Package + "/.MainActivity"
	// Permission is what the helper needs to change the setting; tv-patcher grants it over ADB.
	Permission = "android.permission.WRITE_SECURE_SETTINGS"
	// WatchJobID is the helper's watch job (JOB_ID_WATCH in Adb.java), which fires within
	// seconds of the TV writing adb_wifi_enabled.
	WatchJobID = "2"
	// JobID is the helper's backstop job, which re-checks the settings every 15 minutes (JOB_ID
	// in bootadb/src/com/sloccy/bootadb/Adb.java). tv-patcher runs it whenever it connects, so
	// the settings are applied right away rather than at the job's own pace.
	JobID = "1"
)

// Wanted are the global settings the helper exists to keep, which tv-patcher verifies over ADB
// after installing it. It mirrors WANTED in bootadb/src/com/sloccy/bootadb/Adb.java; only the
// two that decide whether a TV can be reached at all are checked, not the privacy settings.
var Wanted = []struct{ Key, Value, What string }{
	{"adb_wifi_enabled", "1", "Wireless debugging"},
	// Android deletes an ADB key it has not seen for adb_allowed_connection_time (7 days by
	// default), which unpairs this server from a TV it simply had no work for. A TV that will
	// not keep 0 has to be connected to inside that week; see keyExpirySafety in the runner.
	{"adb_allowed_connection_time", "0", "the ADB key expiry (0 = this server's pairing never expires)"},
}

// Bundle is the helper APK shipped in the image.
type Bundle struct {
	Path    string
	Version string // versionName, which the build sets equal to the version code
}

// Load reads the bundled APK and checks that it is the helper.
func Load(path string) (*Bundle, error) {
	m, err := apk.ReadManifest(path)
	if err != nil {
		return nil, err
	}
	if err := Check(m); err != nil {
		return nil, err
	}
	return &Bundle{Path: path, Version: strconv.FormatUint(uint64(m.VersionCode), 10)}, nil
}

// Check says whether a manifest is the helper with what it needs: the right package, the two
// permissions, a launchable activity, and the boot receiver.
func Check(m *apk.Manifest) error {
	if m.Package != Package {
		return fmt.Errorf("helper: package is %q, want %q", m.Package, Package)
	}
	if m.VersionCode == 0 {
		return errors.New("helper: no version code")
	}
	for _, p := range []string{Permission, "android.permission.RECEIVE_BOOT_COMPLETED"} {
		if !has(m.Permissions, p) {
			return fmt.Errorf("helper: missing permission %s", p)
		}
	}
	for _, c := range []string{".MainActivity", ".BootReceiver", ".KeepOnJob", ".WatchJob"} {
		if !has(m.Components, c) {
			return fmt.Errorf("helper: missing component %s", strings.TrimPrefix(c, "."))
		}
	}
	return nil
}

func has(list []string, want string) bool { return slices.Contains(list, want) }
