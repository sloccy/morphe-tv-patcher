// Package sources finds a verified app bundle: a file you dropped in, the cache, APKMirror
// (direct, then through FlareSolverr), then APKPure. The first file that passes every check
// wins. Nothing is ever downgraded: if the newest supported version cannot be found, that is
// reported instead of falling back to an older one.
package sources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sloccy/morphe-tv-patcher/internal/apkmirror"
	"github.com/sloccy/morphe-tv-patcher/internal/apkpure"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
	"github.com/sloccy/morphe-tv-patcher/internal/verify"
)

// App is what to fetch.
type App struct {
	Package      string
	Name         string
	Version      string
	Sources      []string // order of "apkmirror", "apkpure"; default both
	SignerPin    string   // from a settings override; otherwise the recorded signer is used
	SlugOverride string
}

// Env holds the dependencies (functions so tests can fake the network).
type Env struct {
	InputDir  string // <InputDir>/<package>/ is the drop folder
	CacheDir  string
	Store     *store.Store
	FlareURL  string
	DirectOK  bool // APKMirror direct requests allowed
	APKPureOK bool // the user acknowledged apkeep's APKPure warning

	Mirror        func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error)
	Pure          func(ctx context.Context, pkg, version, destDir string) (string, error)
	PureAvailable func() bool
	Verify        func(ctx context.Context, path, pkg, version, source, pinned string) (*verify.Verified, error)
}

// DefaultEnv wires the real implementations.
func DefaultEnv() Env {
	return Env{
		DirectOK:      true,
		Mirror:        apkmirror.Download,
		Pure:          apkpure.Download,
		PureAvailable: apkpure.Available,
		Verify:        verify.Verify,
	}
}

// Fetched is a verified bundle.
type Fetched struct {
	*verify.Verified
	Detail        string // "dropfolder", "cache", "apkmirror (chrome)", "apkpure", ...
	LearnedSigner bool   // first time we recorded this app's signer
}

// FetchError says why nothing worked. Fatal means a wrong or tampered file was seen.
type FetchError struct {
	Attempts []string
	Fatal    bool
}

func (e *FetchError) Error() string {
	if len(e.Attempts) == 0 {
		return "no source available"
	}
	return strings.Join(e.Attempts, "; ")
}

var bundleExt = map[string]bool{".apkm": true, ".xapk": true, ".apks": true, ".apk": true}

// Fetch finds, downloads and verifies the app at a.Version. sdk is the device API level (0 unknown).
func Fetch(ctx context.Context, e *Env, a App, sdk int) (*Fetched, error) {
	pinned := a.SignerPin
	if pinned == "" {
		pinned = e.Store.App(a.Package).SignerSHA256
	}
	var attempts, fatals []string
	finish := func(v *verify.Verified, detail string) *Fetched {
		learned := pinned == ""
		if learned {
			_ = e.Store.UpdateApp(a.Package, func(s *store.AppState) { s.SignerSHA256 = v.Signer })
			slog.Warn("recorded signer for app (pin it with signer_sha256 to lock it in)", "package", a.Package, "signer", v.Signer)
		}
		return &Fetched{Verified: v, Detail: detail, LearnedSigner: learned}
	}
	note := func(label string, err error) {
		var ve *verify.Error
		if errors.As(err, &ve) && ve.Fatal() {
			fatals = append(fatals, fmt.Sprintf("%s: %v", label, err))
		} else {
			attempts = append(attempts, fmt.Sprintf("%s: %v", label, err))
		}
		slog.Warn("source failed", "source", label, "err", err)
	}

	// 1. files you dropped in, 2. the cache
	for _, loc := range []struct{ dir, label string }{
		{filepath.Join(e.InputDir, a.Package), verify.SourceDropFolder},
		{e.CacheDir, "cache"},
	} {
		entries, _ := os.ReadDir(loc.dir)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, en := range entries {
			if en.IsDir() || !bundleExt[filepath.Ext(en.Name())] {
				continue
			}
			p := filepath.Join(loc.dir, en.Name())
			v, err := e.Verify(ctx, p, a.Package, a.Version, loc.label, pinned)
			if err == nil {
				if loc.label == verify.SourceDropFolder && pinned == "" {
					slog.Warn("manual file accepted with no signer on record", "package", a.Package)
				}
				return finish(v, loc.label), nil
			}
			var ve *verify.Error
			if errors.As(err, &ve) {
				// an old version is expected; the cache is shared by every app
				if ve.Kind == verify.KindVersion || (loc.label == "cache" && (ve.Kind == verify.KindUnpinned || ve.Kind == verify.KindPackage)) {
					continue
				}
			}
			note(loc.label+" "+en.Name(), err)
		}
	}

	// 3+. downloads
	download := func(label, source string, fn func() (string, string, error)) *Fetched {
		path, detail, err := fn()
		if err != nil {
			note(label, err)
			return nil
		}
		v, err := e.Verify(ctx, path, a.Package, a.Version, source, pinned)
		if err != nil {
			os.Remove(path)
			note(label, err)
			return nil
		}
		return finish(v, detail)
	}
	srcs := a.Sources
	if len(srcs) == 0 {
		srcs = []string{"apkmirror", "apkpure"}
	}
	app := e.Store.App(a.Package)
	slug := a.SlugOverride
	if slug == "" {
		slug = app.Slug
	}
	req := apkmirror.AppRequest{Package: a.Package, Name: a.Name, Version: a.Version, KnownSlug: slug,
		Hint: apkmirror.Hints[a.Package], SDK: sdk, DestDir: e.CacheDir}
	for _, src := range srcs {
		switch src {
		case "apkmirror":
			run := func(label string, o apkmirror.Options) *Fetched {
				return download(label, verify.SourceAPKMirror, func() (string, string, error) {
					res, used, err := e.Mirror(ctx, req, o)
					if err != nil {
						return "", "", err
					}
					_ = e.Store.UpdateApp(a.Package, func(s *store.AppState) { s.Slug = res.Slug })
					if o.FlareURL == "" {
						_ = e.Store.SetLastProfile(used)
					}
					return res.Path, "apkmirror (" + used + ")", nil
				})
			}
			if e.DirectOK {
				if f := run("apkmirror", apkmirror.Options{ProfileHint: e.Store.LastProfile()}); f != nil {
					return f, nil
				}
			}
			if e.FlareURL != "" {
				if f := run("apkmirror via flaresolverr", apkmirror.Options{FlareURL: e.FlareURL}); f != nil {
					return f, nil
				}
			}
		case "apkpure":
			switch {
			case !e.APKPureOK:
				attempts = append(attempts, "apkpure: off (set APKPURE_ACKNOWLEDGE_DANGERS=true to allow it)")
			case !e.PureAvailable():
				attempts = append(attempts, "apkpure: apkeep not installed")
			default:
				if f := download("apkpure", "apkpure", func() (string, string, error) {
					p, err := e.Pure(ctx, a.Package, a.Version, e.CacheDir)
					return p, "apkpure", err
				}); f != nil {
					return f, nil
				}
			}
		}
	}
	return nil, &FetchError{Attempts: append(attempts, fatals...), Fatal: len(fatals) > 0}
}
