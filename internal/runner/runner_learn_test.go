package runner

import (
	"archive/zip"
	"context"
	"io"
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
	_ = os.WriteFile(p, apktest.Zip(map[string][]byte{"AndroidManifest.xml": apktest.Manifest(helper.Package, version, []string{helper.Permission, "android.permission.RECEIVE_BOOT_COMPLETED"}, []string{".MainActivity", ".BootReceiver", ".KeepOnJob", ".WatchJob"})}), 0o600)
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
	rg.adb.installs = nil
	res = rg.r.Run(context.Background(), Options{Force: true})
	helperInstalls := 0
	for _, i := range rg.adb.installs { // the app installs are not what this is about
		if strings.HasSuffix(i, "bootadb.apk") {
			helperInstalls++
		}
	}
	if len(helperLines(res)) != 0 || len(rg.adb.uninstalled) != 0 || helperInstalls != 0 {
		t.Errorf("an up-to-date helper is not reinstalled: %+v %v %v", helperLines(res), rg.adb.uninstalled, rg.adb.installs)
	}
	// Starting it, though, happens every time: that is what applies its settings, and leaving it
	// to the helper's own jobs meant it never applied anything at all.
	if !strings.Contains(strings.Join(rg.adb.shells, "|"), "am start -n com.sloccy.bootadb/.MainActivity") {
		t.Errorf("the helper must still be started on a later run: %v", rg.adb.shells)
	}
}

func TestAHelperThatIsInstalledButHasNeverRunIsStarted(t *testing.T) {
	// The state a real TV was found in: the right helper installed, its jobs registered, and
	// nothing in its log because nothing had ever called into it.
	rg, _ := helperRig(t, 20261022)
	const serial = "10.0.0.7:40000"
	rg.adb.installed[serial+"/com.sloccy.bootadb"] = "20261022"
	rg.adb.deviceAPK = rg.r.Cfg.BootHelperAPK // byte-identical, so no reinstall is due
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.uninstalled) != 0 {
		t.Errorf("nothing to reinstall: %v", rg.adb.uninstalled)
	}
	if !strings.Contains(strings.Join(rg.adb.shells, "|"), "am start -n com.sloccy.bootadb/.MainActivity") {
		t.Errorf("it must still be started, or it goes on doing nothing: %v", rg.adb.shells)
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
	impostor := filepath.Join(t.TempDir(), "impostor.apk")
	if err := os.WriteFile(impostor, apktest.Zip(map[string][]byte{
		"AndroidManifest.xml": apktest.Manifest(helper.Package, 20261022,
			[]string{helper.Permission, "android.permission.RECEIVE_BOOT_COMPLETED"},
			[]string{".MainActivity", ".BootReceiver", ".KeepOnJob", ".WatchJob"}),
		"classes.dex": []byte("not our code at all"),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	rg.adb.deviceAPK = impostor
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
	if rg.adb.deviceAPK == impostor {
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

func TestAStalePortIsForgottenSoItIsNotDialledForever(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "Bedroom", "10.0.0.7", prime)
	rg.r.Run(context.Background(), Options{Manual: true})
	if rg.st.Port(d.ID) != 40000 {
		t.Fatalf("the working port is remembered: %d", rg.st.Port(d.ID))
	}
	// Wireless debugging goes off and on, so adbd picks a new port: the old one is now dead.
	rg.adb.down["10.0.0.7"] = true
	rg.r.Run(context.Background(), Options{Manual: true})
	if rg.st.Port(d.ID) != 0 {
		t.Errorf("a port nothing answers on must be forgotten, not dialled first forever: %d", rg.st.Port(d.ID))
	}
}

func TestALapsedPairingKeepsThePortAndSaysToPairAgain(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "Bedroom", "10.0.0.7", prime)
	rg.r.Run(context.Background(), Options{Manual: true})
	rg.adb.unpaired["10.0.0.7"] = true
	res := rg.r.Run(context.Background(), Options{Manual: true})
	var line *Result
	for i := range res {
		if strings.Contains(res[i].Message, "Pair") {
			line = &res[i]
		}
	}
	if line == nil || !line.Alert {
		t.Fatalf("losing the pairing needs somebody at the TV, so it is said at once: %+v", res)
	}
	if rg.st.Port(d.ID) != 40000 {
		t.Errorf("the TV answered, so its port is still good: %d", rg.st.Port(d.ID))
	}
}

func TestPairingLearnsThePortTheAdbServerJustFound(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "Bedroom", "10.0.0.7", prime)
	_ = rg.st.SetPort(d.ID, 37000) // the port it was on before it was unpaired
	rg.adb.devicePort = 45678
	if _, err := rg.r.PairDevice(context.Background(), d, 42123, "123456"); err != nil {
		t.Fatal(err)
	}
	if rg.st.Port(d.ID) != 45678 {
		t.Errorf("pairing tells us the new port; without it the next run dials the old one: %d", rg.st.Port(d.ID))
	}
}

func TestATVThatExpiresKeysIsReachedWellInsideTheWeek(t *testing.T) {
	rg := newRig(t)
	rg.r.Cfg.VerifyEvery = 168 * time.Hour
	info := store.DeviceInfo{ABIs: []string{"armeabi-v7a"}, SDK: 31}
	// Six days since the last connection, and Android is still counting down to dropping the key.
	info.SeenAt = rg.now.Add(-6 * 24 * time.Hour).Format(time.RFC3339)
	info.KeyExpiry = "604800000"
	if !rg.r.verifyDue(info, true) {
		t.Error("a TV that still expires keys must be connected to before the week is up")
	}
	info.KeyExpiry = "0" // the helper got its way, so VerifyEvery is free to be a week
	if rg.r.verifyDue(info, true) {
		t.Error("a TV whose pairing cannot expire is left alone until VerifyEvery")
	}
}

func TestTheHelperIsUnthrottledAndItsKeyExpiryChecked(t *testing.T) {
	rg, d := helperRig(t, 20261022)
	rg.r.Run(context.Background(), Options{})
	joined := strings.Join(rg.adb.shells, "|")
	for _, want := range []string{
		"dumpsys deviceidle whitelist +com.sloccy.bootadb",
		"am set-standby-bucket com.sloccy.bootadb active",
		"cmd jobscheduler run -f com.sloccy.bootadb 1",
		"settings get global adb_allowed_connection_time",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, rg.adb.shells)
		}
	}
	if info, _ := rg.st.DeviceInfo(d.ID); info.KeyExpiry != "0" {
		t.Errorf("what the TV says about key expiry is remembered: %+v", info)
	}
}

// The helper's job is started, not waited for, so a TV whose key expiry is still Android's unset
// default must be fixed by tv-patcher itself rather than read back a moment later and reported.
func TestTheKeyExpiryIsSetOverADBNotLeftToTheHelpersJob(t *testing.T) {
	rg, d := helperRig(t, 20261022)
	if rg.adb.settings["adb_allowed_connection_time"] != "" {
		t.Fatal("the TV must start with no key expiry, as a real one does")
	}
	lines := helperLines(rg.r.Run(context.Background(), Options{}))
	if !strings.Contains(strings.Join(rg.adb.shells, "|"), "settings put global adb_allowed_connection_time 0") {
		t.Errorf("it must be written, not waited for: %v", rg.adb.shells)
	}
	if rg.adb.settings["adb_allowed_connection_time"] != "0" {
		t.Errorf("the TV should now never expire this server's key: %q", rg.adb.settings["adb_allowed_connection_time"])
	}
	for _, l := range lines {
		if !l.OK {
			t.Errorf("setting it is not a problem to report: %+v", l)
		}
	}
	if info, _ := rg.st.DeviceInfo(d.ID); info.KeyExpiry != "0" {
		t.Errorf("and the record follows the write, so the keepalive stands down: %+v", info)
	}
}

func TestASecondRunWritesNothingWhenTheSettingsAlreadyHold(t *testing.T) {
	rg, _ := helperRig(t, 20261022)
	rg.r.Run(context.Background(), Options{})
	rg.adb.shells = nil
	for _, x := range helperLines(rg.r.Run(context.Background(), Options{Manual: true})) {
		t.Errorf("nothing changed, so say nothing: %+v", x)
	}
	if j := strings.Join(rg.adb.shells, "|"); strings.Contains(j, "settings put global") {
		t.Errorf("a TV that already holds the values is not written to: %v", rg.adb.shells)
	}
}

func TestATVThatWillNotStopExpiringKeysIsReported(t *testing.T) {
	rg, d := helperRig(t, 20261022)
	// This firmware takes the write and keeps its own value anyway.
	rg.adb.settings["adb_allowed_connection_time"] = "604800000"
	rg.adb.refuses["adb_allowed_connection_time"] = true
	res := rg.r.Run(context.Background(), Options{})
	lines := helperLines(res)
	if len(lines) != 1 || lines[0].OK || !strings.Contains(lines[0].Message, "ADB key expiry") {
		t.Fatalf("the silent failure that unpairs a TV must be said out loud: %+v", lines)
	}
	if info, _ := rg.st.DeviceInfo(d.ID); info.KeyExpiry != "604800000" {
		t.Errorf("recorded even though it is wrong, because the keepalive reads it: %+v", info)
	}
	var apps int
	for _, x := range res {
		if x.Package == prime {
			apps++
		}
	}
	if apps == 0 {
		t.Error("helper trouble never stops the app updates")
	}
}

func TestTheFixedPortIsOnlySetWhenAskedFor(t *testing.T) {
	rg, _ := helperRig(t, 20261022)
	rg.r.Run(context.Background(), Options{})
	if j := strings.Join(rg.adb.shells, "|"); strings.Contains(j, "persist.adb.tcp.port") {
		t.Errorf("nothing is pinned unless TV_FIXED_ADB_PORT says so: %v", rg.adb.shells)
	}

	rg, d := helperRig(t, 20261022)
	rg.r.Cfg.TVFixedADBPort = 5555
	res := rg.r.Run(context.Background(), Options{})
	var line *Result
	for i := range res {
		if res[i].Package == tweakPackage {
			line = &res[i]
		}
	}
	if line == nil || !line.OK || !strings.Contains(line.Message, "5555") {
		t.Fatalf("%+v", line)
	}
	if j := strings.Join(rg.adb.shells, "|"); !strings.Contains(j, "setprop persist.adb.tcp.port 5555") ||
		!strings.Contains(j, "settings put global adb_enabled 1") {
		t.Errorf("%v", rg.adb.shells)
	}
	if len(rg.adb.tcpip) != 0 {
		t.Error("a run must not restart adbd: that would drop the connection the installs need")
	}
	// Said once: the second run reads the property back and leaves it alone.
	rg.adb.shells = nil
	for _, x := range rg.r.Run(context.Background(), Options{Manual: true}) {
		if x.Package == tweakPackage {
			t.Errorf("already pinned: say nothing, %+v", x)
		}
	}
	if j := strings.Join(rg.adb.shells, "|"); strings.Contains(j, "setprop") {
		t.Errorf("must not write when it already reads 5555: %v", rg.adb.shells)
	}

	// The full test is the one place nothing follows, so it may restart adbd there and then.
	rg.adb.props = nil
	if _, err := rg.r.TestDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(rg.adb.tcpip) != 1 {
		t.Errorf("a full test restarts adbd on the fixed port: %v", rg.adb.tcpip)
	}
}

// An install that reports success but leaves the old app in place used to be silent, and then
// looked like a settings problem further down while the page showed the version we shipped.
func TestAnInstallThatDidNotTakeIsReported(t *testing.T) {
	rg, d := helperRig(t, 20261022)
	rg.adb.helperVer = "20260101" // the TV keeps an older helper whatever the installer says
	lines := helperLines(rg.r.Run(context.Background(), Options{}))
	if len(lines) != 1 || lines[0].OK || !strings.Contains(lines[0].Message, "did not take") {
		t.Fatalf("%+v", lines)
	}
	if !strings.Contains(lines[0].Message, "20260101") || !strings.Contains(lines[0].Message, "20261022") {
		t.Errorf("name both versions, so it is obvious which is which: %q", lines[0].Message)
	}
	if info, _ := rg.st.DeviceInfo(d.ID); info.Helper == "20261022" {
		t.Errorf("the page must not claim the version we shipped: %+v", info)
	}
}

// bootadb/build.sh signs with a throw-away key it generates per build, so a rebuild of unchanged
// sources differs byte for byte. Comparing bytes meant the helper was uninstalled and reinstalled
// on every TV every time a new image was published, which the weekly rebuild guarantees.
func TestAReSignedHelperWithTheSameCodeIsLeftAlone(t *testing.T) {
	rg, _ := helperRig(t, 20261022)
	const serial = "10.0.0.7:40000"
	rg.adb.installed[serial+"/com.sloccy.bootadb"] = "20261022"

	// the same app, signed again: every entry identical, only the signature differs
	zr, err := zip.OpenReader(rg.r.Cfg.BootHelperAPK)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{"META-INF/CERT.RSA": []byte("a different throw-away key")}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries[f.Name] = b
	}
	_ = zr.Close()
	resigned := filepath.Join(t.TempDir(), "resigned.apk")
	if err := os.WriteFile(resigned, apktest.Zip(entries), 0o600); err != nil {
		t.Fatal(err)
	}
	rg.adb.deviceAPK = resigned

	rg.r.Run(context.Background(), Options{})
	helperInstalls := 0
	for _, i := range rg.adb.installs { // the app installs are not what this is about
		if strings.HasSuffix(i, "bootadb.apk") {
			helperInstalls++
		}
	}
	if len(rg.adb.uninstalled) != 0 || helperInstalls != 0 {
		t.Errorf("the same app signed again must not be reinstalled: %v %v", rg.adb.uninstalled, rg.adb.installs)
	}
	if lines := helperLines(rg.r.Run(context.Background(), Options{Manual: true})); len(lines) != 0 {
		t.Errorf("and nothing to report: %+v", lines)
	}
}

// A full test must say whether the helper is actually working. Finding out that it had never run
// once took reading logcat by hand, which the page should have been able to answer.
func TestAFullTestReportsWhatTheHelperIsDoing(t *testing.T) {
	rg, d := helperRig(t, 20261022)
	rg.adb.jobsDump = "JOB #u0a99/1: com.sloccy.bootadb/.KeepOnJob\nJOB #u0a99/2: com.sloccy.bootadb/.WatchJob\n"
	rg.adb.helperLog = "--------- beginning of main\n" +
		"10-30 04:00:00.000  I BootAdb  : adb_allowed_connection_time: null -> 0\n" +
		"10-30 04:00:00.001  I BootAdb  : checked 9 settings, changed 1\n"
	chk, err := rg.r.TestDevice(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if chk.HelperJobs != 2 {
		t.Errorf("both jobs are registered: %d", chk.HelperJobs)
	}
	if !strings.Contains(chk.HelperLog, "changed 1") {
		t.Errorf("the last thing it said: %q", chk.HelperLog)
	}

	// A helper that has never run is the case that matters, and must not look the same.
	rg.adb.jobsDump, rg.adb.helperLog = "", ""
	chk, err = rg.r.TestDevice(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if chk.HelperJobs != 0 || chk.HelperLog != "" {
		t.Errorf("silence must read as silence: %+v", chk)
	}
}
