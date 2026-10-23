package sources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sloccy/morphe-tv-patcher/internal/abi"
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
		DirectOK: true}
	_ = os.MkdirAll(e.CacheDir, 0o750)
	e.Verify = func(ctx context.Context, path string, w verify.Want) (*verify.Verified, error) {
		return &verify.Verified{Path: path, Source: w.Source, Signer: "aa", Package: w.Package, Version: w.Version}, nil
	}
	e.Mirror = func(ctx context.Context, req apkmirror.AppRequest, o apkmirror.Options) (apkmirror.Result, string, error) {
		p := filepath.Join(req.DestDir, "m.apkm")
		_ = os.WriteFile(p, []byte("PK"), 0o600)
		return apkmirror.Result{Path: p, Slug: "pub/app"}, "chrome", nil
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
	_ = os.Remove(filepath.Join(r.env.CacheDir, "m.apkm"))
	f, _ = Fetch(context.Background(), r.env, app, 31)
	if f.LearnedSigner {
		t.Error("signer already on record")
	}
}

func TestFallsThroughDirectThenFlare(t *testing.T) {
	r := newRig(t)
	r.env.FlareURL = "http://flare"
	var order []string
	r.env.Mirror = func(ctx context.Context, req apkmirror.AppRequest, o apkmirror.Options) (apkmirror.Result, string, error) {
		if o.FlareURL != "" {
			order = append(order, "flare")
			p := filepath.Join(req.DestDir, "f.apkm")
			_ = os.WriteFile(p, []byte("PK"), 0o600)
			return apkmirror.Result{Path: p, Slug: "pub/app"}, "flaresolverr", nil
		}
		order = append(order, "direct")
		return apkmirror.Result{}, "", apkmirror.ErrBlocked
	}
	f, err := Fetch(context.Background(), r.env, app, 31)
	if err != nil || !strings.Contains(f.Detail, "flaresolverr") {
		t.Fatalf("%+v %v", f, err)
	}
	if strings.Join(order, ",") != "direct,flare" {
		t.Errorf("order %v", order)
	}
}

func TestSignatureMismatchIsFatalAndTheFileIsDeleted(t *testing.T) {
	r := newRig(t)
	r.env.Verify = func(ctx context.Context, path string, w verify.Want) (*verify.Verified, error) {
		return nil, &verify.Error{Kind: verify.KindSignature, Msg: "signer mismatch"}
	}
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
	_ = os.MkdirAll(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, "old.apkm"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "new.apkm"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	r.env.Verify = func(ctx context.Context, path string, w verify.Want) (*verify.Verified, error) {
		if filepath.Base(path) == "old.apkm" {
			return nil, &verify.Error{Kind: verify.KindVersion, Msg: "old"}
		}
		return &verify.Verified{Path: path, Source: w.Source, Signer: "cc"}, nil
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
	_ = os.WriteFile(filepath.Join(r.env.CacheDir, "other.apkm"), []byte("x"), 0o600)
	r.env.Verify = func(ctx context.Context, path string, w verify.Want) (*verify.Verified, error) {
		if filepath.Base(path) == "other.apkm" {
			return nil, &verify.Error{Kind: verify.KindPackage, Msg: "other app"}
		}
		return &verify.Verified{Path: path, Source: w.Source, Signer: "aa"}, nil
	}
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		return apkmirror.Result{}, "", apkmirror.ErrBlocked
	}
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
	_, _ = Fetch(context.Background(), r.env, a, 31)
	if got.KnownSlug != "me/override" || got.Hint != apkmirror.Hints["com.netflix.ninja"] || got.SDK != 31 {
		t.Errorf("%+v", got)
	}
}

// verifyByName fakes verification: the file name says which ABIs the bundle contains
// ("a64.apkm" has arm64-v8a, "a32.apkm" has armeabi-v7a, "both.apkm" has both).
func verifyByName(t *testing.T) func(ctx context.Context, path string, w verify.Want) (*verify.Verified, error) {
	t.Helper()
	return func(ctx context.Context, path string, w verify.Want) (*verify.Verified, error) {
		have := map[string]bool{}
		switch filepath.Base(path) {
		case "a64.apkm":
			have["arm64-v8a"] = true
		case "a32.apkm":
			have["armeabi-v7a"] = true
		case "both.apkm":
			have["arm64-v8a"], have["armeabi-v7a"] = true, true
		}
		rank, ok := abi.Rank(have, w.ABIs)
		if !ok {
			return nil, &verify.Error{Kind: verify.KindArch, Msg: "wrong cpu"}
		}
		return &verify.Verified{Path: path, Source: w.Source, Signer: "aa", Package: w.Package, Version: w.Version, Rank: rank}, nil
	}
}

func TestCachedBuildForAnotherCPUIsSkippedNotAnAlarm(t *testing.T) {
	r := newRig(t)
	r.env.Verify = verifyByName(t)
	_ = os.WriteFile(filepath.Join(r.env.CacheDir, "a64.apkm"), []byte("x"), 0o600)
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		return apkmirror.Result{}, "", apkmirror.ErrBlocked
	}
	a := app
	a.ABIs = []string{"armeabi-v7a", "armeabi"}
	_, err := Fetch(context.Background(), r.env, a, 31)
	if fe, ok := errors.AsType[*FetchError](err); !ok || fe.Fatal || fe.NoBuild {
		t.Errorf("a 64-bit file in the cache must be ignored for a 32-bit device without raising an alarm: %v", err)
	}
}

func TestBestCachedFileWinsAndOnlineIsTriedForBetter(t *testing.T) {
	r := newRig(t)
	r.env.Verify = verifyByName(t)
	_ = os.WriteFile(filepath.Join(r.env.CacheDir, "a32.apkm"), []byte("x"), 0o600)
	var asked []string
	r.env.Mirror = func(ctx context.Context, req apkmirror.AppRequest, o apkmirror.Options) (apkmirror.Result, string, error) {
		asked = append(asked, "mirror")
		p := filepath.Join(req.DestDir, "both.apkm")
		_ = os.WriteFile(p, []byte("x"), 0o600)
		return apkmirror.Result{Path: p, Slug: "pub/app"}, "chrome", nil
	}
	a := app
	a.ABIs = []string{"arm64-v8a", "armeabi-v7a", "armeabi"} // a 64-bit capable device
	f, err := Fetch(context.Background(), r.env, a, 31)
	if err != nil || filepath.Base(f.Path) != "both.apkm" || len(asked) != 1 {
		t.Fatalf("a 32-bit cached file is only second best for this device; the online build should win: %+v %v %v", f, err, asked)
	}
}

func TestSecondBestCachedFileIsUsedWhenNothingBetterExists(t *testing.T) {
	r := newRig(t)
	r.env.Verify = verifyByName(t)
	_ = os.WriteFile(filepath.Join(r.env.CacheDir, "a32.apkm"), []byte("x"), 0o600)
	// the site only has the same 32-bit build (the mirror hands back the cached file)
	r.env.Mirror = func(ctx context.Context, req apkmirror.AppRequest, o apkmirror.Options) (apkmirror.Result, string, error) {
		return apkmirror.Result{Path: filepath.Join(req.DestDir, "a32.apkm"), Slug: "pub/app"}, "chrome", nil
	}
	a := app
	a.ABIs = []string{"arm64-v8a", "armeabi-v7a", "armeabi"}
	f, err := Fetch(context.Background(), r.env, a, 31)
	if err != nil || filepath.Base(f.Path) != "a32.apkm" || f.Rank != 1 {
		t.Fatalf("%+v %v", f, err)
	}
	// and when the site is unreachable the cached 32-bit build still works
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		return apkmirror.Result{}, "", apkmirror.ErrBlocked
	}
	if f, err = Fetch(context.Background(), r.env, a, 31); err != nil || filepath.Base(f.Path) != "a32.apkm" {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestNoBuildForThisCPUIsReportedAsSuch(t *testing.T) {
	r := newRig(t)
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		return apkmirror.Result{}, "", errors.Join(apkmirror.ErrNoBuild, apkmirror.ErrNotFound)
	}
	a := app
	a.ABIs = []string{"arm64-v8a"}
	_, err := Fetch(context.Background(), r.env, a, 31)
	if fe, ok := errors.AsType[*FetchError](err); !ok || !fe.NoBuild || fe.Fatal {
		t.Errorf("got %v", err)
	}
}

func TestDeviceABIsAndPhonePolicyReachTheMirrorAndVerifier(t *testing.T) {
	r := newRig(t)
	var gotReq apkmirror.AppRequest
	var gotWant verify.Want
	r.env.Mirror = func(ctx context.Context, req apkmirror.AppRequest, o apkmirror.Options) (apkmirror.Result, string, error) {
		gotReq = req
		p := filepath.Join(req.DestDir, "m.apkm")
		_ = os.WriteFile(p, []byte("x"), 0o600)
		return apkmirror.Result{Path: p, Slug: "pub/app"}, "chrome", nil
	}
	r.env.Verify = func(ctx context.Context, path string, w verify.Want) (*verify.Verified, error) {
		gotWant = w
		return &verify.Verified{Path: path, Source: w.Source, Signer: "aa"}, nil
	}
	a := app
	a.ABIs, a.AllowPhone = []string{"arm64-v8a", "armeabi-v7a"}, true
	if _, err := Fetch(context.Background(), r.env, a, 31); err != nil {
		t.Fatal(err)
	}
	if len(gotReq.ABIs) != 2 || !gotReq.AllowPhone || len(gotWant.ABIs) != 2 || !gotWant.AllowPhone {
		t.Errorf("req %+v want %+v", gotReq, gotWant)
	}
}

func TestCachedFileThatIsAsGoodAsAnyBuildNeverVisitsAPKMirror(t *testing.T) {
	r := newRig(t)
	r.env.Verify = verifyByName(t)
	_ = os.WriteFile(filepath.Join(r.env.CacheDir, "a32.apkm"), []byte("x"), 0o600)
	visits := 0
	r.env.Mirror = func(context.Context, apkmirror.AppRequest, apkmirror.Options) (apkmirror.Result, string, error) {
		visits++
		return apkmirror.Result{}, "", apkmirror.ErrBlocked
	}
	a := app
	a.ABIs = []string{"arm64-v8a", "armeabi-v7a", "armeabi"} // a 64-bit device holding the 32-bit build

	// the release is known to offer only the 32-bit build: nothing better exists, so no visit
	_ = r.st.SetBuilds(a.Package, a.Version, []string{"armeabi-v7a"})
	f, err := Fetch(context.Background(), r.env, a, 31)
	if err != nil || filepath.Base(f.Path) != "a32.apkm" || visits != 0 {
		t.Fatalf("%+v %v visits=%d", f, err, visits)
	}

	// once a 64-bit build is known to exist, the cached 32-bit file is not good enough: ask the site
	_ = r.st.SetBuilds(a.Package, a.Version, []string{"armeabi-v7a", "arm64-v8a"})
	_, _ = Fetch(context.Background(), r.env, a, 31)
	if visits == 0 {
		t.Error("a better build exists, so the site should have been asked")
	}
}
