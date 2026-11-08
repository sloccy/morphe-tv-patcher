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

// fixedPortStep pins ADB to a port that never changes, when asked for one (TV_FIXED_ADB_PORT).
// Wireless debugging picks a fresh random port every time Android switches it back on, so the
// port has to be found again each time; a fixed one is simply dialled. The property is what
// adbd reads at boot; restart is true only where nothing follows in the same request, because
// restarting adbd drops the connection this step is running over.
//
// It reports a line only when it changed something or failed, and never stops the app updates.
func (r *Runner) fixedPortStep(ctx context.Context, c *conn, restart bool) []Result {
	want := r.Cfg.TVFixedADBPort
	if want <= 0 {
		return nil
	}
	port := strconv.Itoa(want)
	line := Result{Device: c.dev.Name, Package: tweakPackage, Name: "TV settings"}
	have, err := r.ADB.Shell(ctx, c.serial, "getprop persist.adb.tcp.port")
	if err != nil {
		line.Message = "TV settings: could not read the fixed ADB port: " + err.Error()
		return []Result{line}
	}
	if strings.TrimSpace(have) == port {
		return nil // already pinned; runner.connect dials it anyway
	}
	// Set by `adb tcpip` and cleared by a reboot: adbd is already listening there.
	if live, _ := r.ADB.Shell(ctx, c.serial, "getprop service.adb.tcp.port"); strings.TrimSpace(live) == port {
		return nil
	}
	// adbd only listens on a plain TCP port while ADB debugging itself is on.
	if out, err := r.ADB.Shell(ctx, c.serial, "settings put global adb_enabled 1"); err != nil {
		line.Message = "TV settings: could not switch ADB debugging on for the fixed port: " + firstLine(out+err.Error())
		return []Result{line}
	}
	// Some firmware (Android 14 on the Bedroom TV) will not let the adb shell write the property,
	// and some accepts it without keeping it. Either way adbd can still be told to listen there
	// until the TV restarts, which covers Wi-Fi drops and access point changes.
	out, err := r.ADB.Shell(ctx, c.serial, "setprop persist.adb.tcp.port "+port)
	if err != nil {
		return r.runtimeFixedPort(ctx, c, restart, line, "would not let the adb shell set persist.adb.tcp.port: "+firstLine(out+err.Error()))
	}
	// Not every firmware keeps the property, so believe only what it reads back.
	if got, _ := r.ADB.Shell(ctx, c.serial, "getprop persist.adb.tcp.port"); strings.TrimSpace(got) != port {
		return r.runtimeFixedPort(ctx, c, restart, line, fmt.Sprintf("would not keep persist.adb.tcp.port (it reads %q)", strings.TrimSpace(got)))
	}
	line.OK, line.Changed = true, true
	line.Message = fmt.Sprintf("TV settings: ADB will listen on the fixed port %s from the next restart, so its random port no longer has to be found", port)
	if restart {
		if err := r.ADB.TCPIP(ctx, c.serial, want); err != nil {
			line.Message = fmt.Sprintf("TV settings: the fixed ADB port %s is set but adbd would not restart on it: %v. It takes effect when the TV next restarts", port, err)
			line.OK, line.Changed = false, false
			return []Result{line}
		}
		line.Message = fmt.Sprintf("TV settings: ADB now also listens on the fixed port %s, so its random port no longer has to be found", port)
	}
	return []Result{line}
}

// runtimeFixedPort is the fallback for a TV that will not keep the port across restarts: adbd is
// told to listen on it for as long as the TV stays up. Restarting adbd drops the connection, so
// during a run it is only noted (c.tcpipDue) and done by finishFixedPort once the work is done.
func (r *Runner) runtimeFixedPort(ctx context.Context, c *conn, restart bool, line Result, why string) []Result {
	if !restart {
		c.tcpipDue = true
		return nil
	}
	return []Result{r.listenOnFixedPort(ctx, c, line, why)}
}

// finishFixedPort restarts adbd on the fixed port for TVs that need it, at the end of a run.
func (r *Runner) finishFixedPort(ctx context.Context, conns []conn) {
	for i := range conns {
		c := &conns[i]
		if !c.tcpipDue || !c.connected {
			continue
		}
		line := Result{Device: c.dev.Name, Package: tweakPackage, Name: "TV settings"}
		c.extra = append(c.extra, r.listenOnFixedPort(ctx, c, line, "will not keep persist.adb.tcp.port"))
	}
}

func (r *Runner) listenOnFixedPort(ctx context.Context, c *conn, line Result, why string) Result {
	port := r.Cfg.TVFixedADBPort
	if err := r.ADB.TCPIP(ctx, c.serial, port); err != nil {
		line.Message = fmt.Sprintf("TV settings: this TV %s, and adbd would not restart on port %d either: %v. The random wireless-debugging port is still used", why, port, err)
		return line
	}
	line.OK, line.Changed = true, true
	line.Message = fmt.Sprintf("TV settings: this TV %s, so ADB listens on port %d only until the TV restarts; the next run sets it again", why, port)
	return line
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

// PairingPort asks the TV which port its open pairing dialog is on, so the page can fill it in
// instead of making you read it off the screen.
func (r *Runner) PairingPort(ctx context.Context, d store.Device) int {
	return r.ADB.PairingPort(ctx, d.Host)
}
