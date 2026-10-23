package runner

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

// tweakPackage labels result lines about TV settings (they are not about an app).
const tweakPackage = "tv-settings"

const processFlag = "activity_manager max_cached_processes"

// tweaksStep applies the TV settings tv-patcher is asked to keep (today the background
// process limit) while it has a connection. It reports a line only when it changed something
// or failed, and never stops the app updates.
func (r *Runner) tweaksStep(ctx context.Context, c *conn) []Result {
	n := r.Cfg.BackgroundProcessLimit
	if n <= 0 {
		return nil
	}
	want := strconv.Itoa(n)
	line := Result{Device: c.dev.Name, Package: tweakPackage, Name: "TV settings"}
	get := func() (string, error) {
		out, err := r.ADB.Shell(ctx, c.serial, "device_config get "+processFlag)
		return strings.TrimSpace(out), err
	}
	have, err := get()
	if err != nil {
		line.Message = "TV settings: could not read the background process limit: " + err.Error()
		return []Result{line}
	}
	changed := false
	if have != want {
		out, err := r.ADB.Shell(ctx, c.serial, "device_config put "+processFlag+" "+want)
		if got, _ := get(); err != nil || got != want {
			line.Message = fmt.Sprintf("TV settings: could not set the background process limit to %s (it reads %q): %s", want, got, firstLine(out))
			return []Result{line}
		}
		changed = true
	}
	if r.Cfg.PersistTVFlags { // keeps the value across restarts by stopping Google's flag sync on the TV
		if out, err := r.ADB.Shell(ctx, c.serial, "device_config set_sync_disabled_for_tests persistent"); err != nil {
			line.Message = "TV settings: could not keep the background process limit across restarts: " + firstLine(out+err.Error())
			return []Result{line}
		}
	}
	if !changed {
		return nil
	}
	line.OK, line.Changed = true, true
	line.Message = fmt.Sprintf("TV settings: background process limit set to %s (it was %s)", want, orDefault(have))
	return []Result{line}
}

func orDefault(v string) string {
	if v == "" || v == "null" {
		return "the default"
	}
	return v
}

// QuietCheck is what a connection-free test found.
type QuietCheck struct {
	Reachable bool
	Port      int
	Helper    string // boot helper version on record, if any
	SeenAt    string // when tv-patcher last really connected
}

// CheckDevice looks for the wireless debugging port without opening an ADB session, so the
// TV shows no notice. It cannot tell whether the pairing is still valid; a full test can.
func (r *Runner) CheckDevice(ctx context.Context, d store.Device) QuietCheck {
	var known []int
	if p := r.Store.Port(d.ID); p > 0 {
		known = append(known, p)
	}
	port, ok := r.ADB.Reachable(ctx, d.Host, known...)
	q := QuietCheck{Reachable: ok, Port: port}
	if info, have := r.Store.DeviceInfo(d.ID); have {
		q.Helper, q.SeenAt = info.Helper, info.SeenAt
	}
	if ok {
		_ = r.Store.SetPort(d.ID, port)
	}
	return q
}
