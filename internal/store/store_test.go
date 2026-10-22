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
	if err := s.SetApps(a.ID, []string{"com.b", "com.a", "com.b"}); err != nil {
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
	if got.Host != "10.0.0.6" || fmt.Sprint(got.Apps) != "[com.a com.b]" {
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
