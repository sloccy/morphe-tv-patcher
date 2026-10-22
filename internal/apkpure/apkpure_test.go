package apkpure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeApkeep(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "apkeep")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil { //nolint:gosec // a test stub that must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDownloadPicksTheBundleAndPassesOptions(t *testing.T) {
	// last argument is the output dir; record the args so the test can check them
	dir := t.TempDir()
	fakeApkeep(t, `echo "$@" > `+filepath.Join(dir, "args")+`
for last; do :; done
echo small > "$last/icon.apk"
echo bundle-contents > "$last/com.x@1.0@armeabi-v7a.xapk"
`)
	dest := t.TempDir()
	p, err := Download(context.Background(), "com.x", "1.0", dest)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "apkpure-com.x-1.0.xapk" {
		t.Errorf("path %s", p)
	}
	if left, _ := filepath.Glob(filepath.Join(dest, ".apkeep-*")); len(left) != 0 {
		t.Errorf("temp dir left behind: %v", left)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args")) //nolint:gosec // test temp dir
	for _, want := range []string{"-a com.x@1.0", "-d apk-pure", "acknowledge_dangers=true", "arch=armeabi-v7a"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "bundle-contents") { //nolint:gosec // test temp dir
		t.Errorf("wrong file chosen: %q", b)
	}
}

func TestDownloadNotAvailable(t *testing.T) {
	fakeApkeep(t, `echo "No such version"; exit 1`)
	_, err := Download(context.Background(), "com.x", "9.9", t.TempDir())
	if !errors.Is(err, ErrNotAvailable) || !strings.Contains(err.Error(), "No such version") {
		t.Errorf("got %v", err)
	}
}

func TestMissingApkeep(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if Available() {
		t.Fatal("apkeep should not be found")
	}
	if _, err := Download(context.Background(), "a", "1", t.TempDir()); !errors.Is(err, ErrNotAvailable) {
		t.Errorf("got %v", err)
	}
}
