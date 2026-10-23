package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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
	if have != b.Version {
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

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
