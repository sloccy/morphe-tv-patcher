package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/config"
	"github.com/sloccy/morphe-tv-patcher/internal/morphe"
	"github.com/sloccy/morphe-tv-patcher/internal/patches"
	"github.com/sloccy/morphe-tv-patcher/internal/sources"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
	"github.com/sloccy/morphe-tv-patcher/internal/verify"
)

type fakeADB struct {
	mu          sync.Mutex
	down        map[string]bool     // host -> unreachable
	abis        map[string][]string // host -> abis
	installed   map[string]string   // "serial/pkg" -> version
	installMsg  map[string]string   // pkg -> failure output (first attempt)
	installs    []string            // "serial pkg"
	uninstalled []string
}

func newADB() *fakeADB {
	return &fakeADB{down: map[string]bool{}, abis: map[string][]string{}, installed: map[string]string{}, installMsg: map[string]string{}}
}

func (f *fakeADB) Connect(ctx context.Context, host string, known ...int) (int, error) {
	if f.down[host] {
		return 0, errors.New("device unreachable")
	}
	return 40000, nil
}
func (f *fakeADB) ABIs(ctx context.Context, s string) ([]string, error) {
	host := strings.Split(s, ":")[0]
	if a, ok := f.abis[host]; ok {
		return a, nil
	}
	return []string{"armeabi-v7a"}, nil
}
func (f *fakeADB) SDK(context.Context, string) (int, error) { return 31, nil }
func (f *fakeADB) InstalledVersion(ctx context.Context, s, pkg string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installed[s+"/"+pkg], nil
}
func (f *fakeADB) Install(ctx context.Context, s, apk string) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := filepath.Base(apk)
	for pkg, msg := range f.installMsg {
		if strings.HasPrefix(base, pkg) && msg != "" {
			f.installMsg[pkg] = "" // fail once
			return false, msg
		}
	}
	f.installs = append(f.installs, s+" "+base)
	for pkg := range f.installMsg {
		_ = pkg
	}
	return true, "Success"
}
func (f *fakeADB) Uninstall(ctx context.Context, s, pkg string) error {
	f.uninstalled = append(f.uninstalled, s+" "+pkg)
	return nil
}
func (f *fakeADB) Pair(context.Context, string, int, string) error { return nil }

type rig struct {
	r        *Runner
	st       *store.Store
	adb      *fakeADB
	fetches  int
	patches  int
	fetchErr error
	pkgName  func(path string) string
	notes    []string
	now      time.Time
}

const tag = "v1.42.2"

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir, MorpheJar: "m.jar", BytecodeMode: "STRIP_SAFE", Interval: time.Hour, FailuresBeforeAlert: 3, DirectOK: true}
	cfg.Ensure()
	st, err := store.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	rg := &rig{st: st, adb: newADB(), now: time.Unix(1_700_000_000, 0)}
	data, _ := os.ReadFile("../patches/testdata/patches-list.json")
	list, err := patches.ParseList(data)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Latest:   func(context.Context) (*patches.Release, error) { return &patches.Release{Tag: tag}, nil },
		Download: func(context.Context, *patches.Release) (string, error) { return "p.mpp", nil },
		List:     func(context.Context, string) (*patches.List, error) { return list, nil },
		Fetch: func(ctx context.Context, a sources.App, sdk int) (*sources.Fetched, error) {
			rg.fetches++
			if rg.fetchErr != nil {
				return nil, rg.fetchErr
			}
			return &sources.Fetched{Verified: &verify.Verified{Path: "in.apkm", Package: a.Package, Version: a.Version}, Detail: "apkmirror (chrome)"}, nil
		},
		Patch: func(ctx context.Context, o morphe.Options) error {
			rg.patches++
			return os.WriteFile(o.Out, []byte("apk"), 0o644)
		},
		PackageOf: func(path string) (string, error) {
			if rg.pkgName != nil {
				return rg.pkgName(path), nil
			}
			return "", errors.New("unknown")
		},
	}
	rg.r = New(cfg, st, rg.adb, deps, func(ctx context.Context, title, body string) { rg.notes = append(rg.notes, title+": "+body) })
	rg.r.Now = func() time.Time { return rg.now }
	return rg
}

func (rg *rig) device(t *testing.T, name, host string, apps ...string) store.Device {
	t.Helper()
	d, err := rg.st.AddDevice(name, host)
	if err != nil {
		t.Fatal(err)
	}
	rg.st.SetApps(d.ID, apps)
	return d
}

const (
	prime   = "com.amazon.amazonvideo.livingroom"
	netflix = "com.netflix.ninja"
)

func TestOneDeviceOfflineDoesNotBlockTheOther(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "Living room", "10.0.0.6", prime)
	rg.device(t, "Bedroom", "10.0.0.7", prime)
	rg.adb.down["10.0.0.7"] = true
	res := rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 1 || !strings.HasPrefix(rg.adb.installs[0], "10.0.0.6:40000 ") {
		t.Errorf("installs %v", rg.adb.installs)
	}
	var offline, ok bool
	for _, x := range res {
		offline = offline || (!x.OK && strings.Contains(x.Message, "Bedroom") && strings.Contains(x.Message, "not reachable"))
		ok = ok || (x.OK && x.Changed && x.Device == "Living room")
	}
	if !offline || !ok {
		t.Errorf("%+v", res)
	}
	if in, _ := rg.st.Install("living-room", prime); in.APKVersion != "6.23.23+v15.5.0.70-armv7a" || in.PatchesVersion != tag {
		t.Errorf("install record %+v", in)
	}
	if rg.st.Port("living-room") != 40000 {
		t.Error("working port should be remembered")
	}
}

func TestFetchAndPatchOncePerAppAndABISet(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "A", "10.0.0.1", prime)
	rg.device(t, "B", "10.0.0.2", prime)
	rg.device(t, "C", "10.0.0.3", prime)
	rg.adb.abis["10.0.0.3"] = []string{"arm64-v8a", "armeabi-v7a"} // a different ABI set
	rg.r.Run(context.Background(), Options{})
	if rg.fetches != 1 {
		t.Errorf("fetched %d times, want once for the app", rg.fetches)
	}
	if rg.patches != 2 {
		t.Errorf("patched %d times, want once per ABI set (2)", rg.patches)
	}
	if len(rg.adb.installs) != 3 {
		t.Errorf("installs %v", rg.adb.installs)
	}
}

func TestUpToDateSkipsAndForceReinstalls(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "TV", "10.0.0.6", prime)
	rg.r.Run(context.Background(), Options{})
	rg.adb.installed[d.Host+":40000/"+prime] = "6.23.23+v15.5.0.70-armv7a"
	rg.fetches, rg.patches, rg.adb.installs = 0, 0, nil
	res := rg.r.Run(context.Background(), Options{})
	if rg.fetches != 0 || rg.patches != 0 || len(rg.adb.installs) != 0 || !strings.Contains(res[0].Message, "up to date") {
		t.Errorf("%d %d %v %+v", rg.fetches, rg.patches, rg.adb.installs, res)
	}
	rg.r.Run(context.Background(), Options{Force: true})
	if rg.fetches != 1 || len(rg.adb.installs) != 1 {
		t.Errorf("force: %d fetches %v", rg.fetches, rg.adb.installs)
	}
	// the app vanished from the TV: install again even though state says done
	rg.adb.installed[d.Host+":40000/"+prime] = ""
	rg.adb.installs = nil
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 1 {
		t.Error("an app that is no longer on the device must be reinstalled")
	}
	// a newer patches release triggers a reinstall
	rg.adb.installed[d.Host+":40000/"+prime] = "x"
	rg.r.Deps.Latest = func(context.Context) (*patches.Release, error) { return &patches.Release{Tag: "v9"}, nil }
	rg.adb.installs = nil
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 1 {
		t.Error("new patches release should reinstall")
	}
}

func TestCloneInstalledPackageIsTrackedFromTheManifest(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "TV", "10.0.0.6", netflix)
	rg.pkgName = func(string) string { return "com.netflix.ninja.clone" }
	rg.r.Run(context.Background(), Options{})
	in, _ := rg.st.Install(d.ID, netflix)
	if in.InstalledPackage != "com.netflix.ninja.clone" {
		t.Fatalf("%+v", in)
	}
	rg.adb.installed[d.Host+":40000/com.netflix.ninja.clone"] = "13.0.1 build 25028"
	rg.adb.installs = nil
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 0 {
		t.Error("the clone is installed; the stock app's version must not matter")
	}
}

func TestFetchFailureAlertRules(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", prime)
	rg.fetchErr = &sources.FetchError{Attempts: []string{"apkmirror: blocked"}}
	var alerts []bool
	for i := 0; i < 5; i++ {
		res := rg.r.Run(context.Background(), Options{})
		alerts = append(alerts, res[0].Alert)
		rg.now = rg.now.Add(6 * time.Hour)
	}
	// quiet for two cycles, alert on the third, then silent for 24 hours (the 4th and 5th are 6 and 12 hours later)
	want := []bool{false, false, true, false, false}
	if fmt.Sprint(alerts) != fmt.Sprint(want) {
		t.Errorf("alerts %v, want %v", alerts, want)
	}
	if !strings.Contains(rg.notes[0], "Drop the .apkm into") || len(rg.notes) != 1 {
		t.Errorf("notes %v", rg.notes)
	}
	// recovery
	rg.fetchErr = nil
	rg.now = rg.now.Add(48 * time.Hour)
	rg.notes = nil
	rg.r.Run(context.Background(), Options{})
	if len(rg.notes) != 1 || !strings.Contains(rg.notes[0], "recovered") {
		t.Errorf("notes %v", rg.notes)
	}
	if a := rg.st.App(prime); a.FetchFailures != 0 || a.LastFetchAlert != 0 {
		t.Errorf("counters not cleared: %+v", a)
	}
}

func TestFatalFetchAlertsImmediately(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", prime)
	rg.fetchErr = &sources.FetchError{Attempts: []string{"signature: mismatch"}, Fatal: true}
	if res := rg.r.Run(context.Background(), Options{}); !res[0].Alert {
		t.Error("a wrong signature must alert at once")
	}
}

func TestOneMessagePerAppNotPerDevice(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "A", "10.0.0.1", prime)
	rg.device(t, "B", "10.0.0.2", prime)
	rg.fetchErr = &sources.FetchError{Attempts: []string{"x"}, Fatal: true}
	rg.r.Run(context.Background(), Options{})
	if strings.Count(rg.notes[0], "cannot get") != 1 {
		t.Errorf("%q", rg.notes[0])
	}
}

func TestSignatureConflictGuidanceAndAllowUninstall(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "TV", "10.0.0.6", prime)
	rg.adb.installMsg[prime] = "Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: signatures do not match]"
	res := rg.r.Run(context.Background(), Options{})
	if res[0].OK || !res[0].Alert || !strings.Contains(res[0].Message, "Uninstall") || len(rg.adb.uninstalled) != 0 {
		t.Fatalf("%+v uninstalled %v", res, rg.adb.uninstalled)
	}
	// with allow_uninstall it removes the old copy and retries
	dir := filepath.Dir(rg.r.Cfg.Cache())
	_ = dir
	settings := `{"devices":[{"id":"` + d.ID + `","name":"TV","host":"10.0.0.6","apps":["` + prime + `"]}],"overrides":{"` + prime + `":{"allow_uninstall":true}}}`
	os.WriteFile(filepath.Join(rg.r.Cfg.DataDir, "state", "settings.json"), []byte(settings), 0o600)
	st, _ := store.Open(filepath.Join(rg.r.Cfg.DataDir, "state"))
	rg.r.Store = st
	rg.adb.installMsg[prime] = "Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE]"
	res = rg.r.Run(context.Background(), Options{})
	if !res[0].OK || len(rg.adb.uninstalled) != 1 || !strings.HasSuffix(rg.adb.uninstalled[0], prime) {
		t.Errorf("%+v uninstalled %v", res, rg.adb.uninstalled)
	}
}

func TestStockNetflixIsNeverUninstalled(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "TV", "10.0.0.6", netflix)
	settings := `{"devices":[{"id":"` + d.ID + `","name":"TV","host":"10.0.0.6","apps":["` + netflix + `"]}],"overrides":{"` + netflix + `":{"allow_uninstall":true}}}`
	os.WriteFile(filepath.Join(rg.r.Cfg.DataDir, "state", "settings.json"), []byte(settings), 0o600)
	rg.r.Store, _ = store.Open(filepath.Join(rg.r.Cfg.DataDir, "state"))
	rg.pkgName = func(string) string { return "com.netflix.ninja" } // pretend the clone patch did not rename it
	rg.adb.installMsg[netflix] = "Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE]"
	res := rg.r.Run(context.Background(), Options{})
	if res[0].OK || len(rg.adb.uninstalled) != 0 {
		t.Errorf("%+v uninstalled %v", res, rg.adb.uninstalled)
	}
}

func TestNoDeviceAndDryRunNeverInstall(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", prime)
	res := rg.r.Run(context.Background(), Options{NoDevice: true, Packages: []string{netflix}})
	if len(rg.adb.installs) != 0 || len(res) != 1 || !res[0].Changed || !strings.Contains(res[0].Message, "dry run") || res[0].Package != netflix {
		t.Errorf("%+v", res)
	}
	rg.r.Run(context.Background(), Options{DryRun: true})
	if len(rg.adb.installs) != 0 {
		t.Error("dry run installed something")
	}
	if _, ok := rg.st.Install("tv", prime); ok {
		t.Error("a dry run must not record an install")
	}
}

func TestUnsupportedPackageAndNoApps(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", "com.not.supported")
	res := rg.r.Run(context.Background(), Options{})
	if res[0].OK || !strings.Contains(res[0].Message, "not supported") {
		t.Errorf("%+v", res)
	}
	rg2 := newRig(t)
	rg2.device(t, "Empty", "10.0.0.8")
	res = rg2.r.Run(context.Background(), Options{})
	if len(res) != 1 || !res[0].OK || !strings.Contains(res[0].Message, "nothing to do") {
		t.Errorf("%+v", res)
	}
}

func TestOnlyTheRequestedDeviceAndApp(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "A", "10.0.0.1", prime, netflix)
	rg.device(t, "B", "10.0.0.2", prime)
	rg.r.Run(context.Background(), Options{DeviceID: "a", Package: netflix})
	if len(rg.adb.installs) != 1 || !strings.HasPrefix(rg.adb.installs[0], "10.0.0.1:") || !strings.Contains(rg.adb.installs[0], netflix) {
		t.Errorf("%v", rg.adb.installs)
	}
}

func TestPatchFailureAlerts(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", prime)
	rg.r.Deps.Patch = func(context.Context, morphe.Options) error { return errors.New("patches failed: X") }
	res := rg.r.Run(context.Background(), Options{})
	if res[0].OK || !res[0].Alert || !strings.Contains(res[0].Message, "patching") {
		t.Errorf("%+v", res)
	}
	if _, ok := rg.st.Install("tv", prime); ok {
		t.Error("nothing was installed")
	}
}

func TestLoopRunsOnStartThenOnTriggerAndStops(t *testing.T) {
	rg := newRig(t)
	rg.r.Cfg.Interval = time.Hour
	rg.device(t, "TV", "10.0.0.6", prime)
	var mu sync.Mutex
	runs := 0
	orig := rg.r.Deps.Latest
	rg.r.Deps.Latest = func(ctx context.Context) (*patches.Release, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		return orig(ctx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rg.r.Loop(ctx); close(done) }()
	wait := func(n int) {
		for i := 0; i < 200; i++ {
			mu.Lock()
			got := runs
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("expected %d runs", n)
	}
	wait(1)
	if !rg.r.Trigger(Options{Force: true}) {
		t.Fatal("trigger refused")
	}
	wait(2)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop")
	}
	if s := rg.r.Status(); s.Running || s.PatchesVersion != tag {
		t.Errorf("%+v", s)
	}
}

func TestTriggerQueuesAtMostOne(t *testing.T) {
	rg := newRig(t)
	if !rg.r.Trigger(Options{}) || rg.r.Trigger(Options{}) {
		t.Error("only one pending run should be queued")
	}
}

func TestRingKeepsLastLines(t *testing.T) {
	r := NewRing(3, nil)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(r, "line %d\n", i)
	}
	fmt.Fprint(r, "partial")
	got := r.Lines()
	if strings.Join(got, "|") != "line 2|line 3|line 4" {
		t.Errorf("%v", got)
	}
}
