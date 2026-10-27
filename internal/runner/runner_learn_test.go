package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/apk/apktest"
	"github.com/sloccy/morphe-tv-patcher/internal/helper"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

func TestLearnerProbesEachAppOnceOneAtATime(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	a := rg.knownDevice(t, "A", "10.0.0.1", "arm64-v8a")
	_, _ = rg.st.AddSet("S", []store.AppChoice{{Package: netflix}, {Package: prime}}, []string{a.ID})
	var calls, active, peak atomic.Int32
	rg.r.Deps.Probe = func(ctx context.Context, src store.Source, pkg, name, version string) ([]string, error) {
		calls.Add(1)
		n := active.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return []string{"armeabi-v7a"}, nil
	}
	var wg sync.WaitGroup
	for range 10 { // ten identical saves at once
		wg.Go(func() { rg.r.Compat(context.Background(), rg.st.Settings(), []string{a.ID}) })
	}
	wg.Wait()
	if calls.Load() != 2 || peak.Load() != 1 {
		t.Fatalf("each app once, one at a time: calls %d peak %d", calls.Load(), peak.Load())
	}
	if l := rg.r.Learning(); len(l.Pending) != 0 {
		t.Fatalf("%+v", l)
	}
}

func TestCompatDoesNotWaitForSlowLookupsAndWarnsLater(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	a := rg.knownDevice(t, "A", "10.0.0.1", "arm64-v8a")
	_, _ = rg.st.AddSet("S", []store.AppChoice{{Package: netflix}}, []string{a.ID})
	release := make(chan struct{})
	rg.r.Deps.Probe = func(ctx context.Context, src store.Source, pkg, name, version string) ([]string, error) {
		<-release
		return []string{"armeabi-v7a"}, nil
	}
	done := make(chan struct{})
	go func() {
		// this save is still waiting when the answer arrives, so it sees the conflict
		problems, _ := rg.r.Compat(context.Background(), rg.st.Settings(), []string{a.ID})
		if len(problems) != 1 {
			t.Errorf("%+v", problems)
		}
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// cancelling the request ends the wait, but the lookup carries on in the background
	problems, _ := rg.r.Compat(ctx, rg.st.Settings(), []string{a.ID})
	if len(problems) != 0 {
		t.Fatalf("%+v", problems)
	}
	if l := rg.r.Learning(); len(l.Pending) != 1 {
		t.Fatalf("pending %+v", l)
	}
	if len(rg.r.Warnings()) != 0 {
		t.Fatal("nothing known yet")
	}
	close(release)
	<-done
	deadline := time.Now().Add(time.Second)
	for len(rg.r.Warnings()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(rg.r.Warnings()) != 1 {
		t.Fatalf("a conflict found later is a warning: %+v", rg.r.Warnings())
	}
}

func TestFailedLookupsAreNotRepeatedAtOnce(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	a := rg.knownDevice(t, "A", "10.0.0.1", "arm64-v8a")
	_, _ = rg.st.AddSet("S", []store.AppChoice{{Package: netflix}}, []string{a.ID})
	var calls atomic.Int32
	rg.r.Deps.Probe = func(context.Context, store.Source, string, string, string) ([]string, error) {
		calls.Add(1)
		return nil, context.DeadlineExceeded
	}
	for range 3 {
		rg.r.Compat(context.Background(), rg.st.Settings(), []string{a.ID})
	}
	if calls.Load() != 1 {
		t.Fatalf("a failure is remembered for a while; probed %d times", calls.Load())
	}
}

func TestUnreachableTVAlertsOnTheSecondCycleAndSaysWhatToDo(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "Bedroom", "10.0.0.7", prime)
	rg.adb.down["10.0.0.7"] = true
	alerts := make([]bool, 0, 3)
	var msg string
	for range 3 {
		res := rg.r.Run(context.Background(), Options{})
		alerts = append(alerts, res[0].Alert)
		msg = res[0].Message
	}
	if alerts[0] || !alerts[1] || alerts[2] {
		t.Fatalf("alert on the 2nd failed cycle only (then at most daily): %v", alerts)
	}
	if !strings.Contains(msg, "Wireless debugging") {
		t.Errorf("tell the user what to do: %s", msg)
	}
}

func TestTestDeviceReportsPortCPUAndAdviceAndClearsFailures(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "Bedroom", "10.0.0.7", prime)
	rg.adb.abis["10.0.0.7"] = []string{"arm64-v8a", "armeabi-v7a"}
	c, err := rg.r.TestDevice(context.Background(), d)
	if err != nil || c.Port != 40000 || len(c.ABIs) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	if info, ok := rg.st.DeviceInfo(d.ID); !ok || len(info.ABIs) != 2 {
		t.Errorf("the CPU should be saved: %+v", info)
	}
	rg.adb.down["10.0.0.7"] = true
	rg.r.Run(context.Background(), Options{})
	if rg.st.App("device:"+d.ID).FetchFailures == 0 {
		t.Fatal("setup: the failed run should be counted")
	}
	_, err = rg.r.TestDevice(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "Wireless debugging is switched off") {
		t.Fatalf("advice expected: %v", err)
	}
	delete(rg.adb.down, "10.0.0.7")
	if _, err := rg.r.TestDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if rg.st.App("device:"+d.ID).FetchFailures != 0 {
		t.Error("a passing test clears the failure count so the alert does not carry over")
	}
}

// helperRig gives the rig a boot helper APK of the given version and a fake TV.
func helperRig(t *testing.T, version uint32) (*rig, store.Device) {
	t.Helper()
	rg := newRig(t)
	p := filepath.Join(t.TempDir(), "bootadb.apk")
	_ = os.WriteFile(p, apktest.Zip(map[string][]byte{"AndroidManifest.xml": apktest.Manifest(helper.Package, version, []string{helper.Permission, "android.permission.RECEIVE_BOOT_COMPLETED"}, []string{".MainActivity", ".BootReceiver", ".KeepOnJob"})}), 0o600)
	rg.r.Cfg.BootHelperAPK = p
	rg.adb.helperVer = strconv.Itoa(int(version))
	d := rg.device(t, "Bedroom", "10.0.0.7", prime)
	return rg, d
}

func helperLines(res []Result) []Result {
	var out []Result
	for _, x := range res {
		if x.Package == helper.Package {
			out = append(out, x)
		}
	}
	return out
}

func TestRunInstallsTheBootHelperOnceThenLeavesItAlone(t *testing.T) {
	rg, d := helperRig(t, 20261022)
	res := rg.r.Run(context.Background(), Options{})
	lines := helperLines(res)
	if len(lines) != 1 || !lines[0].OK || !lines[0].Changed {
		t.Fatalf("first run installs it: %+v", lines)
	}
	joined := strings.Join(rg.adb.shells, "|")
	if !strings.Contains(joined, "pm grant com.sloccy.bootadb android.permission.WRITE_SECURE_SETTINGS") || !strings.Contains(joined, "am start -n com.sloccy.bootadb/.MainActivity") {
		t.Errorf("must grant the permission and open it once: %v", rg.adb.shells)
	}
	if info, _ := rg.st.DeviceInfo(d.ID); info.Helper != "20261022" {
		t.Errorf("the version should be remembered: %+v", info)
	}
	rg.adb.shells = nil
	rg.adb.uninstalled = nil
	res = rg.r.Run(context.Background(), Options{Force: true})
	if len(helperLines(res)) != 0 || len(rg.adb.uninstalled) != 0 || strings.Contains(strings.Join(rg.adb.shells, "|"), "am start") {
		t.Errorf("an up-to-date helper is left alone: %+v %v %v", helperLines(res), rg.adb.uninstalled, rg.adb.shells)
	}
}

func TestRunUpgradesAnOldBootHelperByReinstalling(t *testing.T) {
	rg, _ := helperRig(t, 20261101)
	rg.adb.installed["10.0.0.7:40000/com.sloccy.bootadb"] = "20261022"
	rg.r.Run(context.Background(), Options{})
	found := false
	for _, u := range rg.adb.uninstalled {
		found = found || strings.HasSuffix(u, "com.sloccy.bootadb")
	}
	if !found || rg.adb.installed["10.0.0.7:40000/com.sloccy.bootadb"] != "20261101" {
		t.Errorf("old copy removed and new installed: %v %v", rg.adb.uninstalled, rg.adb.installed)
	}
}

func TestAnotherAppUnderTheHelpersNameIsReplacedBeforeTheGrant(t *testing.T) {
	rg, _ := helperRig(t, 20261022)
	const serial = "10.0.0.7:40000"
	// An app squatting the package name can report a matching version (build.sh derives it from
	// the public sources), but it is not the APK we ship, so it must never keep the permission.
	rg.adb.installed[serial+"/com.sloccy.bootadb"] = "20261022"
	rg.adb.helperSHA = strings.Repeat("ab", 32)
	rg.r.Run(context.Background(), Options{})

	found := false
	for _, u := range rg.adb.uninstalled {
		found = found || u == serial+" com.sloccy.bootadb"
	}
	grantedAfterInstall := false
	for i, s := range rg.adb.shells {
		if strings.HasPrefix(s, "pm grant") {
			grantedAfterInstall = i > 0 && len(rg.adb.installs) > 0
		}
	}
	if !found || !grantedAfterInstall {
		t.Errorf("the impostor must be removed and ours installed before the grant: %v %v %v",
			rg.adb.uninstalled, rg.adb.installs, rg.adb.shells)
	}
	if rg.adb.helperSHA == strings.Repeat("ab", 32) {
		t.Error("the device should be holding our APK now")
	}
}

func TestBootHelperTroubleNeverStopsAppUpdates(t *testing.T) {
	rg, _ := helperRig(t, 20261022)
	rg.adb.grantErr = "java.lang.SecurityException: grant failed"
	res := rg.r.Run(context.Background(), Options{})
	lines := helperLines(res)
	if len(lines) != 1 || lines[0].OK || !strings.Contains(lines[0].Message, "boot helper") {
		t.Fatalf("%+v", lines)
	}
	if in, _ := rg.st.Install("bedroom", prime); in.APKVersion == "" {
		t.Error("the app itself must still be installed")
	}
	// the alert comes on the second run in a row, like an unreachable TV
	if lines[0].Alert {
		t.Error("not on the first failure")
	}
	if l2 := helperLines(rg.r.Run(context.Background(), Options{Manual: true})); len(l2) != 1 || !l2[0].Alert {
		t.Errorf("second failure alerts: %+v", l2)
	}
}

func TestNoHelperAPKMeansTheStepIsSkipped(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "Bedroom", "10.0.0.7", prime)
	rg.r.Cfg.BootHelperAPK = "/nonexistent/bootadb.apk"
	if len(helperLines(rg.r.Run(context.Background(), Options{}))) != 0 || len(rg.adb.shells) != 0 {
		t.Error("skipped quietly")
	}
}

func TestTestButtonInstallsTheHelperAndSaysSo(t *testing.T) {
	rg, d := helperRig(t, 20261022)
	c, err := rg.r.TestDevice(context.Background(), d)
	if err != nil || !strings.Contains(c.Helper, "installed just now") {
		t.Fatalf("%+v %v", c, err)
	}
	if c, _ = rg.r.TestDevice(context.Background(), d); !strings.HasPrefix(c.Helper, "installed (version") {
		t.Errorf("%+v", c)
	}
}

func TestScheduledRunsConnectOnlyWhenNeeded(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "Bedroom", "10.0.0.7", prime)
	rg.r.Cfg.VerifyEvery = 7 * 24 * time.Hour
	rg.r.Run(context.Background(), Options{}) // first run: never seen, connects and installs
	if rg.adb.connects == 0 || len(rg.adb.installs) != 1 {
		t.Fatalf("first run: %d connects, installs %v", rg.adb.connects, rg.adb.installs)
	}
	rg.adb.connects = 0
	res := rg.r.Run(context.Background(), Options{})
	if rg.adb.connects != 0 || !strings.Contains(res[0].Message, "up to date") {
		t.Fatalf("nothing to do must not touch the TV: %d connects, %+v", rg.adb.connects, res)
	}
	rg.tags[defSrc] = "v9" // a new patches release: one connection, shared by the whole run
	rg.r.Run(context.Background(), Options{})
	if rg.adb.connects != 1 {
		t.Errorf("an update needs exactly one connection, got %d", rg.adb.connects)
	}
	rg.adb.connects = 0
	rg.r.Run(context.Background(), Options{Manual: true})
	rg.r.Run(context.Background(), Options{Force: true})
	if rg.adb.connects != 2 {
		t.Errorf("Run now and Force always connect: %d", rg.adb.connects)
	}
	rg.adb.connects = 0
	now := rg.r.Now()
	rg.r.Now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	rg.r.Run(context.Background(), Options{})
	if rg.adb.connects != 1 {
		t.Errorf("a TV not seen for VerifyEvery is checked: %d", rg.adb.connects)
	}
}

func TestAnUnreachableTVIsReportedOncePerRunNotOncePerApp(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "Bedroom", "10.0.0.7", prime, netflix)
	rg.r.Run(context.Background(), Options{}) // learns the TV, installs both
	rg.adb.down["10.0.0.7"] = true
	rg.tags[defSrc] = "v9" // both apps now need installing, but the TV is off
	res := rg.r.Run(context.Background(), Options{})
	alerts, msgs := 0, 0
	for _, x := range res {
		if strings.Contains(x.Message, "not reachable") || strings.Contains(x.Message, "not installed") {
			msgs++
		}
		if x.Alert {
			alerts++
		}
	}
	if msgs < 2 || alerts > 1 {
		t.Errorf("each app says it was not installed, the alert is once: %d messages, %d alerts: %+v", msgs, alerts, res)
	}
	if rg.st.App("device:"+d.ID).FetchFailures != 1 {
		t.Errorf("one failed cycle, not one per app: %d", rg.st.App("device:"+d.ID).FetchFailures)
	}
}

func TestBackgroundProcessLimitIsSetWhenConnectedAndOnlyWhenDifferent(t *testing.T) {
	rg := newRig(t)
	rg.r.Cfg.BackgroundProcessLimit = 4
	rg.device(t, "Bedroom", "10.0.0.7", prime)
	res := rg.r.Run(context.Background(), Options{})
	var line *Result
	for i := range res {
		if res[i].Package == tweakPackage {
			line = &res[i]
		}
	}
	if line == nil || !line.OK || !strings.Contains(line.Message, "set to 4") || rg.adb.flags["activity_manager max_cached_processes"] != "4" {
		t.Fatalf("%+v %v", line, rg.adb.flags)
	}
	for _, c := range rg.adb.shells {
		if strings.Contains(c, "set_sync_disabled_for_tests") {
			t.Error("Google's flag sync is only touched when asked")
		}
	}
	rg.adb.shells = nil
	for _, x := range rg.r.Run(context.Background(), Options{Manual: true}) {
		if x.Package == tweakPackage {
			t.Errorf("already 4: say nothing, %+v", x)
		}
	}
	for _, c := range rg.adb.shells {
		if strings.HasPrefix(c, "device_config put") {
			t.Error("must not write when it already reads 4")
		}
	}
	rg.r.Cfg.PersistTVFlags = true
	rg.r.Run(context.Background(), Options{Manual: true})
	if !strings.Contains(strings.Join(rg.adb.shells, "|"), "device_config set_sync_disabled_for_tests persistent") {
		t.Error("opt-in persistence")
	}
}

func TestNoProcessLimitConfiguredMeansNoSettingsTouched(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "Bedroom", "10.0.0.7", prime)
	rg.r.Run(context.Background(), Options{})
	for _, c := range rg.adb.shells {
		if strings.Contains(c, "device_config") {
			t.Errorf("unexpected %s", c)
		}
	}
}

func TestQuietCheckNeverConnects(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "Bedroom", "10.0.0.7", prime)
	q := rg.r.CheckDevice(context.Background(), d)
	if !q.Reachable || q.Port != 40000 || rg.adb.connects != 0 {
		t.Fatalf("%+v connects %d", q, rg.adb.connects)
	}
	rg.adb.down["10.0.0.7"] = true
	if q := rg.r.CheckDevice(context.Background(), d); q.Reachable || rg.adb.connects != 0 {
		t.Fatalf("%+v", q)
	}
}
