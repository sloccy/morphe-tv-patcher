// Package morphe runs the Morphe CLI (a Java jar) to patch apps.
package morphe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Options describes one patch run.
type Options struct {
	Jar          string // morphe-desktop-*-all.jar
	MPP          string // patches bundle
	Input        string // .apkm / .xapk / .apk
	Out          string // patched APK to write
	Keystore     string // persistent keystore path (created on first success if absent)
	BytecodeMode string // FULL | STRIP_SAFE | STRIP_FAST
	StripLibs    []string
	OptionsFile  string
	Enable       []string
	Disable      []string
}

// ResultFile is the path of the JSON result written next to the output.
func (o Options) ResultFile() string { return strings.TrimSuffix(o.Out, ".apk") + ".result.json" }

// Args builds the java command line. Morphe fails if --keystore names a missing file, so the
// flag is only passed once we have a keystore of our own.
func (o Options) Args() []string {
	a := []string{"-jar", o.Jar, "patch", "-p", o.MPP, "-r", o.ResultFile(), "-o", o.Out}
	if o.BytecodeMode != "" {
		a = append(a, "--bytecode-mode", o.BytecodeMode)
	}
	if o.Keystore != "" {
		if _, err := os.Stat(o.Keystore); err == nil {
			a = append(a, "--keystore", o.Keystore)
		}
	}
	if len(o.StripLibs) > 0 {
		a = append(a, "--striplibs", strings.Join(o.StripLibs, ","))
	}
	if o.OptionsFile != "" {
		a = append(a, "--options-file", o.OptionsFile)
	}
	for _, p := range o.Enable {
		a = append(a, "-e", p)
	}
	for _, p := range o.Disable {
		a = append(a, "-d", p)
	}
	return append(a, o.Input)
}

// runJava is replaced in tests.
var runJava = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "java", args...) //nolint:gosec // fixed binary; arguments are file paths and patch names, never a shell string
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// Patch runs the CLI and checks the result file.
func Patch(ctx context.Context, o Options) error {
	adopt := false
	if o.Keystore != "" {
		_, err := os.Stat(o.Keystore)
		adopt = err != nil
	}
	out, err := runJava(ctx, o.Args()...)
	if err != nil {
		return fmt.Errorf("morphe: %w: %s", err, tail(out, 1500))
	}
	if _, err := os.Stat(o.Out); err != nil {
		return errors.New("morphe produced no output apk")
	}
	if err := CheckResult(o.ResultFile()); err != nil {
		return err
	}
	if adopt {
		if err := AdoptKeystore(out, o.Jar, o.Keystore); err != nil {
			return err
		}
	}
	return nil
}

var dataRoot = regexp.MustCompile(`Morphe data root: (.+?) \(`)

// AdoptKeystore copies the keystore Morphe created on its own (in <jar dir>/morphe-data, named
// in its log) into our persistent location, so later runs sign with the same key.
func AdoptKeystore(log []byte, jar, dest string) error {
	root := filepath.Join(filepath.Dir(jar), "morphe-data")
	if m := dataRoot.FindSubmatch(log); m != nil {
		root = string(m[1])
	}
	src := filepath.Join(root, "morphe.keystore")
	b, err := os.ReadFile(src) //nolint:gosec // path from Morphe's own log line or the jar directory
	if err != nil {
		return fmt.Errorf("patched, but Morphe's keystore not found at %s; the next run would sign with a different key", src)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(dest, b, 0o600); err != nil { //nolint:gosec // dest is our own keystore path from configuration
		return err
	}
	slog.Warn("new signing keystore saved; BACK IT UP, losing it means uninstalling each app before it can update", "path", dest)
	return nil
}

type result struct {
	Success       bool            `json:"success"`
	FailureReason string          `json:"failureReason"`
	FailureDetail string          `json:"failureDetail"`
	FailedPatches json.RawMessage `json:"failedPatches"`
}

// CheckResult fails on anything but a clean result.json.
func CheckResult(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // the result file we asked Morphe to write
	if err != nil {
		return errors.New("morphe wrote no result file")
	}
	var r result
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("unreadable result file: %w", err)
	}
	if !r.Success {
		reason := r.FailureReason
		if reason == "" {
			reason = r.FailureDetail
		}
		return fmt.Errorf("patching failed: %s", reason)
	}
	if s := strings.TrimSpace(string(r.FailedPatches)); s != "" && s != "[]" && s != "null" {
		return fmt.Errorf("patches failed: %.400s", s)
	}
	return nil
}

var versionLine = regexp.MustCompile(`^\s+(\S.*?)\s+\(\d+ patch(?:es)?\)\s*$`)

// ParseListVersions reads `list-versions` output (lines like "\t6.23.23+v15… (5 patches)").
func ParseListVersions(out []byte) []string {
	var vs []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if m := versionLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			vs = append(vs, m[1])
		}
	}
	return vs
}

// ListPatches runs `list-patches -p -v` (with -x to include experimental versions) and returns
// its output; patches.ParseListPatches reads it.
func ListPatches(ctx context.Context, jar, mpp string, experimental bool) ([]byte, error) {
	args := []string{"-jar", jar, "list-patches", "--patches", mpp, "-p", "-v"}
	if experimental {
		args = append(args, "-x")
	}
	out, err := runJava(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list-patches: %w: %s", err, tail(out, 400))
	}
	return out, nil
}

// ListVersions asks the CLI which versions of pkg the bundle supports; nil on any problem.
func ListVersions(ctx context.Context, jar, mpp, pkg string) []string {
	out, err := runJava(ctx, "-jar", jar, "list-versions", "--patches", mpp, "-f", pkg)
	if err != nil {
		slog.Warn("list-versions failed", "err", err)
		return nil
	}
	return ParseListVersions(out)
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

var _ = io.Discard
