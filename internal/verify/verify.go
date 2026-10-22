// Package verify runs the checks every candidate app file must pass, whatever its source:
// right package and version, an Android TV launcher, armeabi-v7a, and the publisher's
// signing certificate.
package verify

import (
	"archive/zip"
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
	"sync"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/apk"
)

// TargetABI is the native code this patcher installs for.
const TargetABI = "armeabi-v7a"

// Kind classifies a failed check.
type Kind string

const (
	KindVersion   Kind = "version"
	KindPackage   Kind = "package"
	KindLeanback  Kind = "leanback"
	KindArch      Kind = "arch"
	KindSignature Kind = "signature"
	KindFormat    Kind = "format"
	KindUnpinned  Kind = "unpinned"
)

// Error is a failed check.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Msg }

// Fatal errors mean "wrong or tampered app", never "try again later".
func (e *Error) Fatal() bool {
	return e.Kind == KindSignature || e.Kind == KindLeanback || e.Kind == KindPackage
}

// Sources trusted to establish a signer when none is on record yet.
const (
	SourceAPKMirror  = "apkmirror"
	SourceDropFolder = "dropfolder"
)

// Verified is a file that passed every check.
type Verified struct {
	Path    string
	Source  string
	Signer  string
	Package string
	Version string
}

// Facts are what Inspect learned from a file.
type Facts struct {
	Package     string
	Version     string
	Leanback    bool
	ABIs        map[string]bool
	Signer      string
	SignerOK    *bool // apksigner verdict; nil when apksigner is not installed
	SignerNotes string
}

var abiSplit = regexp.MustCompile(`(?:^|/)(?:split_)?config\.(armeabi_v7a|arm64_v8a|x86_64|x86)\.apk$`)

// PickBase chooses the main APK inside a bundle. APKMirror names it base.apk; APKPure XAPKs
// name it <package>.apk and call the splits config.*.apk (so the largest file is a split).
func PickBase(zr *zip.Reader) (*zip.File, error) {
	var tops []*zip.File
	for _, f := range zr.File {
		if !strings.Contains(f.Name, "/") && strings.HasSuffix(f.Name, ".apk") {
			tops = append(tops, f)
		}
	}
	for _, f := range tops {
		if f.Name == "base.apk" {
			return f, nil
		}
	}
	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			if rc, err := f.Open(); err == nil {
				var m struct {
					Package string `json:"package_name"`
				}
				_ = json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&m)
				rc.Close()
				for _, t := range tops {
					if m.Package != "" && t.Name == m.Package+".apk" {
						return t, nil
					}
				}
			}
		}
	}
	var best *zip.File
	for _, t := range tops {
		if strings.HasPrefix(t.Name, "config.") || strings.HasPrefix(t.Name, "split_config.") {
			continue
		}
		if best == nil || t.UncompressedSize64 > best.UncompressedSize64 {
			best = t
		}
	}
	if best == nil {
		return nil, &Error{KindFormat, "no base apk inside bundle"}
	}
	return best, nil
}

// Inspect reads the facts about a bundle or a plain .apk.
func Inspect(ctx context.Context, path string) (*Facts, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, &Error{KindFormat, "not a zip/apk"}
	}
	defer zr.Close()

	abis := map[string]bool{}
	basePath := path
	var tmp string
	if !strings.HasSuffix(path, ".apk") {
		for _, f := range zr.File {
			if m := abiSplit.FindStringSubmatch(f.Name); m != nil {
				abis[strings.ReplaceAll(m[1], "_", "-")] = true
			}
		}
		bf, err := PickBase(&zr.Reader)
		if err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp("", "base-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		tmp = filepath.Join(dir, "base.apk")
		if err := extract(bf, tmp); err != nil {
			return nil, err
		}
		basePath = tmp
	}

	bz, err := zip.OpenReader(basePath)
	if err != nil {
		return nil, &Error{KindFormat, "base apk is not a zip"}
	}
	defer bz.Close()
	var manifest []byte
	for _, f := range bz.File {
		if strings.HasPrefix(f.Name, "lib/") {
			if parts := strings.Split(f.Name, "/"); len(parts) > 2 {
				abis[parts[1]] = true
			}
		}
		if f.Name == "AndroidManifest.xml" {
			rc, err := f.Open()
			if err != nil {
				return nil, &Error{KindFormat, err.Error()}
			}
			manifest, err = io.ReadAll(io.LimitReader(rc, 16<<20))
			rc.Close()
			if err != nil {
				return nil, &Error{KindFormat, err.Error()}
			}
		}
	}
	m, err := apk.ParseManifest(manifest)
	if err != nil {
		return nil, &Error{KindFormat, "unreadable manifest: " + err.Error()}
	}
	fh, err := os.Open(basePath)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	st, _ := fh.Stat()
	signer, serr := apk.SignerSHA256(fh, st.Size())
	f := &Facts{Package: m.Package, Version: m.VersionName, Leanback: m.Leanback, ABIs: abis, Signer: signer}
	ok, notes, certs := runAPKSigner(ctx, basePath)
	f.SignerOK, f.SignerNotes = ok, notes
	if serr != nil && f.Signer == "" {
		f.Signer = certs // v1-only APK: take the certificate apksigner printed
	}
	return f, nil
}

func extract(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, rc)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Check applies the rules to facts. pinned is the signer on record (may be empty).
func Check(f *Facts, pkg, version, source, pinned string) error {
	switch {
	case f.Package != pkg:
		return &Error{KindPackage, fmt.Sprintf("%s != %s", f.Package, pkg)}
	case f.Version != version:
		return &Error{KindVersion, fmt.Sprintf("%s != %s", f.Version, version)}
	case !f.Leanback:
		return &Error{KindLeanback, "no Android TV launcher activity (phone build?)"}
	case len(f.ABIs) > 0 && !f.ABIs[TargetABI]:
		return &Error{KindArch, fmt.Sprintf("native code for %v, need %s", keys(f.ABIs), TargetABI)}
	case f.Signer == "":
		return &Error{KindSignature, "no signing certificate found"}
	case f.SignerOK != nil && !*f.SignerOK:
		return &Error{KindSignature, "apksigner verify failed: " + f.SignerNotes}
	case pinned != "" && f.Signer != pinned:
		return &Error{KindSignature, fmt.Sprintf("signer %.16s… != pinned %.16s…", f.Signer, pinned)}
	case pinned == "" && source != SourceAPKMirror && source != SourceDropFolder:
		return &Error{KindUnpinned, "no signer on record yet; only APKMirror or a manual drop can set it (" + source + ")"}
	}
	return nil
}

// Verify inspects a file and applies the rules.
func Verify(ctx context.Context, path, pkg, version, source, pinned string) (*Verified, error) {
	f, err := Inspect(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := Check(f, pkg, version, source, pinned); err != nil {
		return nil, err
	}
	return &Verified{Path: path, Source: source, Signer: f.Signer, Package: f.Package, Version: f.Version}, nil
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- apksigner ----

var (
	warnOnce      sync.Once
	signerDigest  = regexp.MustCompile(`(?m)^Signer #1 certificate SHA-256 digest: ([0-9a-fA-F]{64})`)
	lookPath      = exec.LookPath
	execAPKSigner = func(ctx context.Context, exe, apk string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "verify", "--print-certs", apk)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.Bytes(), err
	}
)

// runAPKSigner returns (verdict, notes, signer from its output). nil verdict if not installed.
func runAPKSigner(ctx context.Context, apk string) (*bool, string, string) {
	exe, err := lookPath("apksigner")
	if err != nil {
		warnOnce.Do(func() { slog.Warn("apksigner not found; skipping the cryptographic signature check") })
		return nil, "", ""
	}
	out, err := execAPKSigner(ctx, exe, apk)
	ok := err == nil
	var cert string
	if m := signerDigest.FindSubmatch(out); m != nil {
		cert = strings.ToLower(string(m[1]))
	}
	notes := string(out)
	if len(notes) > 300 {
		notes = notes[:300]
	}
	if errors.Is(err, context.DeadlineExceeded) {
		notes = "apksigner timed out"
	}
	return &ok, notes, cert
}

// PackageOf reads the package name from a plain APK's manifest (used on patched output, where
// a clone patch may have renamed the package, e.g. com.netflix.ninja.clone).
func PackageOf(apkPath string) (string, error) {
	zr, err := zip.OpenReader(apkPath)
	if err != nil {
		return "", &Error{KindFormat, "not a zip/apk"}
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "AndroidManifest.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			b, err := io.ReadAll(io.LimitReader(rc, 16<<20))
			rc.Close()
			if err != nil {
				return "", err
			}
			m, err := apk.ParseManifest(b)
			if err != nil {
				return "", &Error{KindFormat, err.Error()}
			}
			return m.Package, nil
		}
	}
	return "", &Error{KindFormat, "no AndroidManifest.xml"}
}
