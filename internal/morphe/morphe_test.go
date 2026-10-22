package morphe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestArgsOmitMissingKeystore(t *testing.T) {
	dir := t.TempDir()
	o := Options{Jar: "m.jar", MPP: "p.mpp", Input: "in.apkm", Out: filepath.Join(dir, "o.apk"),
		Keystore: filepath.Join(dir, "ks"), BytecodeMode: "STRIP_SAFE", StripLibs: []string{"armeabi-v7a"},
		Enable: []string{"A"}, Disable: []string{"B"}}
	if strings.Contains(strings.Join(o.Args(), " "), "--keystore") {
		t.Error("missing keystore file must not be passed (Morphe errors on it)")
	}
	_ = os.WriteFile(o.Keystore, []byte("k"), 0o600)
	a := o.Args()
	joined := strings.Join(a, " ")
	for _, want := range []string{"--keystore " + o.Keystore, "--striplibs armeabi-v7a", "--bytecode-mode STRIP_SAFE", "-e A", "-d B"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	if a[len(a)-1] != "in.apkm" {
		t.Errorf("input must be last: %v", a)
	}
	if o.ResultFile() != filepath.Join(dir, "o.result.json") {
		t.Errorf("result file %s", o.ResultFile())
	}
}

func TestCheckResultRealShapes(t *testing.T) {
	if err := CheckResult("testdata/result-ok.json"); err != nil {
		t.Error(err)
	}
	err := CheckResult("testdata/result-fail.json")
	if err == nil || !strings.Contains(err.Error(), "Keystore file not found") {
		t.Errorf("got %v", err)
	}
	if CheckResult("testdata/missing.json") == nil {
		t.Error("missing result must fail")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "r.json")
	_ = os.WriteFile(p, []byte(`{"success":true,"failedPatches":[{"name":"X"}]}`), 0o600)
	if CheckResult(p) == nil {
		t.Error("failed patches must fail")
	}
}

func TestParseListVersionsRealOutput(t *testing.T) {
	out := "INFO: x\nPackage name: com.netflix.ninja\nMost common compatible versions:\n\t13.0.1 build 25028 (6 patches)\n\n" +
		"Package name: com.cbs.ott\nMost common compatible versions:\n\t16.19.0 (1 patch)\n\t16.8.0 (1 patch)\n"
	got := ParseListVersions([]byte(out))
	want := []string{"13.0.1 build 25028", "16.19.0", "16.8.0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestPatchAdoptsKeystore(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data-root")
	_ = os.MkdirAll(root, 0o750)
	_ = os.WriteFile(filepath.Join(root, "morphe.keystore"), []byte("KEY"), 0o600)
	o := Options{Jar: filepath.Join(dir, "m.jar"), MPP: "p", Input: "in", Out: filepath.Join(dir, "o.apk"),
		Keystore: filepath.Join(dir, "state", "morphe.keystore")}
	runJava = func(ctx context.Context, args ...string) ([]byte, error) {
		_ = os.WriteFile(o.Out, []byte("apk"), 0o600)
		_ = os.WriteFile(o.ResultFile(), []byte(`{"success":true,"failedPatches":[]}`), 0o600)
		return []byte("INFO: Morphe data root: " + root + " (JAR-adjacent)\n"), nil
	}
	if err := Patch(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(o.Keystore); string(b) != "KEY" {
		t.Errorf("keystore not adopted: %q", b)
	}
	// a second run passes --keystore and does not need adoption
	if !strings.Contains(strings.Join(o.Args(), " "), "--keystore") {
		t.Error("keystore should now be used")
	}
}
