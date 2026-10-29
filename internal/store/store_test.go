package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDevicesPersistAndIDsAreUnique(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddDevice("Living room", "10.0.0.6")
	if err != nil || a.ID != "living-room" {
		t.Fatalf("%+v %v", a, err)
	}
	b, err := s.AddDevice("Living Room", "10.0.0.7")
	if err != nil || b.ID != "living-room-2" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := s.AddDevice("Dup", "10.0.0.6"); err == nil {
		t.Error("the same host twice should be refused")
	}
	_ = s.SeedSources([]string{"o/default"}, false)
	if err := s.SetApps(a.ID, []AppChoice{{"com.b", ""}, {"com.a", ""}, {"com.b", ""}}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetInstall(a.ID, "com.a", Install{APKVersion: "1.0", PatchesVersion: "v1"})
	_ = s.SetPort(a.ID, 41234)
	_ = s.UpdateApp("com.a", func(x *AppState) { x.SignerSHA256 = "ab" })

	s2, err := Open(dir) // reload from disk
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Settings().Devices[0]
	if got.Host != "10.0.0.6" || len(got.Apps) != 2 || got.Apps[0] != (AppChoice{"com.a", "o-default"}) || got.Apps[1].Package != "com.b" {
		t.Errorf("%+v", got)
	}
	if in, ok := s2.Install(a.ID, "com.a"); !ok || in.APKVersion != "1.0" {
		t.Errorf("%+v %v", in, ok)
	}
	if s2.Port(a.ID) != 41234 || s2.App("com.a").SignerSHA256 != "ab" {
		t.Error("port or app state lost")
	}
}

func TestRemoveDeviceForgetsItsInstalls(t *testing.T) {
	s, _ := Open(t.TempDir())
	d, _ := s.AddDevice("TV", "10.0.0.6")
	_ = s.SetInstall(d.ID, "com.a", Install{APKVersion: "1"})
	_ = s.SetPort(d.ID, 1111)
	if err := s.RemoveDevice(d.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Install(d.ID, "com.a"); ok || s.Port(d.ID) != 0 || len(s.Settings().Devices) != 0 {
		t.Error("device data should be gone")
	}
	if s.RemoveDevice("nope") == nil {
		t.Error("removing an unknown device should fail")
	}
}

func TestValidHost(t *testing.T) {
	good := []string{"10.0.0.6", "tv.local", "living-room", "a1"}
	bad := []string{"", "-oProxyCommand=x", "10.0.0.6; rm -rf /", "a b", "::1", "300.1.1.1", "host/path", "-1.2.3.4"}
	for _, h := range good {
		if !ValidHost(h) {
			t.Errorf("%q should be valid", h)
		}
	}
	for _, h := range bad {
		if ValidHost(h) {
			t.Errorf("%q should be invalid", h)
		}
	}
	s, _ := Open(t.TempDir())
	if _, err := s.AddDevice("x", "-oBad"); err == nil {
		t.Error("AddDevice must validate the host")
	}
	if _, err := s.AddDevice("   ", "10.0.0.1"); err == nil {
		t.Error("blank name should fail")
	}
}

func TestValidPackage(t *testing.T) {
	good := []string{"com.a", "com.netflix.ninja", "com.netflix.ninja.clone", "a.b_c.d1"}
	bad := []string{"", "com", "com.foo;id>/sdcard/p;", "../patches/evil", "-rf", "com.foo bar",
		"com.foo/../bar", "com..foo", strings.Repeat("a.b", 100)}
	for _, p := range good {
		if !ValidPackage(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range bad {
		if ValidPackage(p) {
			t.Errorf("%q should be invalid", p)
		}
	}

	// the choke point for the page and for a hand-edited settings.json
	s, _ := Open(t.TempDir())
	d, _ := s.AddDevice("tv", "10.0.0.6")
	if err := s.SetApps(d.ID, []AppChoice{{Package: "com.foo;id"}}); err == nil {
		t.Error("an app with a bogus package name must be refused")
	}
	if len(s.Settings().Devices[0].Apps) != 0 {
		t.Error("nothing should have been saved")
	}
	if err := s.SetInstall(d.ID, "com.a", Install{InstalledPackage: "com.a;id"}); err == nil {
		t.Error("a bogus installed package must not reach state.json")
	}
}

func TestConcurrentAccessAndNoTempFilesLeft(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.SetInstall("d", fmt.Sprintf("com.%d", i), Install{APKVersion: "1"})
			_ = s.UpdateApp(fmt.Sprintf("com.%d", i), func(a *AppState) { a.FetchFailures++ })
			_ = s.Settings()
		}(i)
	}
	wg.Wait()
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
	if _, err := Open(dir); err != nil {
		t.Errorf("files written during concurrency must still parse: %v", err)
	}
}

func TestCorruptFileIsAnErrorNotSilentlyEmpty(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{broken"), 0o600)
	if _, err := Open(dir); err == nil {
		t.Error("a corrupt settings file must not be replaced with an empty one")
	}
}

func TestOverridesAreReadFromSettings(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"devices":[],"overrides":{"com.a":{"apkmirror":"pub/app","allow_uninstall":true}}}`), 0o600)
	s, _ := Open(dir)
	if o := s.Override("com.a"); o.APKMirror != "pub/app" || !o.AllowUninstall {
		t.Errorf("%+v", o)
	}
	if o := s.Override("com.zzz"); o.APKMirror != "" {
		t.Error("unknown override should be empty")
	}
}

func TestFirstRunSeedingIsOnceAndEverySourceIsRemovable(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if err := s.SeedSources([]string{"ajstrick81/morphe-androidtv-patches", "MorpheApp/morphe-patches"}, true); err != nil {
		t.Fatal(err)
	}
	got := s.Settings()
	if !got.SourcesSeeded || len(got.Sources) != 2 || got.Sources[0].ID != "ajstrick81-morphe-androidtv-patches" || !got.Sources[0].Prerelease {
		t.Fatalf("%+v", got)
	}
	// seeding again (a restart, or a different PATCHES_REPO) changes nothing
	_ = s.SeedSources([]string{"someone/else"}, false)
	if len(s.Settings().Sources) != 2 {
		t.Error("seeding must happen only once")
	}
	// any source can be removed, including the first one; removing all is respected on restart
	d, _ := s.AddDevice("TV", "10.0.0.6")
	_ = s.SetApps(d.ID, []AppChoice{{"com.a", ""}, {"com.b", "morpheapp-morphe-patches"}})
	if apps := s.Settings().Devices[0].Apps; apps[0].Source != "ajstrick81-morphe-androidtv-patches" {
		t.Errorf("an empty source means the first one: %+v", apps)
	}
	n, err := s.RemoveSource("ajstrick81-morphe-androidtv-patches")
	if err != nil || n != 1 {
		t.Fatalf("removed %d %v", n, err)
	}
	if _, err := s.RemoveSource("morpheapp-morphe-patches"); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(dir)
	_ = s2.SeedSources([]string{"ajstrick81/morphe-androidtv-patches"}, false)
	if got := s2.Settings(); len(got.Sources) != 0 || !got.SourcesSeeded {
		t.Errorf("a removed repository must not come back after a restart: %+v", got)
	}
	if err := s2.SetApps(d.ID, []AppChoice{{"com.a", ""}}); err == nil {
		t.Error("with no patch sources, choosing an app should explain that one is needed")
	}
	// and a repository can be added back by hand
	if _, err := s2.AddSource("ajstrick81/morphe-androidtv-patches", false, false); err != nil {
		t.Error(err)
	}
}

func TestSeedingRejectsBadRepos(t *testing.T) {
	s, _ := Open(t.TempDir())
	if err := s.SeedSources([]string{"ok/repo", "not a repo"}, false); err == nil {
		t.Error("an invalid PATCHES_REPO entry should be reported")
	}
}

func TestPatchSourcesLifecycle(t *testing.T) {
	s, _ := Open(t.TempDir())
	_ = s.SeedSources([]string{"owner/first"}, false)
	m, err := s.AddSource("MorpheApp/morphe-patches", false, true)
	if err != nil || m.ID != "morpheapp-morphe-patches" || !m.AllowPhone {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := s.AddSource("morpheapp/MORPHE-PATCHES", false, false); err == nil {
		t.Error("the same repository twice (any case) should be refused")
	}
	if _, err := s.AddSource("owner/first", false, false); err == nil {
		t.Error("a seeded repository cannot be added again")
	}
	for _, bad := range []string{"", "noslash", "a/b/c", "../x/y", "a b/c", "-rf/x;y", "owner/", "/name", "a/..", "a/b c"} {
		if _, err := s.AddSource(bad, false, false); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if _, err := s.RemoveSource("nope"); err == nil {
		t.Error("unknown source")
	}
	d, _ := s.AddDevice("TV", "10.0.0.6")
	if err := s.SetApps(d.ID, []AppChoice{{"com.a", "owner-first"}, {"com.yt", m.ID}, {"com.yt", "owner-first"}}); err != nil {
		t.Fatal(err)
	}
	apps := s.Settings().Devices[0].Apps
	if len(apps) != 2 || apps[1] != (AppChoice{"com.yt", m.ID}) {
		t.Errorf("one source per app, the first choice wins: %+v", apps)
	}
	if err := s.SetApps(d.ID, []AppChoice{{"com.a", "ghost"}}); err == nil {
		t.Error("an unknown source must be refused")
	}
	n, err := s.RemoveSource(m.ID)
	if err != nil || n != 1 {
		t.Fatalf("removed %d %v", n, err)
	}
	if apps := s.Settings().Devices[0].Apps; len(apps) != 1 || apps[0].Package != "com.a" {
		t.Errorf("removing a source drops its app choices: %+v", apps)
	}
}

func TestOlderPlainStringAppsStillLoad(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"sources":[{"id":"x","repo":"o/x"}],"devices":[{"id":"tv","name":"TV","host":"10.0.0.6","apps":["com.a",{"package":"com.b","source":"x"},{"package":"com.c"}]}]}`), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Settings().Devices[0].Apps
	want := []AppChoice{{"com.a", ""}, {"com.b", "x"}, {"com.c", ""}} // an empty source means the first one
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("%+v", got)
	}
}

func TestDeviceInfoRoundTripAndRemoval(t *testing.T) {
	s, _ := Open(t.TempDir())
	d, _ := s.AddDevice("TV", "10.0.0.6")
	_ = s.SetDeviceInfo(d.ID, DeviceInfo{ABIs: []string{"arm64-v8a", "armeabi-v7a"}, SDK: 34, SeenAt: "t"})
	in, ok := s.DeviceInfo(d.ID)
	if !ok || in.SDK != 34 || len(in.ABIs) != 2 {
		t.Errorf("%+v %v", in, ok)
	}
	in.ABIs[0] = "mutated"
	if again, _ := s.DeviceInfo(d.ID); again.ABIs[0] != "arm64-v8a" {
		t.Error("DeviceInfo must return a copy")
	}
	_ = s.RemoveDevice(d.ID)
	if _, ok := s.DeviceInfo(d.ID); ok {
		t.Error("removing a device forgets its CPU info")
	}
}

func TestBuildsAreRecordedAndBounded(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if _, ok := s.Builds("com.a", "1"); ok {
		t.Fatal("nothing known yet")
	}
	_ = s.SetBuilds("com.a", "1", nil) // nothing to record
	if _, ok := s.Builds("com.a", "1"); ok {
		t.Error("an empty list is not knowledge")
	}
	_ = s.SetBuilds("com.a", "1", []string{"armeabi-v7a"})
	got, ok := s.Builds("com.a", "1")
	if !ok || len(got) != 1 || got[0] != "armeabi-v7a" {
		t.Errorf("%v %v", got, ok)
	}
	got[0] = "mutated"
	if again, _ := s.Builds("com.a", "1"); again[0] != "armeabi-v7a" {
		t.Error("Builds must return a copy")
	}
	for i := range 10 {
		_ = s.SetBuilds("com.a", fmt.Sprintf("v%d", i), []string{"*"})
	}
	s2, _ := Open(dir) // survives a restart, and stays small
	if n := len(s2.App("com.a").Builds); n > 6 {
		t.Errorf("old versions should be dropped, have %d", n)
	}
	if _, ok := s2.Builds("com.a", "v9"); !ok {
		t.Error("the latest version must be kept")
	}
}

// ---- app sets ----

func setsRig(t *testing.T) (st *Store, first, second Device) {
	t.Helper()
	s, _ := Open(t.TempDir())
	_ = s.SeedSources([]string{"o/one", "o/two"}, false)
	a, _ := s.AddDevice("Living room", "10.0.0.6")
	b, _ := s.AddDevice("Bedroom", "10.0.0.7")
	return s, a, b
}

func pkgs(es []EffectiveApp) string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Package+"@"+e.Source+"<"+e.Via)
	}
	return strings.Join(out, " ")
}

func TestSetsAreGivenToSeveralDevices(t *testing.T) {
	s, a, b := setsRig(t)
	set, err := s.AddSet("Streaming", []AppChoice{{"com.netflix", ""}, {"com.disney", "o-two"}}, []string{a.ID, b.ID})
	if err != nil || set.ID != "streaming" {
		t.Fatalf("%+v %v", set, err)
	}
	for _, d := range []Device{a, b} {
		if got := pkgs(s.EffectiveApps(d.ID)); got != "com.disney@o-two<set:streaming com.netflix@o-one<set:streaming" {
			t.Errorf("%s: %s", d.Name, got)
		}
	}
	// edit the set once: both devices follow
	devs := []string{a.ID, b.ID}
	if err := s.UpdateSet("streaming", "Streaming", []AppChoice{{"com.netflix", "o-one"}}, &devs); err != nil {
		t.Fatal(err)
	}
	if got := pkgs(s.EffectiveApps(b.ID)); got != "com.netflix@o-one<set:streaming" {
		t.Errorf("%s", got)
	}
	// assign from the set side to only one device
	only := []string{a.ID}
	_ = s.UpdateSet("streaming", "Streaming", []AppChoice{{"com.netflix", ""}}, &only)
	if len(s.EffectiveApps(b.ID)) != 0 || len(s.EffectiveApps(a.ID)) != 1 {
		t.Error("assignment from the set side should add and remove devices")
	}
	// and from the device side, with order
	set2, _ := s.AddSet("Kids", []AppChoice{{"com.kids", ""}}, nil)
	_ = s.SetDeviceSets(b.ID, []string{set2.ID, "streaming", set2.ID})
	if got := s.Settings().Devices[1].Sets; len(got) != 2 || got[0] != "kids" {
		t.Errorf("sets keep their order and are not repeated: %v", got)
	}
}

func TestExtrasWinThenFirstSetWins(t *testing.T) {
	s, a, _ := setsRig(t)
	s1, _ := s.AddSet("One", []AppChoice{{"com.x", "o-one"}, {"com.only1", ""}}, nil)
	s2, _ := s.AddSet("Two", []AppChoice{{"com.x", "o-two"}, {"com.only2", ""}}, nil)
	_ = s.SetDeviceSets(a.ID, []string{s2.ID, s1.ID})
	if got := pkgs(s.EffectiveApps(a.ID)); got != "com.only1@o-one<set:one com.only2@o-one<set:two com.x@o-two<set:two" {
		t.Errorf("the first set wins a clash: %s", got)
	}
	_ = s.SetApps(a.ID, []AppChoice{{"com.x", "o-one"}}) // the device's own choice beats every set
	if got := pkgs(s.EffectiveApps(a.ID)); !strings.Contains(got, "com.x@o-one<device") {
		t.Errorf("a device's own extras win: %s", got)
	}
	if got := s.EffectiveApps("nope"); got != nil {
		t.Errorf("%v", got)
	}
}

func TestSetValidationAndRemoval(t *testing.T) {
	s, a, b := setsRig(t)
	if _, err := s.AddSet("  ", nil, nil); err == nil {
		t.Error("blank name")
	}
	if _, err := s.AddSet(strings.Repeat("x", 61), nil, nil); err == nil {
		t.Error("overlong name")
	}
	set, err := s.AddSet("Movies", []AppChoice{{"com.a", ""}}, []string{a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSet("movies", nil, nil); err == nil {
		t.Error("names are unique, any case")
	}
	if _, err := s.AddSet("Bad source", []AppChoice{{"com.a", "ghost"}}, nil); err == nil {
		t.Error("unknown source")
	}
	if _, err := s.AddSet("Bad device", nil, []string{"ghost"}); err == nil {
		t.Error("unknown device")
	}
	if len(s.Settings().Sets) != 1 {
		t.Error("a refused set must not be saved (even half-created)")
	}
	if _, err := s.AddSet("", nil, nil); err == nil {
		t.Error("empty")
	}
	if err := s.SetDeviceSets(b.ID, []string{"ghost"}); err == nil {
		t.Error("unknown set")
	}
	if err := s.UpdateSet("ghost", "X", nil, nil); err == nil {
		t.Error("unknown set update")
	}
	if err := s.RemoveSet(set.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.Settings(); len(got.Sets) != 0 || len(got.Devices[0].Sets) != 0 {
		t.Errorf("removing a set takes it off every device: %+v", got)
	}
	if s.RemoveSet(set.ID) == nil {
		t.Error("removing twice")
	}
}

func TestPreviewDoesNotChangeAnything(t *testing.T) {
	s, a, _ := setsRig(t)
	set, _ := s.AddSet("Streaming", []AppChoice{{"com.a", ""}}, nil)
	before := s.Settings()
	after := s.Preview(func(st *Settings) {
		_ = st.Assign(set.ID, []string{a.ID})
		st.Sets[0].Apps = append(st.Sets[0].Apps, AppChoice{Package: "com.b", Source: "o-one"})
	})
	if got := Effective(after, a.ID); len(got) != 2 {
		t.Errorf("the preview shows the proposed result: %+v", got)
	}
	if got := s.EffectiveApps(a.ID); len(got) != 0 || len(s.Settings().Sets[0].Apps) != 1 || len(before.Devices[0].Sets) != 0 {
		t.Error("Preview must not touch the store")
	}
}

func TestSetsSurviveRestartAndOldFilesLoad(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	_ = s.SeedSources([]string{"o/one"}, false)
	d, _ := s.AddDevice("TV", "10.0.0.6")
	_, _ = s.AddSet("Streaming", []AppChoice{{"com.a", ""}}, []string{d.ID})
	s2, _ := Open(dir)
	if got := pkgs(s2.EffectiveApps("tv")); got != "com.a@o-one<set:streaming" {
		t.Errorf("%s", got)
	}
	old := t.TempDir()
	_ = os.WriteFile(filepath.Join(old, "settings.json"), []byte(`{"sources":[{"id":"x","repo":"o/x"}],"devices":[{"id":"tv","name":"TV","host":"10.0.0.6","apps":[{"package":"com.a","source":"x"}]}]}`), 0o600)
	s3, err := Open(old)
	if err != nil {
		t.Fatal(err)
	}
	if got := pkgs(s3.EffectiveApps("tv")); got != "com.a@x<device" {
		t.Errorf("a file without sets keeps working; its apps are the device's extras: %s", got)
	}
}

func TestPatchedBuildsAreRememberedAndBounded(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if s.PatchedID("com.a", "1|aaa") != "" {
		t.Fatal("nothing built yet")
	}
	_ = s.SetPatchedID("com.a", "1|aaa", "")
	_ = s.SetPatchedID("com.a", "", "1:deadbeef")
	if s.PatchedID("com.a", "1|aaa") != "" {
		t.Error("an id or a key that is missing is not knowledge")
	}
	_ = s.SetPatchedID("com.a", "1|aaa", "1:deadbeef")
	if s.PatchedID("com.a", "1|aaa") != "1:deadbeef" {
		t.Errorf("got %q", s.PatchedID("com.a", "1|aaa"))
	}
	for i := range maxPatchedKeys + 4 {
		_ = s.SetPatchedID("com.a", fmt.Sprintf("2|%d", i), fmt.Sprintf("1:%d", i))
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.state.Apps["com.a"].Patched); n > maxPatchedKeys {
		t.Errorf("the file would grow for ever: %d keys kept", n)
	}
	if again.PatchedID("com.a", fmt.Sprintf("2|%d", maxPatchedKeys+3)) == "" {
		t.Error("the newest build must survive a restart")
	}
}

func TestInstallRemembersWhatIsOnTheDevice(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	in := Install{APKVersion: "1.2.3", PatchesVersion: "v1", PatchSource: "o-x", ContentID: "1:abc",
		InstalledAt: "2026-10-28T00:00:00Z"}
	if err := s.SetInstall("tv", "com.a", in); err != nil {
		t.Fatal(err)
	}
	again, _ := Open(dir)
	got, ok := again.Install("tv", "com.a")
	if !ok || got != in {
		t.Errorf("%+v", got)
	}
	// a state.json from before builds had an identity still loads, with no identity
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"installs":{"tv/com.a":{"apk_version":"1.2.3","patches_version":"v1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok = old.Install("tv", "com.a"); !ok || got.ContentID != "" || got.APKVersion != "1.2.3" {
		t.Errorf("%+v %v", got, ok)
	}
}
