package store

import (
	"fmt"
	"os"
	"path/filepath"
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
	_ = s.EnsureDefaultSource("o/default", false)
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
	if got.Host != "10.0.0.6" || len(got.Apps) != 2 || got.Apps[0] != (AppChoice{"com.a", DefaultSourceID}) || got.Apps[1].Package != "com.b" {
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

func TestPatchSourcesLifecycle(t *testing.T) {
	s, _ := Open(t.TempDir())
	if err := s.EnsureDefaultSource("owner/default-patches", true); err != nil {
		t.Fatal(err)
	}
	if got := s.Settings().Sources; len(got) != 1 || !got[0].Default || got[0].ID != DefaultSourceID || !got[0].Prerelease {
		t.Fatalf("%+v", got)
	}
	// the environment decides the default repository, and a restart picks up a change
	_ = s.EnsureDefaultSource("owner/other", false)
	if got := s.Settings().Sources; len(got) != 1 || got[0].Repo != "owner/other" || got[0].Prerelease {
		t.Errorf("%+v", got)
	}
	m, err := s.AddSource("MorpheApp/morphe-patches", false, true)
	if err != nil || m.ID != "morpheapp-morphe-patches" || !m.AllowPhone || m.Default {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := s.AddSource("morpheapp/MORPHE-PATCHES", false, false); err == nil {
		t.Error("the same repository twice (any case) should be refused")
	}
	if _, err := s.AddSource("owner/other", false, false); err == nil {
		t.Error("the default repository cannot be added again")
	}
	for _, bad := range []string{"", "noslash", "a/b/c", "../x/y", "a b/c", "-rf/x;y", "owner/", "/name", "a/..", "a/b c"} {
		if _, err := s.AddSource(bad, false, false); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if _, err := s.RemoveSource(DefaultSourceID); err == nil {
		t.Error("the default source must not be removable")
	}
	if _, err := s.RemoveSource("nope"); err == nil {
		t.Error("unknown source")
	}
	d, _ := s.AddDevice("TV", "10.0.0.6")
	if err := s.SetApps(d.ID, []AppChoice{{"com.a", DefaultSourceID}, {"com.yt", m.ID}, {"com.yt", DefaultSourceID}}); err != nil {
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
		[]byte(`{"devices":[{"id":"tv","name":"TV","host":"10.0.0.6","apps":["com.a",{"package":"com.b","source":"x"},{"package":"com.c"}]}]}`), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Settings().Devices[0].Apps
	want := []AppChoice{{"com.a", DefaultSourceID}, {"com.b", "x"}, {"com.c", DefaultSourceID}}
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
