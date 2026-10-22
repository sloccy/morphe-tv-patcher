// Package apkpure downloads app bundles from APKPure through the apkeep tool. It is a
// second source; apkeep's docs warn that APKPure has modified apps to spread malware, so
// callers must only use files that pass the signer check.
package apkpure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotAvailable: apkeep is missing, or APKPure does not have this version.
var ErrNotAvailable = errors.New("not available from APKPure")

// Options passed to apkeep: the acknowledgement it insists on, and the architecture we run.
const Options = "acknowledge_dangers=true,arch=armeabi-v7a"

var extensions = map[string]bool{".xapk": true, ".apks": true, ".apkm": true, ".apk": true}

// Available reports whether apkeep is installed.
func Available() bool {
	_, err := exec.LookPath("apkeep")
	return err == nil
}

// run is replaced in tests.
var run = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "apkeep", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// Download fetches pkg at version into destDir and returns the file path.
func Download(ctx context.Context, pkg, version, destDir string) (string, error) {
	if !Available() {
		return "", fmt.Errorf("%w: apkeep is not installed", ErrNotAvailable)
	}
	tmp, err := os.MkdirTemp("", "apkeep-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	out, runErr := run(ctx, "-a", pkg+"@"+version, "-d", "apk-pure", "-o", Options, "-s", "3000", tmp)
	entries, _ := os.ReadDir(tmp)
	var best string
	var bestSize int64
	for _, e := range entries {
		if !extensions[filepath.Ext(e.Name())] {
			continue
		}
		if info, err := e.Info(); err == nil && info.Size() >= bestSize {
			best, bestSize = e.Name(), info.Size()
		}
	}
	if runErr != nil || best == "" {
		return "", fmt.Errorf("%w: %s %s: %s", ErrNotAvailable, pkg, version, lastLine(out))
	}
	dest := filepath.Join(destDir, "apkpure-"+pkg+"-"+strings.ReplaceAll(version, " ", "_")+filepath.Ext(best))
	if err := os.Rename(filepath.Join(tmp, best), dest); err != nil {
		return "", err
	}
	return dest, nil
}

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
