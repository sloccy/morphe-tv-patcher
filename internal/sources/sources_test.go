package sources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sloccy/morphe-tv-patcher/internal/apkmirror"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
	"github.com/sloccy/morphe-tv-patcher/internal/verify"
)

type rig struct {
	env *Env
	st  *store.Store
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st}
	base := t.TempDir()
	e := &Env{InputDir: filepath.Join(base, "input"), CacheDir: filepath.Join(base, "cache"), Store: st,
		DirectOK: true, APKPureOK: true, PureAvailable: func() bool { return true }}
	os.MkdirAll(e.CacheDir, 0o755)
	e.Verify = func(ctx context.Context, path, pkg, version, source, pinned string) (*verify.Verified, error) {
		return &verify.Verified{Path: path, Source: source, Signer: "aa", Package: pkg, Version: version}, nil
	}
	e.Mirror = func(ctx context.Context, req apkmirror.AppRequest, o apkmirror.Options) (apkmirror.Result, string, error) {
		p := filepath.Join(req.DestDir, "m.apkm")
		os.WriteFile(p, []byte("PK"), 0o644)
		return apkmirror.Result{Path: p, Slug: "pub/app"}, "chrome", nil
	}
	e.Pure = func(ctx context.Context, pkg, version, dest string) (string, error) {
		p := filepath.Join(dest, "p.xapk")
		os.WriteFile(p, []byte("PK"), 0o644)
		return p, nil
	}
	r.env = e
	return r
}

var app = App{Package: "com.x", Name: "X", Version: "1.0"}

func TestAPKMirrorFirstLearnsSignerSlugAndProfile(t *testing.T) {
	r := newRig(t)
	f, err := Fetch(context.Background(), r.env, app, 31)
	if err != nil || f.Detail != "apkmirror (chrome)" || !f.LearnedSigner {
		t.Fatalf("%+v %v", f, err)
	}
	s := r.st.App("com.x")
	if s.SignerSHA256 != "aa" || s.Slug != "pub/app" || r.st.LastProfile() != "chrome" {
		t.Errorf("state %+v profile %q", s, r.st.LastProfile())
	}
	os.Remove(filepath.Join(r.env.CacheDir, "m.apkm"))
	f, _ = Fetch(context.Background(), r.env, app, 31)
	if f.LearnedSigner {
		t.Error("signer already on record")
	}
}

func TestFallsThroughDirectThenFlareThenAPKPure(t *testing.T) {
	r := newRig(t)
	r.env.FlareURL = "http://flare"
	r.st.UpdateApp("com.x", func(s *store.AppState) { s.SignerSHA256 = "aa" })
	var order []string
	r.env.Mirror = func(ctx context.Context, req apkmirror.AppRequest, o apkmirror.Options) (apkmirror.Result, string, error) {
		if o.FlareURL != "" {
			order = append(order, "flare")
		} else {
			order = append(order, "direct")
		}
		return apkmirror.Result{}, "", apkmirror.ErrBlocked
	}
	f, err := Fetch(context.Background(), r.env, app, 31)
	if err != nil || f.Detail != "apkpure" {
		t.Fatalf("%+v %v", f, err)
	}
	if strings.Join(order, ",") != "direct,flare" {
		t.Errorf("order %v", order)
	}
}

func TestAPKPureIsOffWithoutOptIn(t *testing.T) {
	r := newRig(t)
	r.env.APKPureOK = false
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		return apkmirror.Result{}, "", apkmirror.ErrBlocked
	}
	r.env.Pure = func(context.Context, string, string, string) (string, error) {
		t.Fatal("must not download")
		return "", nil
	}
	_, err := Fetch(context.Background(), r.env, app, 31)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Fatal || !strings.Contains(fe.Error(), "APKPURE_ACKNOWLEDGE_DANGERS") {
		t.Fatalf("got %v", err)
	}
}

func TestMirrorOnlySourcesNeverTouchAPKPure(t *testing.T) {
	r := newRig(t)
	a := app
	a.Sources = []string{"apkmirror"}
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		return apkmirror.Result{}, "", apkmirror.ErrNotFound
	}
	r.env.Pure = func(context.Context, string, string, string) (string, error) {
		t.Fatal("apkpure not allowed")
		return "", nil
	}
	if _, err := Fetch(context.Background(), r.env, a, 31); err == nil {
		t.Error("should fail")
	}
}

func TestSignatureMismatchIsFatalAndTheFileIsDeleted(t *testing.T) {
	r := newRig(t)
	r.env.Verify = func(ctx context.Context, path, pkg, version, source, pinned string) (*verify.Verified, error) {
		return nil, &verify.Error{Kind: verify.KindSignature, Msg: "signer mismatch"}
	}
	r.env.APKPureOK = false
	_, err := Fetch(context.Background(), r.env, app, 31)
	var fe *FetchError
	if !errors.As(err, &fe) || !fe.Fatal {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(r.env.CacheDir, "m.apkm")); statErr == nil {
		t.Error("a rejected download must be removed")
	}
}

func TestDropFolderWinsAndOldVersionsAreIgnored(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(r.env.InputDir, "com.x")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "old.apkm"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "new.apkm"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	r.env.Verify = func(ctx context.Context, path, pkg, version, source, pinned string) (*verify.Verified, error) {
		if filepath.Base(path) == "old.apkm" {
			return nil, &verify.Error{Kind: verify.KindVersion, Msg: "old"}
		}
		return &verify.Verified{Path: path, Source: source, Signer: "cc"}, nil
	}
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		t.Fatal("should not scrape when a good file was dropped in")
		return apkmirror.Result{}, "", nil
	}
	f, err := Fetch(context.Background(), r.env, app, 31)
	if err != nil || f.Detail != "dropfolder" || filepath.Base(f.Path) != "new.apkm" {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestCacheIgnoresOtherAppsFilesAndTheyAreNotFatal(t *testing.T) {
	r := newRig(t)
	os.WriteFile(filepath.Join(r.env.CacheDir, "other.apkm"), []byte("x"), 0o644)
	r.env.Verify = func(ctx context.Context, path, pkg, version, source, pinned string) (*verify.Verified, error) {
		if filepath.Base(path) == "other.apkm" {
			return nil, &verify.Error{Kind: verify.KindPackage, Msg: "other app"}
		}
		return &verify.Verified{Path: path, Source: source, Signer: "aa"}, nil
	}
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		return apkmirror.Result{}, "", apkmirror.ErrBlocked
	}
	r.env.APKPureOK = false
	_, err := Fetch(context.Background(), r.env, app, 31)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Fatal {
		t.Errorf("got %v (another app's cached file is not a wrong-app alarm)", err)
	}
}

func TestSlugOverrideHintAndSDKArePassedThrough(t *testing.T) {
	r := newRig(t)
	var got apkmirror.AppRequest
	r.env.Mirror = func(ctx context.Context, req apkmirror.AppRequest, o apkmirror.Options) (apkmirror.Result, string, error) {
		got = req
		return apkmirror.Result{}, "", apkmirror.ErrNotFound
	}
	a := app
	a.SlugOverride = "me/override"
	a.Package = "com.netflix.ninja"
	Fetch(context.Background(), r.env, a, 31)
	if got.KnownSlug != "me/override" || got.Hint != apkmirror.Hints["com.netflix.ninja"] || got.SDK != 31 {
		t.Errorf("%+v", got)
	}
}
