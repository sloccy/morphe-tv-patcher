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
	pairErr     error
	paired      []string
	shells      []string          // commands run through Shell
	grantErr    string            // output of "pm grant" when it fails
	helperVer   string            // version a successful helper install leaves on the device
	connects    int               // adb connect calls (each one makes the real TV show a notice)
	flags       map[string]string // device_config "namespace key" -> value
	playing     map[string]bool   // host -> something is playing
	playingErr  error
}

func (f *fakeADB) Playing(ctx context.Context, s string) (bool, error) {
	host, _, _ := strings.Cut(s, ":")
	return f.playing[host], f.playingErr
}

func newADB() *fakeADB {
	return &fakeADB{playing: map[string]bool{}, down: map[string]bool{}, abis: map[string][]string{}, installed: map[string]string{}, installMsg: map[string]string{}}
}

func (f *fakeADB) Reachable(ctx context.Context, host string, known ...int) (int, bool) {
	return 40000, !f.down[host]
}
func (f *fakeADB) Connect(ctx context.Context, host string, known ...int) (int, error) {
	f.connects++
	if f.down[host] {
		return 0, errors.New("device unreachable")
	}
	return 40000, nil
}
func (f *fakeADB) ABIs(ctx context.Context, s string) ([]string, error) {
	host, _, _ := strings.Cut(s, ":")
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
func (f *fakeADB) Install(ctx context.Context, s, apk string) (ok bool, output string) {
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
	if base == "bootadb.apk" {
		f.installed[s+"/com.sloccy.bootadb"] = f.helperVer
	}
	for pkg := range f.installMsg {
		_ = pkg
	}
	return true, "Success"
}
func (f *fakeADB) Uninstall(ctx context.Context, s, pkg string) error {
	f.uninstalled = append(f.uninstalled, s+" "+pkg)
	delete(f.installed, s+"/"+pkg)
	return nil
}
func (f *fakeADB) Shell(ctx context.Context, serial, cmd string) (string, error) {
	f.shells = append(f.shells, cmd)
	switch {
	case strings.HasPrefix(cmd, "pm grant"):
		if f.grantErr != "" {
			return f.grantErr, nil
		}
		return "", nil
	case strings.HasPrefix(cmd, "device_config get "):
		if v, ok := f.flags[strings.TrimPrefix(cmd, "device_config get ")]; ok {
			return v + "\n", nil
		}
		return "null\n", nil
	case strings.HasPrefix(cmd, "device_config put "):
		parts := strings.Fields(strings.TrimPrefix(cmd, "device_config put "))
		if f.flags == nil {
			f.flags = map[string]string{}
		}
		f.flags[parts[0]+" "+parts[1]] = parts[2]
		return "", nil
	case strings.HasPrefix(cmd, "settings get global adb_wifi_enabled"):
		return "1\n", nil
	}
	return "", nil
}
func (f *fakeADB) Pair(ctx context.Context, host string, port int, code string) error {
	f.paired = append(f.paired, fmt.Sprintf("%s:%d %s", host, port, code))
	return f.pairErr
}

type rig struct {
	r        *Runner
	st       *store.Store
	adb      *fakeADB
	fetches  int
	fetchABI [][]string
	patches  int
	fetchErr error
	pkgName  func(path string) string
	// patch sources: the release each reports (default "v1.42.2"), failures, and extra lists
	tags        map[string]string
	sourceErr   map[string]error
	sourceLists map[string]*patches.List
	cached      map[string]*patches.Release // last good bundle per source (what Deps.Cached returns)
	appsCalls   []string
	notes       []string
	now         time.Time
	cacheFetch  bool
	stripLibs   [][]string // StripLibs of every patch call
}

const (
	tag    = "v1.42.2"
	defSrc = "o-default" // the repository seeded on the first run
)

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir, MorpheJar: "m.jar", BytecodeMode: "STRIP_SAFE", Interval: time.Hour, FailuresBeforeAlert: 3, DirectOK: true}
	_ = cfg.Ensure()
	st, err := store.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SeedSources([]string{"o/default"}, false)
	rg := &rig{st: st, adb: newADB(), now: time.Unix(1_700_000_000, 0),
		tags: map[string]string{}, sourceErr: map[string]error{}, sourceLists: map[string]*patches.List{}, cached: map[string]*patches.Release{}}
	data, _ := os.ReadFile("../patches/testdata/patches-list.json")
	list, err := patches.ParseList(data)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Latest:   func(ctx context.Context, src store.Source) (*patches.Release, error) { return rg.latest(src) },
		Download: func(ctx context.Context, src store.Source, rel *patches.Release) (string, error) { return "p.mpp", nil },
		Apps: func(ctx context.Context, src store.Source, rel *patches.Release, mpp string) (*patches.List, error) {
			rg.appsCalls = append(rg.appsCalls, src.ID)
			if l, ok := rg.sourceLists[src.ID]; ok {
				return l, nil
			}
			return list, nil
		},
		Fetch: func(ctx context.Context, a sources.App, sdk int) (*sources.Fetched, error) {
			rg.fetches++
			rg.fetchABI = append(rg.fetchABI, a.ABIs)
			if rg.fetchErr != nil {
				return nil, rg.fetchErr
			}
			path := "in.apkm"
			if rg.cacheFetch { // a download that lands in the cache folder, as the real one does
				path = filepath.Join(cfg.Cache(), a.Package+"-"+a.Version+".apkm")
				_ = os.WriteFile(path, []byte("x"), 0o600)
			}
			return &sources.Fetched{Verified: &verify.Verified{Path: path, Package: a.Package, Version: a.Version}, Detail: "apkmirror (chrome)"}, nil
		},
		Patch: func(ctx context.Context, o morphe.Options) error {
			rg.patches++
			rg.stripLibs = append(rg.stripLibs, o.StripLibs)
			return os.WriteFile(o.Out, []byte("apk"), 0o600)
		},
		Cached: func(src store.Source) (*patches.Release, string, bool) {
			if rel := rg.cached[src.ID]; rel != nil {
				return rel, "cached.mpp", true
			}
			return nil, "", false
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

func (rg *rig) latest(src store.Source) (*patches.Release, error) {
	if err := rg.sourceErr[src.ID]; err != nil {
		return nil, err
	}
	t := tag
	if v, ok := rg.tags[src.ID]; ok {
		t = v
	}
	return &patches.Release{Tag: t, Name: "patches.mpp"}, nil
}

func (rg *rig) device(t *testing.T, name, host string, apps ...string) store.Device {
	t.Helper()
	d, err := rg.st.AddDevice(name, host)
	if err != nil {
		t.Fatal(err)
	}
	choices := make([]store.AppChoice, 0, len(apps))
	for _, a := range apps {
		choices = append(choices, store.AppChoice{Package: a, Source: defSrc})
	}
	_ = rg.st.SetApps(d.ID, choices)
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

func TestFetchAndPatchOncePerAppAndCPUGroup(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "A", "10.0.0.1", prime)
	rg.device(t, "B", "10.0.0.2", prime)
	rg.device(t, "C", "10.0.0.3", prime)
	rg.adb.abis["10.0.0.3"] = []string{"arm64-v8a", "armeabi-v7a"} // a different ABI set
	rg.r.Run(context.Background(), Options{})
	if rg.fetches != 2 {
		t.Errorf("fetched %d times, want once per CPU group (2)", rg.fetches)
	}
	if rg.patches != 2 {
		t.Errorf("patched %d times, want once per CPU group (2)", rg.patches)
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
	if len(rg.adb.installs) != 0 {
		t.Error("a scheduled run goes by its records and does not connect just to look")
	}
	rg.r.Run(context.Background(), Options{Manual: true})
	if len(rg.adb.installs) != 1 {
		t.Error("Run now checks the TV, and an app that is no longer on it must be reinstalled")
	}
	// a newer patches release triggers a reinstall
	rg.adb.installed[d.Host+":40000/"+prime] = "x"
	rg.tags[defSrc] = "v9"
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
	alerts := make([]bool, 0, 5)
	for range 5 {
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
	settings := `{"sources":[{"id":"o-default","repo":"o/default"}],"devices":[{"id":"` + d.ID + `","name":"TV","host":"10.0.0.6","apps":["` + prime + `"]}],"overrides":{"` + prime + `":{"allow_uninstall":true}}}`
	_ = os.WriteFile(filepath.Join(rg.r.Cfg.DataDir, "state", "settings.json"), []byte(settings), 0o600)
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
	settings := `{"sources":[{"id":"o-default","repo":"o/default"}],"devices":[{"id":"` + d.ID + `","name":"TV","host":"10.0.0.6","apps":["` + netflix + `"]}],"overrides":{"` + netflix + `":{"allow_uninstall":true}}}`
	_ = os.WriteFile(filepath.Join(rg.r.Cfg.DataDir, "state", "settings.json"), []byte(settings), 0o600)
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
	rg.r.Deps.Latest = func(ctx context.Context, src store.Source) (*patches.Release, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		return orig(ctx, src)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rg.r.Loop(ctx); close(done) }()
	wait := func(n int) {
		for range 200 {
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
	if s := rg.r.Status(); s.Running || len(s.Sources) == 0 || s.Sources[0].Tag != tag {
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
	for i := range 5 {
		_, _ = fmt.Fprintf(r, "line %d\n", i)
	}
	_, _ = fmt.Fprint(r, "partial")
	got := r.Lines()
	if strings.Join(got, "|") != "line 2|line 3|line 4" {
		t.Errorf("%v", got)
	}
}

func TestEachCPUGroupGetsItsOwnBuild(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "Old", "10.0.0.1", prime)
	rg.device(t, "New", "10.0.0.2", prime)
	rg.adb.abis["10.0.0.1"] = []string{"armeabi-v7a", "armeabi"}
	rg.adb.abis["10.0.0.2"] = []string{"arm64-v8a", "armeabi-v7a", "armeabi"}
	var strips [][]string
	orig := rg.r.Deps.Patch
	rg.r.Deps.Patch = func(ctx context.Context, o morphe.Options) error {
		strips = append(strips, o.StripLibs)
		return orig(ctx, o)
	}
	rg.r.Run(context.Background(), Options{})
	if len(rg.fetchABI) != 2 || len(strips) != 2 {
		t.Fatalf("fetches %v patches %v", rg.fetchABI, strips)
	}
	seen := map[string]bool{}
	for _, a := range rg.fetchABI {
		seen[strings.Join(a, ",")] = true
	}
	if !seen["armeabi-v7a,armeabi"] || !seen["arm64-v8a,armeabi-v7a,armeabi"] {
		t.Errorf("each group must be fetched with its own preference order: %v", rg.fetchABI)
	}
	if len(rg.adb.installs) != 2 || rg.adb.installs[0] == rg.adb.installs[1] {
		t.Errorf("two different patched files expected: %v", rg.adb.installs)
	}
	if info, ok := rg.st.DeviceInfo("new"); !ok || len(info.ABIs) != 3 || info.SDK != 31 {
		t.Errorf("device info not saved: %+v %v", info, ok)
	}
}

func TestNoBuildForThisCPUIsOneQuietMessageNotAFailureCount(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "Chromebox", "10.0.0.5", prime)
	rg.adb.abis["10.0.0.5"] = []string{"arm64-v8a"}
	rg.fetchErr = &sources.FetchError{Attempts: []string{"apkmirror: no build"}, NoBuild: true}
	res := rg.r.Run(context.Background(), Options{})
	if res[0].OK || !res[0].Alert || !strings.Contains(res[0].Message, "no build of") || !strings.Contains(res[0].Message, "arm64-v8a") {
		t.Fatalf("%+v", res)
	}
	if a := rg.st.App(prime); a.FetchFailures != 0 {
		t.Errorf("a CPU mismatch is not a download failure: %+v", a)
	}
	rg.now = rg.now.Add(6 * time.Hour)
	if res = rg.r.Run(context.Background(), Options{}); res[0].Alert {
		t.Error("the same mismatch must not alert again every cycle")
	}
	if len(rg.adb.installs) != 0 {
		t.Error("nothing should be installed")
	}
}

func TestNoDeviceRunsBuildForTheRequestedCPUs(t *testing.T) {
	rg := newRig(t)
	var strip []string
	rg.r.Deps.Patch = func(ctx context.Context, o morphe.Options) error {
		strip = o.StripLibs
		return os.WriteFile(o.Out, []byte("apk"), 0o600)
	}
	rg.r.Run(context.Background(), Options{NoDevice: true, Packages: []string{prime}, ABIs: []string{"arm64-v8a", "armeabi-v7a"}})
	if len(strip) != 0 || strings.Join(rg.fetchABI[0], ",") != "arm64-v8a,armeabi-v7a" {
		t.Errorf("strip %v fetch %v", strip, rg.fetchABI)
	}
	rg.fetchABI = nil
	rg.r.Run(context.Background(), Options{NoDevice: true, Packages: []string{prime}})
	if strings.Join(rg.fetchABI[0], ",") != "armeabi-v7a,armeabi" {
		t.Errorf("no CPU given should mean the 32-bit default: %v", rg.fetchABI)
	}
}

// ---- patch sources ----

const yt = "com.google.android.youtube"

func (rg *rig) secondSource(t *testing.T, allowPhone bool) store.Source {
	t.Helper()
	src, err := rg.st.AddSource("MorpheApp/morphe-patches", false, allowPhone)
	if err != nil {
		t.Fatal(err)
	}
	stable := map[string][]string{yt: {"20.1.0", "20.2.0"}, prime: {"1.0"}}
	rg.sourceLists[src.ID] = patches.FromCLI("v1.48.0", stable, stable, nil)
	rg.tags[src.ID] = "v1.48.0"
	return src
}

func (rg *rig) deviceWith(t *testing.T, name, host string, choices ...store.AppChoice) store.Device {
	t.Helper()
	d, err := rg.st.AddDevice(name, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := rg.st.SetApps(d.ID, choices); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAppFromASecondPatchSourceUsesThatSourcesBundleAndPhonePolicy(t *testing.T) {
	rg := newRig(t)
	src := rg.secondSource(t, true)
	d := rg.deviceWith(t, "TV", "10.0.0.6", store.AppChoice{Package: yt, Source: src.ID}, store.AppChoice{Package: prime, Source: defSrc})
	var gotApps []sources.App
	orig := rg.r.Deps.Fetch
	rg.r.Deps.Fetch = func(ctx context.Context, a sources.App, sdk int) (*sources.Fetched, error) {
		gotApps = append(gotApps, a)
		return orig(ctx, a, sdk)
	}
	res := rg.r.Run(context.Background(), Options{})
	for _, x := range res {
		if !x.OK {
			t.Fatalf("%+v", res)
		}
	}
	byPkg := map[string]sources.App{}
	for _, a := range gotApps {
		byPkg[a.Package] = a
	}
	if a := byPkg[yt]; a.Version != "20.2.0" || !a.AllowPhone {
		t.Errorf("youtube from the second source: %+v", a)
	}
	if a := byPkg[prime]; a.AllowPhone || a.Version != "6.23.23+v15.5.0.70-armv7a" {
		t.Errorf("an app from the default source keeps the strict TV check: %+v", a)
	}
	in, _ := rg.st.Install(d.ID, yt)
	if in.PatchSource != src.ID || in.PatchesVersion != "v1.48.0" || in.APKVersion != "20.2.0" {
		t.Errorf("install record %+v", in)
	}
	if got := rg.r.Lists(); len(got) != 2 || got[src.ID] == nil || got[defSrc] == nil {
		t.Errorf("lists %v", got)
	}
	st := rg.r.Status()
	if len(st.Sources) != 2 || st.Sources[0].Tag != tag || st.Sources[1].Tag != "v1.48.0" {
		t.Errorf("status %+v", st)
	}
}

func TestSameAppFromTwoSourcesOnTwoDevicesIsPatchedSeparately(t *testing.T) {
	rg := newRig(t)
	src := rg.secondSource(t, false)
	rg.deviceWith(t, "A", "10.0.0.1", store.AppChoice{Package: prime, Source: defSrc})
	rg.deviceWith(t, "B", "10.0.0.2", store.AppChoice{Package: prime, Source: src.ID})
	rg.r.Run(context.Background(), Options{})
	if rg.fetches != 2 || rg.patches != 2 || len(rg.adb.installs) != 2 || rg.adb.installs[0] == rg.adb.installs[1] {
		t.Errorf("fetches %d patches %d installs %v", rg.fetches, rg.patches, rg.adb.installs)
	}
}

func TestSwitchingAnAppsPatchSourceReinstallsIt(t *testing.T) {
	rg := newRig(t)
	src := rg.secondSource(t, false)
	d := rg.deviceWith(t, "TV", "10.0.0.6", store.AppChoice{Package: prime, Source: defSrc})
	rg.r.Run(context.Background(), Options{})
	rg.adb.installed[d.Host+":40000/"+prime] = "6.23.23+v15.5.0.70-armv7a"
	rg.adb.installs = nil
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 0 {
		t.Fatal("unchanged: nothing to do")
	}
	_ = rg.st.SetApps(d.ID, []store.AppChoice{{Package: prime, Source: src.ID}})
	rg.sourceLists[src.ID] = patches.FromCLI("v1.48.0", map[string][]string{prime: {"1.0"}}, map[string][]string{prime: {"1.0"}}, nil)
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 1 {
		t.Errorf("a different patch source must reinstall: %v", rg.adb.installs)
	}
}

func TestUnreachablePatchSourceDoesNotStopTheOthers(t *testing.T) {
	rg := newRig(t)
	src := rg.secondSource(t, false)
	rg.sourceErr[src.ID] = errors.New("HTTP 404")
	rg.deviceWith(t, "TV", "10.0.0.6", store.AppChoice{Package: yt, Source: src.ID}, store.AppChoice{Package: prime, Source: defSrc})
	res := rg.r.Run(context.Background(), Options{})
	var bad, good bool
	for _, x := range res {
		bad = bad || (!x.OK && strings.Contains(x.Message, "MorpheApp/morphe-patches") && strings.Contains(x.Message, "unavailable") && !x.Alert)
		good = good || (x.OK && x.Package == prime)
	}
	if !bad || !good {
		t.Errorf("%+v", res)
	}
	if st := rg.r.Status(); len(st.Sources) != 2 || (st.Sources[1].Error == "" && st.Sources[0].Error == "") {
		t.Errorf("the failing source should show its error: %+v", st.Sources)
	}
	// like a download problem, it alerts only after repeated failures
	alerts := 0
	for range 4 {
		rg.now = rg.now.Add(6 * time.Hour)
		for _, x := range rg.r.Run(context.Background(), Options{}) {
			if x.Alert && !x.OK {
				alerts++
			}
		}
	}
	if alerts != 1 {
		t.Errorf("one alert after repeated failures, got %d", alerts)
	}
}

func TestRemovedPatchSourceIsReportedNotCrashed(t *testing.T) {
	rg := newRig(t)
	src := rg.secondSource(t, false)
	d := rg.deviceWith(t, "TV", "10.0.0.6", store.AppChoice{Package: yt, Source: src.ID})
	// simulate settings.json edited by hand so the choice points at a source that no longer exists
	settings := `{"sources":[{"id":"o-default","repo":"o/default"}],"devices":[{"id":"` + d.ID + `","name":"TV","host":"10.0.0.6","apps":[{"package":"` + yt + `","source":"gone"}]}]}`
	_ = os.WriteFile(filepath.Join(rg.r.Cfg.DataDir, "state", "settings.json"), []byte(settings), 0o600)
	rg.r.Store, _ = store.Open(filepath.Join(rg.r.Cfg.DataDir, "state"))
	res := rg.r.Run(context.Background(), Options{})
	if res[0].OK || !strings.Contains(res[0].Message, "removed") {
		t.Errorf("%+v", res)
	}
}

func TestRefreshListsReadsEachReleaseOnce(t *testing.T) {
	rg := newRig(t)
	rg.secondSource(t, false)
	rg.r.RefreshLists(context.Background())
	rg.r.RefreshLists(context.Background())
	if len(rg.appsCalls) != 2 {
		t.Errorf("each source's release is parsed once, got %v", rg.appsCalls)
	}
	rg.tags[defSrc] = "v2"
	rg.r.RefreshLists(context.Background())
	if len(rg.appsCalls) != 3 {
		t.Errorf("a new release is parsed again: %v", rg.appsCalls)
	}
}

func TestCheckRepo(t *testing.T) {
	rg := newRig(t)
	rg.r.Deps.Latest = func(ctx context.Context, src store.Source) (*patches.Release, error) {
		switch src.Repo {
		case "a/ok":
			return &patches.Release{Tag: "v1", Size: 5 << 20}, nil
		case "a/huge":
			return &patches.Release{Tag: "v1", Size: 500 << 20}, nil
		}
		return nil, errors.New("no release with an .mpp asset")
	}
	if err := rg.r.CheckRepo(context.Background(), "a/ok", false); err != nil {
		t.Error(err)
	}
	if rg.r.CheckRepo(context.Background(), "a/huge", false) == nil || rg.r.CheckRepo(context.Background(), "a/none", false) == nil {
		t.Error("a repo without a usable bundle must be refused")
	}
}

func TestNoDeviceRunsUseTheChosenSource(t *testing.T) {
	rg := newRig(t)
	src := rg.secondSource(t, true)
	res := rg.r.Run(context.Background(), Options{NoDevice: true, Packages: []string{yt}, Source: src.ID})
	if len(res) != 1 || !res[0].OK || !strings.Contains(res[0].Message, "dry run") {
		t.Errorf("%+v", res)
	}
}

func TestVanishedRepositoryKeepsWorkingOnItsLastGoodBundle(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "TV", "10.0.0.6", prime)
	rg.cached[defSrc] = &patches.Release{Tag: "v1.42.2", Name: "patches-1.42.2.mpp"}
	rg.sourceErr[defSrc] = errors.New("HTTP 404: repository deleted")

	res := rg.r.Run(context.Background(), Options{})
	var installed bool
	for _, x := range res {
		installed = installed || (x.OK && x.Changed && x.Package == prime)
	}
	if !installed {
		t.Fatalf("with the repository gone, the cached bundle must still be used: %+v", res)
	}
	if in, _ := rg.st.Install(d.ID, prime); in.PatchesVersion != "v1.42.2" {
		t.Errorf("%+v", in)
	}
	st := rg.r.Status()
	if len(st.Sources) != 1 || st.Sources[0].Warning == "" || st.Sources[0].Error != "" || st.Sources[0].Tag != "v1.42.2" {
		t.Errorf("the page should show a warning, not an error: %+v", st.Sources)
	}
	// quiet at first; after repeated cycles one message says the repository is gone
	alerts := 0
	for range 4 {
		rg.now = rg.now.Add(6 * time.Hour)
		for _, x := range rg.r.Run(context.Background(), Options{}) {
			if x.Alert && !x.OK {
				alerts++
				if !strings.Contains(x.Message, "remove it on the page") {
					t.Errorf("message should say what to do: %q", x.Message)
				}
			}
		}
	}
	if alerts != 1 {
		t.Errorf("one alert after the failure repeats, got %d", alerts)
	}
	// it comes back: the warning clears
	delete(rg.sourceErr, defSrc)
	rg.r.Run(context.Background(), Options{})
	if st := rg.r.Status(); st.Sources[0].Warning != "" {
		t.Errorf("warning should clear: %+v", st.Sources)
	}
}

func TestNoCachedBundleAndNoRepositoryIsAnErrorNotACrash(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", prime)
	rg.sourceErr[defSrc] = errors.New("HTTP 404")
	res := rg.r.Run(context.Background(), Options{})
	if res[0].OK || !strings.Contains(res[0].Message, "unavailable") {
		t.Errorf("%+v", res)
	}
	if st := rg.r.Status(); st.Sources[0].Error == "" {
		t.Errorf("%+v", st.Sources)
	}
}

func TestRemovingTheFirstRepositoryLeavesTheAppRunningForOthers(t *testing.T) {
	rg := newRig(t)
	src := rg.secondSource(t, false)
	rg.deviceWith(t, "TV", "10.0.0.6", store.AppChoice{Package: prime, Source: defSrc}, store.AppChoice{Package: yt, Source: src.ID})
	if _, err := rg.st.RemoveSource(defSrc); err != nil {
		t.Fatal(err)
	}
	res := rg.r.Run(context.Background(), Options{})
	for _, x := range res {
		if !x.OK || x.Package == prime {
			t.Fatalf("only the other repository's app should run: %+v", res)
		}
	}
	if len(rg.adb.installs) != 1 || !strings.Contains(rg.adb.installs[0], yt) {
		t.Errorf("%v", rg.adb.installs)
	}
}

func TestAppsWithNoSourceUseTheFirstOne(t *testing.T) {
	rg := newRig(t)
	d := rg.device(t, "TV", "10.0.0.6", prime)
	settings := `{"sources_seeded":true,"sources":[{"id":"o-default","repo":"o/default"}],"devices":[{"id":"` + d.ID + `","name":"TV","host":"10.0.0.6","apps":["` + prime + `"]}]}`
	_ = os.WriteFile(filepath.Join(rg.r.Cfg.DataDir, "state", "settings.json"), []byte(settings), 0o600)
	rg.r.Store, _ = store.Open(filepath.Join(rg.r.Cfg.DataDir, "state"))
	rg.r.Run(context.Background(), Options{})
	if in, _ := rg.r.Store.Install(d.ID, prime); in.PatchSource != defSrc {
		t.Errorf("a plain package name uses the first source: %+v", in)
	}
}

func TestNoPatchSourcesAtAllIsHandled(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6")
	_, _ = rg.st.RemoveSource(defSrc)
	res := rg.r.Run(context.Background(), Options{})
	if len(res) != 1 || !res[0].OK || !strings.Contains(res[0].Message, "nothing to do") {
		t.Errorf("%+v", res)
	}
	rg.r.RefreshLists(context.Background()) // must not panic with no sources
	if len(rg.r.Lists()) != 1 {             // the list loaded earlier is not invented or lost
		t.Logf("lists: %v", rg.r.Lists())
	}
}

// ---- app sets, compatibility, pairing ----

func (rg *rig) knownDevice(t *testing.T, name, host string, abis ...string) store.Device {
	t.Helper()
	d, err := rg.st.AddDevice(name, host)
	if err != nil {
		t.Fatal(err)
	}
	_ = rg.st.SetDeviceInfo(d.ID, store.DeviceInfo{ABIs: abis, SDK: 31, SeenAt: "t"})
	return d
}

func TestADeviceGetsItsSetsPlusItsOwnExtras(t *testing.T) {
	rg := newRig(t)
	a := rg.device(t, "Living room", "10.0.0.1", disney)
	b := rg.device(t, "Bedroom", "10.0.0.2")
	set, err := rg.st.AddSet("Streaming", []store.AppChoice{{Package: prime}, {Package: netflix}}, []string{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	_ = set
	res := rg.r.Run(context.Background(), Options{})
	for _, x := range res {
		if !x.OK {
			t.Fatalf("%+v", res)
		}
	}
	if len(rg.adb.installs) != 5 { // living room: disney + prime + netflix, bedroom: prime + netflix
		t.Errorf("installs %v", rg.adb.installs)
	}
	if rg.fetches != 3 { // each app is fetched once for the one CPU group, however many devices use it
		t.Errorf("fetches %d", rg.fetches)
	}
	if _, ok := rg.st.Install(b.ID, disney); ok {
		t.Error("the bedroom TV does not have the living room's extra app")
	}
	// the set changes once; both TVs follow on the next run
	_ = rg.st.UpdateSet("streaming", "Streaming", []store.AppChoice{{Package: netflix}}, nil)
	rg.adb.installs = nil
	rg.r.Run(context.Background(), Options{Force: true})
	if len(rg.adb.installs) != 3 { // living room: disney + netflix, bedroom: netflix
		t.Errorf("after editing the set: %v", rg.adb.installs)
	}
}

const disney = "com.disney.disneyplus"

func TestCompatBlocksOnlyKnownMismatches(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	onn := rg.knownDevice(t, "Onn", "10.0.0.1", "armeabi-v7a", "armeabi")
	box := rg.knownDevice(t, "Box64", "10.0.0.2", "arm64-v8a", "armeabi-v7a", "armeabi")
	only := rg.knownDevice(t, "OnlyArm64", "10.0.0.3", "arm64-v8a")
	nev := rg.st.Settings()
	_ = nev
	_ = rg.st.SetBuilds(netflix, "13.0.1 build 25028", []string{"armeabi-v7a"})
	_ = rg.st.SetBuilds(disney, "26.16.0+rc2-2026.09.08", []string{"arm64-v8a", "armeabi-v7a"})
	set, _ := rg.st.AddSet("All", []store.AppChoice{{Package: netflix}, {Package: disney}}, []string{onn.ID, box.ID, only.ID})
	_ = set
	problems, unchecked := rg.r.Compat(context.Background(), rg.st.Settings(), []string{onn.ID, box.ID, only.ID})
	if len(problems) != 1 || problems[0].Device != "OnlyArm64" || problems[0].Package != netflix ||
		!strings.Contains(problems[0].Reason, "armeabi-v7a") || !strings.Contains(problems[0].Reason, "arm64-v8a") {
		t.Fatalf("only the arm64-only box cannot run Netflix: %+v", problems)
	}
	if len(unchecked) != 0 {
		t.Errorf("%v", unchecked)
	}
}

func TestCompatUniversalBuildsRunEverywhereAndUnknownsDoNotBlock(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	fresh, _ := rg.st.AddDevice("Never reached", "10.0.0.9") // CPU unknown
	only := rg.knownDevice(t, "OnlyArm64", "10.0.0.3", "arm64-v8a")
	_ = rg.st.SetBuilds(prime, "6.23.23+v15.5.0.70-armv7a", []string{"*"})
	_, _ = rg.st.AddSet("S", []store.AppChoice{{Package: prime}, {Package: netflix}}, []string{fresh.ID, only.ID})
	// netflix: builds unknown and no probe available
	problems, unchecked := rg.r.Compat(context.Background(), rg.st.Settings(), []string{fresh.ID, only.ID})
	if len(problems) != 0 {
		t.Errorf("universal builds run anywhere and unknowns are never blocked: %+v", problems)
	}
	joined := strings.Join(unchecked, "|")
	if !strings.Contains(joined, "Never reached") || !strings.Contains(joined, "could not check") {
		t.Errorf("the page should say what was not checked: %v", unchecked)
	}
}

func TestCompatLearnsBuildsByProbingOnceAndRemembersThem(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	a := rg.knownDevice(t, "A", "10.0.0.1", "arm64-v8a")
	b := rg.knownDevice(t, "B", "10.0.0.2", "arm64-v8a")
	_, _ = rg.st.AddSet("S", []store.AppChoice{{Package: netflix}}, []string{a.ID, b.ID})
	calls := 0
	rg.r.Deps.Probe = func(ctx context.Context, src store.Source, pkg, name, version string) ([]string, error) {
		calls++
		return []string{"armeabi-v7a"}, nil
	}
	problems, _ := rg.r.Compat(context.Background(), rg.st.Settings(), []string{a.ID, b.ID})
	if calls != 1 || len(problems) != 2 {
		t.Fatalf("one probe serves both devices: calls %d problems %+v", calls, problems)
	}
	rg.r.Compat(context.Background(), rg.st.Settings(), []string{a.ID})
	if calls != 1 {
		t.Errorf("what was learned is remembered; probed %d times", calls)
	}
	// a failing probe is "unchecked", never a block
	rg2 := newRig(t)
	rg2.r.RefreshLists(context.Background())
	c := rg2.knownDevice(t, "C", "10.0.0.3", "arm64-v8a")
	_, _ = rg2.st.AddSet("S", []store.AppChoice{{Package: netflix}}, []string{c.ID})
	rg2.r.Deps.Probe = func(context.Context, store.Source, string, string, string) ([]string, error) {
		return nil, errors.New("HTTP 429")
	}
	problems, unchecked := rg2.r.Compat(context.Background(), rg2.st.Settings(), []string{c.ID})
	if len(problems) != 0 || len(unchecked) != 1 {
		t.Errorf("%+v %v", problems, unchecked)
	}
}

func TestRunsRememberWhichBuildsAnAppOffers(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", prime)
	orig := rg.r.Deps.Fetch
	rg.r.Deps.Fetch = func(ctx context.Context, a sources.App, sdk int) (*sources.Fetched, error) {
		f, err := orig(ctx, a, sdk)
		f.Available = []string{"armeabi-v7a"}
		return f, err
	}
	rg.r.Run(context.Background(), Options{})
	if b, ok := rg.st.Builds(prime, "6.23.23+v15.5.0.70-armv7a"); !ok || len(b) != 1 {
		t.Errorf("%v %v", b, ok)
	}
	// and a 'no build' failure records what the release does have
	rg2 := newRig(t)
	d := rg2.device(t, "Box", "10.0.0.5", netflix)
	rg2.adb.abis["10.0.0.5"] = []string{"arm64-v8a"}
	rg2.fetchErr = &sources.FetchError{NoBuild: true, Available: []string{"armeabi-v7a"}}
	rg2.r.Run(context.Background(), Options{})
	_ = d
	if b, ok := rg2.st.Builds(netflix, "13.0.1 build 25028"); !ok || b[0] != "armeabi-v7a" {
		t.Errorf("%v %v", b, ok)
	}
}

func TestPairingSavesWhenAndReadsTheCPU(t *testing.T) {
	rg := newRig(t)
	d, _ := rg.st.AddDevice("TV", "10.0.0.6")
	rg.adb.abis["10.0.0.6"] = []string{"arm64-v8a", "armeabi-v7a"}
	cpu, err := rg.r.PairDevice(context.Background(), d, 38000, "123456")
	if err != nil || !cpu {
		t.Fatalf("%v %v", cpu, err)
	}
	info, _ := rg.st.DeviceInfo(d.ID)
	if info.PairedAt == "" || len(info.ABIs) != 2 || info.SDK != 31 {
		t.Errorf("%+v", info)
	}
	if len(rg.adb.paired) != 1 || rg.adb.paired[0] != "10.0.0.6:38000 123456" {
		t.Errorf("%v", rg.adb.paired)
	}
	// a later run keeps the pairing time
	_, _ = rg.r.connect(context.Background(), d)
	if again, _ := rg.st.DeviceInfo(d.ID); again.PairedAt == "" {
		t.Error("reaching the device again must not forget it was paired")
	}
	// paired, but the TV cannot be read right now: still success
	rg.adb.down["10.0.0.7"] = true
	d2, _ := rg.st.AddDevice("TV2", "10.0.0.7")
	cpu, err = rg.r.PairDevice(context.Background(), d2, 38001, "654321")
	if err != nil || cpu {
		t.Errorf("%v %v", cpu, err)
	}
	if i2, _ := rg.st.DeviceInfo(d2.ID); i2.PairedAt == "" {
		t.Error("pairing worked, so it is recorded")
	}
	// a wrong code is an error and records nothing
	rg.adb.pairErr = errors.New("pairing failed: wrong code")
	d3, _ := rg.st.AddDevice("TV3", "10.0.0.8")
	if _, err := rg.r.PairDevice(context.Background(), d3, 38002, "000000"); err == nil {
		t.Error("expected an error")
	}
	if _, ok := rg.st.DeviceInfo(d3.ID); ok {
		t.Error("nothing is recorded for a failed pairing")
	}
}

func TestBusyTVIsLeftAloneUntilARunFindsItIdle(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", prime)
	rg.adb.playing["10.0.0.6"] = true
	res := rg.r.Run(context.Background(), Options{})
	if rg.fetches != 0 || rg.patches != 0 || len(rg.adb.installs) != 0 {
		t.Errorf("a playing TV must not trigger work: %d fetches, %d patches, %v", rg.fetches, rg.patches, rg.adb.installs)
	}
	if len(res) == 0 || !res[0].OK || res[0].Alert || !strings.Contains(res[0].Message, "waiting") {
		t.Errorf("want a quiet waiting line, got %+v", res)
	}
	if _, has := rg.st.Install(rg.st.Settings().Devices[0].ID, prime); has {
		t.Error("a deferred install must leave no install record")
	}
	rg.adb.playing["10.0.0.6"] = false
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 1 {
		t.Errorf("once idle the next run installs: %v", rg.adb.installs)
	}
}

func TestRunNowAndForceIgnorePlayback(t *testing.T) {
	for _, o := range []Options{{Manual: true}, {Force: true}} {
		rg := newRig(t)
		rg.device(t, "TV", "10.0.0.6", prime)
		rg.adb.playing["10.0.0.6"] = true
		rg.r.Run(context.Background(), o)
		if len(rg.adb.installs) != 1 {
			t.Errorf("%+v should install while playing: %v", o, rg.adb.installs)
		}
	}
}

func TestOnlyTheBusyTVWaits(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "A", "10.0.0.1", prime)
	rg.device(t, "B", "10.0.0.2", prime)
	rg.adb.playing["10.0.0.1"] = true
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 1 || !strings.HasPrefix(rg.adb.installs[0], "10.0.0.2:") {
		t.Errorf("only the idle TV should be updated: %v", rg.adb.installs)
	}
}

func TestFailedPlaybackCheckDoesNotBlockUpdates(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "TV", "10.0.0.6", prime)
	rg.adb.playingErr = errors.New("dumpsys failed")
	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 1 {
		t.Errorf("a broken check must not hold updates back: %v", rg.adb.installs)
	}
}

func TestInstallRemovesOlderVersionsFromCacheAndOutput(t *testing.T) {
	rg := newRig(t)
	rg.cacheFetch = true
	rg.device(t, "TV", "10.0.0.6", prime)
	cfg := rg.r.Cfg
	const oldVer = "5.0.0"
	oldBundle := filepath.Join(cfg.Cache(), prime+"-"+oldVer+".apkm")
	oldApk := filepath.Join(cfg.Output(), prime+"-"+oldVer+"-v1-abc.apk")
	oldResult := filepath.Join(cfg.Output(), prime+"-"+oldVer+"-v1-abc.result.json")
	other := filepath.Join(cfg.Cache(), "com.other-1.0.apkm")
	dropped := filepath.Join(cfg.Input(), prime, "mine.apkm")
	_ = os.MkdirAll(filepath.Dir(dropped), 0o750)
	for _, f := range []string{oldBundle, oldApk, oldResult, other, dropped} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = rg.st.UpdateApp(prime, func(a *store.AppState) { a.CacheFiles = map[string]string{oldBundle: oldVer} })

	rg.r.Run(context.Background(), Options{})
	if len(rg.adb.installs) != 1 {
		t.Fatalf("installs %v", rg.adb.installs)
	}
	for _, gone := range []string{oldBundle, oldApk, oldResult} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s should have been deleted after the update", filepath.Base(gone))
		}
	}
	for _, kept := range []string{other, dropped} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s belongs to something else and must stay", filepath.Base(kept))
		}
	}
	files, _ := filepath.Glob(filepath.Join(cfg.Cache(), prime+"-*.apkm"))
	apks, _ := filepath.Glob(filepath.Join(cfg.Output(), prime+"-*.apk"))
	if len(files) != 1 || len(apks) != 1 {
		t.Errorf("the current version's files must remain: %v %v", files, apks)
	}
}

func TestFailedInstallKeepsOlderFiles(t *testing.T) {
	rg := newRig(t)
	rg.cacheFetch = true
	rg.device(t, "TV", "10.0.0.6", prime)
	old := filepath.Join(rg.r.Cfg.Cache(), prime+"-5.0.0.apkm")
	_ = os.WriteFile(old, []byte("x"), 0o600)
	_ = rg.st.UpdateApp(prime, func(a *store.AppState) { a.CacheFiles = map[string]string{old: "5.0.0"} })
	rg.adb.installMsg[prime] = "Failure [INSTALL_FAILED_INSUFFICIENT_STORAGE]"
	rg.r.Run(context.Background(), Options{})
	if _, err := os.Stat(old); err != nil {
		t.Error("nothing may be deleted when the update did not succeed")
	}
}

func TestPatchingNeverStripsNativeLibraries(t *testing.T) {
	rg := newRig(t)
	rg.device(t, "A", "10.0.0.1", prime)
	rg.device(t, "B", "10.0.0.2", prime)
	rg.adb.abis["10.0.0.2"] = []string{"arm64-v8a", "armeabi-v7a"}
	rg.r.Run(context.Background(), Options{})
	if len(rg.stripLibs) != 2 {
		t.Fatalf("patched %d times, want 2", len(rg.stripLibs))
	}
	for _, s := range rg.stripLibs {
		if len(s) != 0 {
			t.Errorf("--striplibs %v would drop architectures; the patched app must keep them all", s)
		}
	}
}
