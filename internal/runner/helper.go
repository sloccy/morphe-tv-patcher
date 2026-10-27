package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/sloccy/morphe-tv-patcher/internal/helper"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

// HelperResult says what ensureHelper did on one device.
type HelperResult struct {
	Skipped   bool   // no helper APK in this build, nothing to do
	Version   string // version now on the device
	Installed bool   // it was (re)installed this time
}

// ensureHelper makes sure the boot helper is on the device, current, and allowed to change the
// Wireless debugging setting. It is best effort: callers report an error but carry on.
func (r *Runner) ensureHelper(ctx context.Context, c *conn) (HelperResult, error) {
	path := r.Cfg.BootHelperAPK
	if path == "" {
		return HelperResult{Skipped: true}, nil
	}
	if _, err := os.Stat(path); err != nil {
		return HelperResult{Skipped: true}, nil //nolint:nilerr // a build without the helper just skips this step
	}
	b, err := helper.Load(path)
	if err != nil {
		return HelperResult{}, fmt.Errorf("the bundled boot helper is unusable: %w", err)
	}
	res := HelperResult{Version: b.Version}
	have, err := r.ADB.InstalledVersion(ctx, c.serial, helper.Package)
	if err != nil {
		return res, err
	}
	if have != b.Version || !r.helperIsOurs(ctx, c, b) {
		if have != "" { // every build is signed with a new key, so an update is a reinstall
			if err := r.ADB.Uninstall(ctx, c.serial, helper.Package); err != nil {
				return res, err
			}
		}
		if ok, out := r.ADB.Install(ctx, c.serial, b.Path); !ok {
			return res, fmt.Errorf("installing the boot helper failed: %s", firstLine(out))
		}
		res.Installed = true
	}
	if out, err := r.ADB.Shell(ctx, c.serial, "pm grant "+helper.Package+" "+helper.Permission); err != nil || strings.Contains(out, "Exception") {
		return res, fmt.Errorf("this TV would not let the boot helper change the Wireless debugging setting: %s", firstLine(out+fmt.Sprint(err)))
	}
	if res.Installed { // the first launch frees the app to receive boot broadcasts
		if _, err := r.ADB.Shell(ctx, c.serial, "am start -n "+helper.Activity); err != nil {
			return res, fmt.Errorf("could not open the boot helper once: %w", err)
		}
		// Opening it only schedules the job (an exported activity must do nothing privileged),
		// so run that job now to apply the settings. Best effort: the read below is the real test.
		_, _ = r.ADB.Shell(ctx, c.serial, "cmd jobscheduler run -f "+helper.Package+" "+helper.JobID)
	}
	if out, err := r.ADB.Shell(ctx, c.serial, "settings get global adb_wifi_enabled"); err != nil || strings.TrimSpace(out) != "1" {
		return res, fmt.Errorf("the boot helper is installed but Wireless debugging reads %q", strings.TrimSpace(out))
	}
	_ = r.Store.UpdateDeviceInfo(c.dev.ID, func(d *store.DeviceInfo) { d.Helper = b.Version })
	if res.Installed {
		slog.Info("boot helper installed", "device", c.dev.Name, "version", b.Version)
	}
	return res, nil
}

// A path as "pm path" prints it, and nothing a shell would act on (no space, quote, $ or ;).
var helperAPKPath = regexp.MustCompile(`^package:(/[A-Za-z0-9._+=~/-]+\.apk)$`)

// helperIsOurs says whether the APK installed under the helper's package name is byte-for-byte the
// one this build ships. The version alone proves nothing: bootadb/build.sh derives it from a hash of
// the public sources, so an app squatting the package name can report a matching version and would
// then be handed WRITE_SECURE_SETTINGS, which apps cannot get any other way.
func (r *Runner) helperIsOurs(ctx context.Context, c *conn, b *helper.Bundle) bool {
	out, err := r.ADB.Shell(ctx, c.serial, "pm path "+helper.Package)
	if err != nil {
		return false
	}
	m := helperAPKPath.FindStringSubmatch(firstLine(out))
	if m == nil {
		return false
	}
	out, err = r.ADB.Shell(ctx, c.serial, "sha256sum "+m[1])
	if err != nil {
		return false
	}
	want, err := fileSHA256(b.Path)
	if err != nil {
		return false
	}
	return want != "" && strings.HasPrefix(strings.TrimSpace(out), want)
}

func fileSHA256(path string) (string, error) {
	fh, err := os.Open(path) //nolint:gosec // the helper APK inside the image
	if err != nil {
		return "", err
	}
	defer func() { _ = fh.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
