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
	ABIs         []string // the device's ABIs in order of preference; nil = the 32-bit default
	AllowPhone   bool     // accept phone builds (no Android TV launcher)
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
	Verify        func(ctx context.Context, path string, w verify.Want) (*verify.Verified, error)
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
	// NoBuild means the release exists but has no build for the device's CPU: not a download
	// problem, so it must not feed the "cannot download" alert counters.
	NoBuild bool
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
	noBuild := false
	want := func(source string) verify.Want {
		return verify.Want{Package: a.Package, Version: a.Version, Source: source, Pinned: pinned, ABIs: a.ABIs, AllowPhone: a.AllowPhone}
	}
	finish := func(v *verify.Verified, detail string) *Fetched {
		learned := pinned == ""
		if learned {
			_ = e.Store.UpdateApp(a.Package, func(s *store.AppState) { s.SignerSHA256 = v.Signer })
			slog.Warn("recorded signer for app (pin it with signer_sha256 to lock it in)", "package", a.Package, "signer", v.Signer)
		}
		return &Fetched{Verified: v, Detail: detail, LearnedSigner: learned}
	}
	note := func(label string, err error) {
		if errors.Is(err, apkmirror.ErrNoBuild) {
			noBuild = true
		}
		if ve, ok := errors.AsType[*verify.Error](err); ok {
			noBuild = noBuild || ve.Kind == verify.KindArch
			if ve.Fatal() {
				fatals = append(fatals, fmt.Sprintf("%s: %v", label, err))
				slog.Warn("source failed", "source", label, "err", err)
				return
			}
		}
		attempts = append(attempts, fmt.Sprintf("%s: %v", label, err))
		slog.Warn("source failed", "source", label, "err", err)
	}

	// 1. files you dropped in, 2. the cache. A file that matches the device's best ABI wins at
	// once; otherwise keep the best seen and look online for a better one.
	var bestLocal *Fetched
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
			v, err := e.Verify(ctx, p, want(loc.label))
			if err == nil {
				if loc.label == verify.SourceDropFolder && pinned == "" {
					slog.Warn("manual file accepted with no signer on record", "package", a.Package)
				}
				if v.Rank == 0 {
					return finish(v, loc.label), nil
				}
				if bestLocal == nil || v.Rank < bestLocal.Rank {
					bestLocal = &Fetched{Verified: v, Detail: loc.label}
				}
				continue
			}
			if ve, ok := errors.AsType[*verify.Error](err); ok {
				// an old version is expected; the cache is shared by every app, and its files
				// may be for another CPU
				if ve.Kind == verify.KindVersion || (loc.label == "cache" && (ve.Kind == verify.KindUnpinned || ve.Kind == verify.KindPackage || ve.Kind == verify.KindArch)) {
					continue
				}
			}
			note(loc.label+" "+en.Name(), err)
		}
	}
	// withBest returns the downloaded file unless a local one matches the device better.
	withBest := func(f *Fetched) (*Fetched, error) {
		if bestLocal != nil && bestLocal.Rank < f.Rank {
			return finish(bestLocal.Verified, bestLocal.Detail), nil
		}
		return f, nil
	}

	// 3+. downloads
	download := func(label, source string, fn func() (string, string, error)) *Fetched {
		path, detail, err := fn()
		if err != nil {
			note(label, err)
			return nil
		}
		v, err := e.Verify(ctx, path, want(source))
		if err != nil {
			_ = os.Remove(path)
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
		Hint: apkmirror.Hints[a.Package], SDK: sdk, DestDir: e.CacheDir, ABIs: a.ABIs, AllowPhone: a.AllowPhone}
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
					return withBest(f)
				}
			}
			if e.FlareURL != "" {
				if f := run("apkmirror via flaresolverr", apkmirror.Options{FlareURL: e.FlareURL}); f != nil {
					return withBest(f)
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
					return withBest(f)
				}
			}
		}
	}
	if bestLocal != nil { // downloads failed or found nothing, but a usable (if not ideal) file exists
		return finish(bestLocal.Verified, bestLocal.Detail), nil
	}
	return nil, &FetchError{Attempts: append(attempts, fatals...), Fatal: len(fatals) > 0, NoBuild: noBuild && len(fatals) == 0}
}
