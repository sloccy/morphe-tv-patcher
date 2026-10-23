// Package helper describes the boot helper: a tiny Android app (see bootadb/) that switches
// Wireless debugging back on after a TV restarts. tv-patcher installs it on each TV.
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
)

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
	for _, c := range []string{".MainActivity", ".BootReceiver", ".KeepOnJob"} {
		if !has(m.Components, c) {
			return fmt.Errorf("helper: missing component %s", strings.TrimPrefix(c, "."))
		}
	}
	return nil
}

func has(list []string, want string) bool { return slices.Contains(list, want) }
