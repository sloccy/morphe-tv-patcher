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
	// KeyExpiry is what the TV's adb_allowed_connection_time reads. "0" means the pairing keys
	// never expire, which is what the helper asks for; anything else is a countdown to being
	// unpaired (7 days by default) and makes tv-patcher connect inside that window.
	KeyExpiry string
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
		// Believe the TV, not the installer: an install that reports success and leaves the old
		// app in place would otherwise look like a settings problem further down, and the page
		// would show the version this build ships rather than the one actually running.
		if now, err := r.ADB.InstalledVersion(ctx, c.serial, helper.Package); err == nil && now != "" {
			res.Version = now
		}
		if res.Version != b.Version {
			return res, fmt.Errorf("the boot helper still reads version %q after installing %q, so the install did not take", res.Version, b.Version)
		}
	}
	if out, err := r.ADB.Shell(ctx, c.serial, "pm grant "+helper.Package+" "+helper.Permission); err != nil || strings.Contains(out, "Exception") {
		return res, fmt.Errorf("this TV would not let the boot helper change the Wireless debugging setting: %s", firstLine(out+fmt.Sprint(err)))
	}
	if res.Installed { // the first launch frees the app to receive boot broadcasts
		if _, err := r.ADB.Shell(ctx, c.serial, "am start -n "+helper.Activity); err != nil {
			return res, fmt.Errorf("could not open the boot helper once: %w", err)
		}
	}
	// Nobody ever opens the helper, so Android files it under "rare" and may defer its jobs far
	// past their 15 minutes. These two take it out of that bucket; both are best effort, because
	// not every TV's shell has them, and the reads below are the real test.
	for _, cmd := range []string{
		"dumpsys deviceidle whitelist +" + helper.Package, // persists on the TV
		"am set-standby-bucket " + helper.Package + " active",
	} {
		if _, err := r.ADB.Shell(ctx, c.serial, cmd); err != nil {
			slog.Debug("the TV would not unthrottle the boot helper", "device", c.dev.Name, "cmd", cmd, "err", err)
		}
	}
	// Opening the app only schedules the jobs (an exported activity must do nothing privileged),
	// so run the backstop job, which applies the helper's privacy settings now rather than at its
	// own pace. Nothing below depends on it having finished: it returns as soon as the job starts.
	_, _ = r.ADB.Shell(ctx, c.serial, "cmd jobscheduler run -f "+helper.Package+" "+helper.JobID)
	// Read every setting before judging any of them, so the record (and the keepalive that uses
	// it) is right even when another one is what went wrong.
	reads := make([]string, len(helper.Wanted))
	for i, w := range helper.Wanted {
		out, err := r.ADB.Shell(ctx, c.serial, "settings get global "+w.Key)
		if err == nil {
			reads[i] = strings.TrimSpace(out)
		}
	}
	// Then set what is wrong here and now, rather than leaving these two to the helper. The job
	// above is asynchronous, so reading straight after it raced the write and found the key expiry
	// still unset; and a helper that nobody ever opens can sit in Android's rare or restricted
	// standby bucket with its 15-minute job deferred for hours, all of which the TV spends counting
	// down to dropping this server's pairing. The shell holds WRITE_SECURE_SETTINGS as well, so
	// this is the same write the helper would have made. The helper still matters for every moment
	// tv-patcher is not connected: a restart, a Wi-Fi drop, a move to another access point.
	for i, w := range helper.Wanted {
		if reads[i] == w.Value {
			continue
		}
		if out, err := r.ADB.Shell(ctx, c.serial, "settings put global "+w.Key+" "+w.Value); err != nil {
			slog.Warn("could not set a TV setting", "device", c.dev.Name, "key", w.Key, "err", firstLine(out+err.Error()))
		}
		if out, err := r.ADB.Shell(ctx, c.serial, "settings get global "+w.Key); err == nil {
			reads[i] = strings.TrimSpace(out) // only the read-back counts
		}
	}
	for i, w := range helper.Wanted {
		if w.Key == "adb_allowed_connection_time" {
			res.KeyExpiry = reads[i]
		}
	}
	_ = r.Store.UpdateDeviceInfo(c.dev.ID, func(d *store.DeviceInfo) {
		d.Helper, d.KeyExpiry = res.Version, res.KeyExpiry
	})
	for i, w := range helper.Wanted {
		// "settings get" prints null for a key nobody ever wrote, so a TV still on Android's
		// 7-day default fails here rather than looking fine until it unpairs itself.
		if reads[i] != w.Value {
			return res, fmt.Errorf("the boot helper is installed but %s reads %q, not %q", w.What, reads[i], w.Value)
		}
	}
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
