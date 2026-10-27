package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sloccy/morphe-tv-patcher/internal/apk/apktest"
)

const (
	primePkg = "com.amazon.amazonvideo.livingroom"
	primeVer = "6.23.23+v15.5.0.70-armv7a"
)

func noAPKSigner(t *testing.T) {
	t.Helper()
	old := lookPath
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPath = old })
}

func manifest(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/prime.AndroidManifest.xml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func write(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// APKMirror layout: base.apk + split_config.* + info.json.
func mirrorBundle(t *testing.T, signer []byte, libs ...string) string {
	t.Helper()
	files := map[string][]byte{"AndroidManifest.xml": manifest(t)}
	for _, l := range libs {
		files["lib/"+l+"/libx.so"] = []byte("so")
	}
	// the splits are real signed APKs, as they are in a bundle from APKMirror: every APK inside
	// carries the publisher's certificate, and Inspect checks each one.
	split := func(body string) []byte {
		return apktest.SignedAPK(map[string][]byte{"part": []byte(body)}, signer)
	}
	return write(t, "app.apkm", apktest.Zip(map[string][]byte{
		"info.json":                    []byte(`{"pname":"` + primePkg + `"}`),
		"base.apk":                     apktest.SignedAPK(files, signer),
		"split_config.xhdpi.apk":       split("s"),
		"split_config.armeabi_v7a.apk": split("native"),
	}))
}

func TestInspectAPKMirrorBundle(t *testing.T) {
	noAPKSigner(t)
	der, want := apktest.Cert("pub")
	f, err := Inspect(context.Background(), mirrorBundle(t, der))
	if err != nil {
		t.Fatal(err)
	}
	if f.Package != primePkg || f.Version != primeVer || !f.Leanback || f.Signer != want {
		t.Errorf("%+v", f)
	}
	if !f.ABIs["armeabi-v7a"] || len(f.ABIs) != 1 {
		t.Errorf("abis %v", f.ABIs)
	}
	if f.SignerOK != nil {
		t.Error("no apksigner installed: verdict must be unknown, not true")
	}
}

// APKPure layout: <package>.apk is the base; the biggest file is an ABI split named config.*.apk.
func TestInspectAPKPureXAPKPicksRightBase(t *testing.T) {
	noAPKSigner(t)
	der, want := apktest.Cert("pub")
	big := make([]byte, 200_000)
	p := write(t, "x.xapk", apktest.Zip(map[string][]byte{
		"manifest.json":          []byte(`{"package_name":"` + primePkg + `"}`),
		primePkg + ".apk":        apktest.SignedAPK(map[string][]byte{"AndroidManifest.xml": manifest(t)}, der),
		"config.armeabi_v7a.apk": apktest.SignedAPK(map[string][]byte{"part": big}, der),
		"config.en.apk":          apktest.SignedAPK(map[string][]byte{"part": []byte("e")}, der),
	}))
	f, err := Inspect(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Package != primePkg || f.Signer != want || !f.ABIs["armeabi-v7a"] {
		t.Errorf("%+v", f)
	}
}

func TestInspectPlainAPKWhateverItIsCalled(t *testing.T) {
	noAPKSigner(t)
	der, want := apktest.Cert("pub")
	apkBytes := apktest.SignedAPK(map[string][]byte{"AndroidManifest.xml": manifest(t), "lib/armeabi-v7a/x.so": []byte("so")}, der)
	for _, name := range []string{"single.apk", "single.apkm", "single.bin"} {
		f, err := Inspect(context.Background(), write(t, name, apkBytes))
		if err != nil || f.Package != primePkg || f.Signer != want || !f.ABIs["armeabi-v7a"] {
			t.Errorf("%s: %+v %v", name, f, err)
		}
	}
}

func TestInspectRejectsBadFiles(t *testing.T) {
	noAPKSigner(t)
	var e *Error
	if _, err := Inspect(context.Background(), write(t, "a.apkm", []byte("not a zip"))); !errors.As(err, &e) || e.Kind != KindFormat {
		t.Errorf("got %v", err)
	}
	noBase := write(t, "b.apkm", apktest.Zip(map[string][]byte{"info.json": []byte("{}"), "split_config.en.apk": []byte("x")}))
	if _, err := Inspect(context.Background(), noBase); !errors.As(err, &e) || e.Kind != KindFormat {
		t.Errorf("got %v", err)
	}
	badManifest := write(t, "c.apkm", apktest.Zip(map[string][]byte{
		"base.apk": apktest.Zip(map[string][]byte{"AndroidManifest.xml": []byte("junk")})}))
	if _, err := Inspect(context.Background(), badManifest); !errors.As(err, &e) || e.Kind != KindFormat {
		t.Errorf("got %v", err)
	}
}

// The rules are about the file, so these cases allow a missing apksigner; the gate for an
// unverifiable file has its own case at the end of TestCheckRules.
func want(source, pinned string) Want {
	return Want{Package: "com.x", Version: "1.0", Source: source, Pinned: pinned, AllowUnverified: true}
}

func facts() *Facts {
	return &Facts{Package: "com.x", Version: "1.0", Leanback: true, ABIs: map[string]bool{"armeabi-v7a": true}, Signer: "aa"}
}

func TestCheckRules(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name   string
		mod    func(*Facts)
		source string
		pinned string
		kind   Kind
		fatal  bool
	}{
		{"ok", func(*Facts) {}, "apkmirror", "", "", false},
		{"ok pinned from apkpure", func(*Facts) {}, "apkpure", "aa", "", false},
		{"ok no native code", func(f *Facts) { f.ABIs = nil }, "apkmirror", "", "", false},
		{"wrong package", func(f *Facts) { f.Package = "com.y" }, "apkmirror", "", KindPackage, true},
		{"old version", func(f *Facts) { f.Version = "0.9" }, "apkmirror", "", KindVersion, false},
		{"phone build", func(f *Facts) { f.Leanback = false }, "apkmirror", "", KindLeanback, true},
		{"arm64 only", func(f *Facts) { f.ABIs = map[string]bool{"arm64-v8a": true} }, "apkmirror", "", KindArch, false},
		{"no signer", func(f *Facts) { f.Signer = "" }, "apkmirror", "", KindSignature, true},
		{"apksigner fails", func(f *Facts) { f.SignerOK = &no }, "apkmirror", "", KindSignature, true},
		{"apksigner ok", func(f *Facts) { f.SignerOK = &yes }, "apkmirror", "", "", false},
		{"pin mismatch", func(*Facts) {}, "apkmirror", "bb", KindSignature, true},
		{"apkpure unpinned", func(*Facts) {}, "apkpure", "", KindUnpinned, false},
		{"dropfolder unpinned ok", func(*Facts) {}, "dropfolder", "", "", false},
	}
	for _, c := range cases {
		f := facts()
		c.mod(f)
		err := Check(f, want(c.source, c.pinned))
		if c.kind == "" {
			if err != nil {
				t.Errorf("%s: unexpected %v", c.name, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Kind != c.kind || e.Fatal() != c.fatal {
			t.Errorf("%s: got %v (kind %v fatal %v), want %v fatal %v", c.name, err, e, e != nil && e.Fatal(), c.kind, c.fatal)
		}
	}

	// Without apksigner nothing is checked cryptographically, so the file is refused unless the
	// operator opted in with ALLOW_UNVERIFIED_SIGNATURES.
	strict := want("apkmirror", "")
	strict.AllowUnverified = false
	var e *Error
	if err := Check(facts(), strict); !errors.As(err, &e) || e.Kind != KindUnverifiable || !e.Fatal() {
		t.Errorf("an unverifiable file must be refused, got %v", err)
	}
	if err := Check(facts(), want("apkmirror", "")); err != nil {
		t.Errorf("with AllowUnverified it goes through: %v", err)
	}
}

func TestInspectRejectsSplitsItCannotVouchFor(t *testing.T) {
	noAPKSigner(t)
	der, _ := apktest.Cert("pub")
	other, _ := apktest.Cert("attacker")
	base := apktest.SignedAPK(map[string][]byte{"AndroidManifest.xml": manifest(t)}, der)
	w := Want{Package: primePkg, Version: primeVer, Source: "apkmirror", AllowUnverified: true}

	for _, c := range []struct {
		name  string
		split []byte
	}{
		{"signed by another key", apktest.SignedAPK(map[string][]byte{"lib/armeabi-v7a/x.so": []byte("evil")}, other)},
		{"not signed at all", apktest.Zip(map[string][]byte{"lib/armeabi-v7a/x.so": []byte("evil")})},
	} {
		p := write(t, "evil.apkm", apktest.Zip(map[string][]byte{
			"base.apk":                     base,
			"split_config.armeabi_v7a.apk": c.split,
		}))
		f, err := Inspect(context.Background(), p)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// Morphe merges the splits into the patched app, so the bundle must be refused even though
		// the base is the genuine, correctly signed file.
		var e *Error
		if err := Check(f, w); !errors.As(err, &e) || e.Kind != KindSignature || !e.Fatal() {
			t.Errorf("a split %s must condemn the bundle, got %v", c.name, err)
		}
	}
}

func TestInspectRejectsDuplicateEntries(t *testing.T) {
	noAPKSigner(t)
	der, _ := apktest.Cert("pub")
	good := apktest.SignedAPK(map[string][]byte{"AndroidManifest.xml": manifest(t)}, der)
	evil := apktest.SignedAPK(map[string][]byte{"AndroidManifest.xml": manifest(t), "classes.dex": []byte("payload")}, der)
	// Go reads the first entry of a duplicate pair, Java the last: one copy would be verified and
	// the other patched, so the bundle is refused outright.
	p := write(t, "dup.apkm", apktest.ZipEntries(
		apktest.Entry{Name: "base.apk", Data: good},
		apktest.Entry{Name: "base.apk", Data: evil},
	))
	var e *Error
	if _, err := Inspect(context.Background(), p); !errors.As(err, &e) || e.Kind != KindFormat {
		t.Errorf("got %v", err)
	}
}

func TestAPKSignerVerdictAndV1Fallback(t *testing.T) {
	lookPath = func(string) (string, error) { return "/bin/apksigner", nil }
	t.Cleanup(func() { lookPath = exec.LookPath })
	old := execAPKSigner
	t.Cleanup(func() { execAPKSigner = old })

	// verification fails
	execAPKSigner = func(context.Context, string, string) ([]byte, error) {
		return []byte("DOES NOT VERIFY"), errors.New("exit 1")
	}
	der, _ := apktest.Cert("pub")
	f, err := Inspect(context.Background(), mirrorBundle(t, der))
	if err != nil || f.SignerOK == nil || *f.SignerOK {
		t.Fatalf("expected a false verdict: %+v %v", f, err)
	}
	if err := Check(f, Want{Package: primePkg, Version: primeVer, Source: "apkmirror"}); err == nil || !strings.Contains(err.Error(), "DOES NOT VERIFY") {
		t.Errorf("got %v", err)
	}

	// a v1-only APK has no signing block: the certificate comes from apksigner's output
	execAPKSigner = func(context.Context, string, string) ([]byte, error) {
		return []byte("Signer #1 certificate SHA-256 digest: " + strings.Repeat("AB", 32) + "\n"), nil
	}
	f, err = Inspect(context.Background(), mirrorBundle(t, nil))
	if err != nil || f.Signer != strings.Repeat("ab", 32) || f.SignerOK == nil || !*f.SignerOK {
		t.Errorf("v1 fallback: %+v %v", f, err)
	}
}

// Real downloaded bundles, when APK_REAL_DIR (folder of *.apkm) is set.
func TestInspectRealBundles(t *testing.T) {
	dir := os.Getenv("APK_REAL_BUNDLES")
	if dir == "" {
		t.Skip("set APK_REAL_BUNDLES to a folder of *.apkm files")
	}
	want := map[string][2]string{
		"com.amazon.amazonvideo.livingroom": {"6.23.23+v15.5.0.70-armv7a", "2f19adeb284eb36f7f07786152b9a1d14b21653203ad0b04ebbf9c73ab6d7625"},
		"com.netflix.ninja":                 {"13.0.1 build 25028", "363863596ea99241eb71b1a985553aa604de3ea3c5f0c546742390e682164e6b"},
		"com.disney.disneyplus":             {"26.16.0+rc2-2026.09.08", "b4d251e979ea974d8f070feb5ab69fc4c2873f4e8add421ff63ad5f2789d63d4"},
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.apkm"))
	if len(files) == 0 {
		t.Skip("no bundles")
	}
	for _, p := range files {
		f, err := Inspect(context.Background(), p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		w := want[f.Package]
		if f.Version != w[0] || f.Signer != w[1] || !f.Leanback || !f.ABIs["armeabi-v7a"] {
			t.Errorf("%s: %+v", p, f)
		}
		// AllowUnverified so this still runs on a machine without the Android SDK; with apksigner
		// installed it also proves the real splits carry the base's certificate.
		if err := Check(f, Want{Package: f.Package, Version: w[0], Source: "apkmirror", Pinned: w[1], AllowUnverified: true}); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestCheckPhoneBuildsAndDeviceCPUs(t *testing.T) {
	phone := facts()
	phone.Leanback = false
	w := want("apkmirror", "")
	var e *Error
	if err := Check(phone, w); !errors.As(err, &e) || e.Kind != KindLeanback {
		t.Errorf("a phone build must be refused by default, got %v", err)
	}
	w.AllowPhone = true
	if err := Check(phone, w); err != nil {
		t.Errorf("AllowPhone should accept it: %v", err)
	}

	cpu := []struct {
		name   string
		have   []string
		device []string
		ok     bool
	}{
		{"combined build on a 32-bit device", []string{"arm64-v8a", "armeabi-v7a"}, []string{"armeabi-v7a", "armeabi"}, true},
		{"32-bit build on a 64-bit-capable device", []string{"armeabi-v7a"}, []string{"arm64-v8a", "armeabi-v7a", "armeabi"}, true},
		{"32-bit build on an arm64-only device", []string{"armeabi-v7a"}, []string{"arm64-v8a"}, false},
		{"64-bit build on a 32-bit device", []string{"arm64-v8a"}, []string{"armeabi-v7a", "armeabi"}, false},
	}
	for _, c := range cpu {
		f := facts()
		f.ABIs = map[string]bool{}
		for _, a := range c.have {
			f.ABIs[a] = true
		}
		w := want("apkmirror", "")
		w.ABIs = c.device
		err := Check(f, w)
		if (err == nil) != c.ok {
			t.Errorf("%s: got %v", c.name, err)
		}
		if err != nil && (!errors.As(err, &e) || e.Kind != KindArch || e.Fatal()) {
			t.Errorf("%s: want a non-fatal arch error, got %v", c.name, err)
		}
	}
}

func TestVerifyReportsRank(t *testing.T) {
	noAPKSigner(t)
	der, _ := apktest.Cert("pub")
	p := write(t, "r.apkm", apktest.Zip(map[string][]byte{
		"base.apk":                     apktest.SignedAPK(map[string][]byte{"AndroidManifest.xml": manifest(t)}, der),
		"split_config.armeabi_v7a.apk": apktest.SignedAPK(map[string][]byte{"part": []byte("n")}, der),
	}))
	w := Want{Package: primePkg, Version: primeVer, Source: "apkmirror", AllowUnverified: true,
		ABIs: []string{"arm64-v8a", "armeabi-v7a", "armeabi"}}
	v, err := Verify(context.Background(), p, w)
	if err != nil || v.Rank != 1 {
		t.Errorf("a 32-bit build on a 64-bit-capable device is rank 1, got %+v %v", v, err)
	}
	w.ABIs = []string{"armeabi-v7a"}
	if v, _ = Verify(context.Background(), p, w); v == nil || v.Rank != 0 {
		t.Errorf("on a 32-bit device it is rank 0, got %+v", v)
	}
}
