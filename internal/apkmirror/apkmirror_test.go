package apkmirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/apk/apktest"
)

const (
	pkg      = "com.amazon.amazonvideo.livingroom"
	version  = "6.23.23+v15.5.0.70-armv7a"
	slug     = "amazon-mobile-llc/prime-video-android-tv-android-tv"
	relPath  = "/apk/" + slug + "/prime-video-android-tv-6-23-23v15-5-0-70-armv7a-release/"
	zipBytes = "PK\x03\x04fake-apkm-contents"
)

func fx(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "apkmirror-"+name+".html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func noSleep(context.Context, time.Duration, time.Duration) {}

func page(t *testing.T, raw string) *Page {
	t.Helper()
	p, err := parsePage("x", raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// mirror serves the real saved pages for the Prime Video chain. The guessed release URL 404s
// (as it does on the real site) so the app-page lookup is exercised too.
func mirror(t *testing.T, gate func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	release, variant, dl := fx(t, "release"), fx(t, "variant"), fx(t, "download")
	vhref, err := PickVariant(page(t, release), version, 31)
	if err != nil {
		t.Fatal(err)
	}
	btn, err := downloadButton(page(t, variant))
	if err != nil {
		t.Fatal(err)
	}
	fl, err := fileLink(page(t, dl))
	if err != nil {
		t.Fatal(err)
	}
	flPath := strings.SplitN(fl, "?", 2)[0]
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if gate != nil && gate(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/":
			fmt.Fprint(w, "<html><title>APKMirror</title></html>")
		case r.URL.Path == "/apk/"+slug+"/":
			fmt.Fprintf(w, `<html><title>app</title><a href="%s">6.23.23</a><a href="/apk/%s/x-6-24-8-release/">new</a></html>`, relPath, slug)
		case r.URL.Path == relPath:
			fmt.Fprint(w, release)
		case r.URL.Path == vhref:
			fmt.Fprint(w, variant)
		case r.URL.Path == strings.SplitN(btn, "?", 2)[0]:
			fmt.Fprint(w, dl)
		case r.URL.Path == flPath:
			if r.Header.Get("Referer") == "" {
				http.Error(w, "no referer", 403)
				return
			}
			fmt.Fprint(w, zipBytes)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func req(t *testing.T) AppRequest {
	return AppRequest{Package: pkg, Name: "Prime Video Android TV", Version: version, SDK: 31, DestDir: t.TempDir()}
}

func TestFullChainViaAppPageLookup(t *testing.T) {
	srv := mirror(t, nil)
	c := &Client{F: NewDirect(Profiles[0]), Base: srv.URL, Sleep: noSleep}
	r := req(t)
	r.Hint = slug
	res, err := c.Fetch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	if string(b) != zipBytes || res.Slug != slug {
		t.Errorf("got %q slug %q", b, res.Slug)
	}
}

func TestRotatesProfilesWhenBlocked(t *testing.T) {
	var chromeHits atomic.Int32
	srv := mirror(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.UserAgent(), "Chrome/") {
			chromeHits.Add(1)
			w.WriteHeader(403)
			fmt.Fprint(w, "<title>Just a moment...</title>")
			return true
		}
		return false
	})
	r := req(t)
	r.Hint = slug
	res, used, err := Download(context.Background(), r, Options{Base: srv.URL, Backoff: []time.Duration{time.Millisecond}, Sleep: noSleep})
	if err != nil || used != "firefox" || res.Path == "" {
		t.Fatalf("used %q err %v", used, err)
	}
	if chromeHits.Load() == 0 {
		t.Error("chrome should have been tried first")
	}
	// the profile that worked goes first next time
	chromeHits.Store(0)
	r.DestDir = t.TempDir()
	if _, used, err = Download(context.Background(), r, Options{Base: srv.URL, ProfileHint: "firefox", Sleep: noSleep}); err != nil || used != "firefox" {
		t.Fatalf("used %q err %v", used, err)
	}
	if chromeHits.Load() != 0 {
		t.Error("profile hint ignored")
	}
}

func TestAllProfilesBlocked(t *testing.T) {
	srv := mirror(t, func(w http.ResponseWriter, r *http.Request) bool { w.WriteHeader(429); return true })
	r := req(t)
	r.Hint = slug
	_, _, err := Download(context.Background(), r, Options{Base: srv.URL, Backoff: []time.Duration{time.Millisecond}, Sleep: noSleep})
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("got %v", err)
	}
}

func TestNotFoundServedAs200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><head><title>Page Not Found - APKMirror</title></head></html>")
	}))
	defer srv.Close()
	_, err := NewDirect(Profiles[0]).Page(context.Background(), srv.URL+"/x", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v", err)
	}
}

func TestDownloadMustBeAZip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>captcha</html>") }))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "x.apkm")
	err := NewDirect(Profiles[0]).File(context.Background(), srv.URL, "", dest)
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("got %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("non-zip must not be kept")
	}
}

func TestCandidatesRankAndroidTVFirstAndDropNoise(t *testing.T) {
	body := `<a href="/apk/pluto-inc/pluto-tv/">p</a><a href="/apk/pluto-inc/pluto-tv-android-tv/">tv</a>` +
		`<a href="/apk/bytedance-pte-ltd/capcut/">c</a><a href="/apk/google-inc/youtube/">y</a>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<title>s</title>"+body) }))
	defer srv.Close()
	cl := &Client{F: NewDirect(Profiles[0]), Base: srv.URL, Sleep: noSleep}
	got, err := cl.Candidates(context.Background(), "tv.pluto.android", "Pluto TV Android TV")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pluto-inc/pluto-tv-android-tv", "pluto-inc/pluto-tv"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates %v, want %v (Android TV page first, sidebar noise dropped)", got, want)
	}
}

func TestFallsBackToSearchWhenHintIsWrongApp(t *testing.T) {
	release, variant, dl := fx(t, "release"), fx(t, "variant"), fx(t, "download")
	vhref, _ := PickVariant(page(t, release), version, 31)
	btn, _ := downloadButton(page(t, variant))
	fl, _ := fileLink(page(t, dl))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("s") == pkg:
			fmt.Fprintf(w, `<title>s</title><a href="/apk/%s/">tv</a>`, slug)
		case r.URL.Path == "/":
			fmt.Fprint(w, "<title>x</title>")
		case r.URL.Path == "/apk/wrong/prime-video-phone/prime-video-phone-"+"6-23-23v15-5-0-70-armv7a-release/":
			fmt.Fprint(w, "<title>phone</title>nothing here")
		case r.URL.Path == "/apk/"+slug+"/prime-video-android-tv-android-tv-6-23-23v15-5-0-70-armv7a-release/":
			fmt.Fprint(w, release)
		case r.URL.Path == vhref:
			fmt.Fprint(w, variant)
		case r.URL.Path == strings.SplitN(btn, "?", 2)[0]:
			fmt.Fprint(w, dl)
		case r.URL.Path == strings.SplitN(fl, "?", 2)[0]:
			fmt.Fprint(w, zipBytes)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{F: NewDirect(Profiles[0]), Base: srv.URL, Sleep: noSleep}
	r := req(t)
	r.Hint = "wrong/prime-video-phone"
	res, err := c.Fetch(context.Background(), r)
	if err != nil || res.Slug != slug {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestPickVariantRanking(t *testing.T) {
	row := func(href, cells string) string {
		return fmt.Sprintf(`<div class="table-row headerFont"><div class="table-cell"><a class="accent_color" href="%s">v</a> %s</div></div>`, href, cells)
	}
	cases := []struct {
		name, rows, ver string
		sdk             int
		want            string
	}{
		{"both archs beat armv7-only", row("/a", "BUNDLE armeabi-v7a Android 7.0+") + row("/b", "BUNDLE arm64-v8a + armeabi-v7a Android 7.0+"), "1.0", 31, "/b"},
		{"plain APK rows are fine", row("/a", "APK armeabi-v7a Android 5.0+"), "1.0", 31, "/a"},
		{"better arch beats type", row("/a", "APK armeabi-v7a Android 5.0+") + row("/b", "BUNDLE arm64-v8a + armeabi-v7a Android 5.0+"), "1.0", 31, "/b"},
		{"rows that are neither are ignored", row("/a", "Download armeabi-v7a Android 5.0+"), "1.0", 31, ""},
		{"arm64 only is unusable", row("/a", "BUNDLE arm64-v8a Android 7.0+"), "1.0", 31, ""},
		{"too new for the device", row("/a", "BUNDLE armeabi-v7a Android 12L+") + row("/b", "BUNDLE armeabi-v7a Android 7.0+"), "1.0", 31, "/b"},
		{"12L fits an sdk 32 device", row("/a", "BUNDLE armeabi-v7a Android 12L+"), "1.0", 32, "/a"},
		{"unknown sdk does not filter", row("/a", "BUNDLE armeabi-v7a Android 13+"), "1.0", 0, "/a"},
		{"exact version row wins", row("/a", "BUNDLE 5.66.0-mobile arm64-v8a + armeabi-v7a Android 7.0+") + row("/b", "BUNDLE 5.66.0-leanback armeabi-v7a Android 7.0+"), "5.66.0-leanback", 31, "/b"},
		{"universal counts", row("/a", "BUNDLE universal Android 7.0+"), "1.0", 31, "/a"},
	}
	for _, c := range cases {
		got, err := PickVariant(page(t, "<html>"+c.rows+"</html>"), c.ver, c.sdk)
		if c.want == "" {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: got %q %v", c.name, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q %v, want %q", c.name, got, err, c.want)
		}
	}
	// the real saved release page
	href, err := PickVariant(page(t, fx(t, "release")), version, 31)
	if err != nil || !strings.HasSuffix(href, "-android-apk-download/") {
		t.Errorf("real page: %q %v", href, err)
	}
}

func TestAPILevel(t *testing.T) {
	for _, c := range []struct {
		major int
		l     bool
		want  int
	}{{5, false, 21}, {7, false, 24}, {12, false, 31}, {12, true, 32}, {13, false, 33}} {
		if got := APILevel(c.major, c.l); got != c.want {
			t.Errorf("APILevel(%d,%v) = %d", c.major, c.l, got)
		}
	}
}

func TestReleaseLookupVariants(t *testing.T) {
	appPage := func(hrefs ...string) string {
		s := "<title>app</title>"
		for _, h := range hrefs {
			s += fmt.Sprintf(`<a href="%s">r</a>`, h)
		}
		return s
	}
	cases := []struct{ name, slug, version, href string }{
		{"apk in url (Peacock)", "p/peacock", "7.8.100", "/apk/p/peacock/peacock-tv-stream-7-8-100-apk-release/"},
		{"underscore kept (ViX)", "u/vix", "4.47.2_tv", "/apk/u/vix/vix-tv-4-47-2_tv-release/"},
		{"numeric core (Pluto leanback)", "pl/pluto", "5.66.0-leanback", "/apk/pl/pluto/plutotv-5-66-0-release/"},
	}
	for _, c := range cases {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/apk/" + c.slug + "/":
				fmt.Fprint(w, appPage(c.href, "/apk/"+c.slug+"/other-1-0-release/"))
			case c.href:
				fmt.Fprint(w, "<title>release</title>"+c.slug)
			default:
				http.NotFound(w, r)
			}
		})
		srv := httptest.NewServer(mux)
		cl := &Client{F: NewDirect(Profiles[0]), Base: srv.URL, Sleep: noSleep}
		u, p, err := cl.FindRelease(context.Background(), c.slug, c.version)
		srv.Close()
		if err != nil || !strings.HasSuffix(u, c.href) || p == nil {
			t.Errorf("%s: %q %v", c.name, u, err)
		}
	}
}

func TestWholeWordMatching(t *testing.T) {
	foxOne := tokens("com.fox.foxone", "Fox One Android TV")
	if matches("mozilla/firefox-fenix", foxOne) {
		t.Error(`"fox" must not match "firefox"`)
	}
	if !matches("fox-corp/fox-one-android-tv", foxOne) {
		t.Error("the real Fox One page should match")
	}
	f1 := tokens("com.formulaone.production", "F1 TV Android TV")
	if !matches("formula-one-digital-media-limited/f1-tv-android-tv", f1) || matches("google-inc/youtube", f1) {
		t.Error("F1 TV matching wrong")
	}
}

func TestPlainAPKIsRenamedAndBundlesAreNot(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, files map[string][]byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, apktest.Zip(files), 0o644)
		return p
	}
	plain := write("a.apkm", map[string][]byte{"AndroidManifest.xml": []byte("x")})
	bundle := write("b.apkm", map[string][]byte{"base.apk": []byte("x"), "info.json": []byte("{}")})
	if got := fixExtension(plain); !strings.HasSuffix(got, "a.apk") {
		t.Errorf("plain apk kept %s", got)
	}
	if got := fixExtension(bundle); !strings.HasSuffix(got, "b.apkm") {
		t.Errorf("bundle renamed to %s", got)
	}
}

func TestTokens(t *testing.T) {
	got := strings.Join(tokens("com.formulaone.production", "F1 TV Android TV"), ",")
	if got != "f1,formulaone" {
		t.Errorf("tokens = %q", got)
	}
}

func TestFlareHandsCookiesToTheFileDownload(t *testing.T) {
	var sawCookie, sawUA atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("cf_clearance")
		if c != nil && c.Value == "solved" {
			sawCookie.Store(true)
		}
		if r.UserAgent() == "Sidecar-Chrome/152" {
			sawUA.Store(true)
		}
		if c == nil {
			w.WriteHeader(403)
			fmt.Fprint(w, "<title>Just a moment...</title>")
			return
		}
		fmt.Fprint(w, zipBytes)
	}))
	defer target.Close()
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ URL string }
		json.NewDecoder(r.Body).Decode(&in)
		fmt.Fprint(w, `{"status":"ok","solution":{"response":"<html><title>ok</title></html>","status":200,"userAgent":"Sidecar-Chrome/152","cookies":[{"name":"cf_clearance","value":"solved"}]}}`)
	}))
	defer sidecar.Close()

	f := NewFlare(sidecar.URL)
	p, err := f.Page(context.Background(), target.URL+"/page", "")
	if err != nil || p.Title != "ok" {
		t.Fatalf("page: %v %v", p, err)
	}
	dest := filepath.Join(t.TempDir(), "f.apkm")
	if err := f.File(context.Background(), target.URL+"/download.php", target.URL, dest); err != nil {
		t.Fatal(err)
	}
	if !sawCookie.Load() || !sawUA.Load() {
		t.Errorf("cookie %v, user agent %v: both must be reused for the file", sawCookie.Load(), sawUA.Load())
	}
}

func TestFlareUnavailableIsBlocked(t *testing.T) {
	f := NewFlare("http://127.0.0.1:1")
	if _, err := f.Page(context.Background(), "http://x/", ""); !errors.Is(err, ErrBlocked) {
		t.Errorf("got %v", err)
	}
}

// scripted serves saved pages by URL path and records every request.
type scripted struct {
	pages map[string]string
	seen  []string
	refs  map[string]string
}

func (f *scripted) Page(ctx context.Context, u, ref string) (*Page, error) {
	f.seen = append(f.seen, u)
	if f.refs == nil {
		f.refs = map[string]string{}
	}
	f.refs[u] = ref
	raw, ok := f.pages[u]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, u)
	}
	return parsePage(u, raw)
}

func (f *scripted) File(ctx context.Context, u, ref, dest string) error {
	f.seen = append(f.seen, "FILE "+u)
	return os.WriteFile(dest, []byte(zipBytes), 0o644)
}

func TestSingleVariantReleaseUsesTheDownloadButtonOnThePage(t *testing.T) {
	single := fx(t, "single-release")
	if hasVariantTable(page(t, single)) {
		t.Fatal("fixture should be a release page without a variants table")
	}
	btn, err := downloadButton(page(t, single))
	if err != nil || !strings.Contains(btn, "/download/?key=") {
		t.Fatalf("%q %v", btn, err)
	}
	rel := "https://m.test/apk/future-today-inc/fawesome-movies-tv-shows-android-tv/fawesome-movies-tv-shows-android-tv-19-7-release/"
	f := &scripted{pages: map[string]string{
		"https://m.test/":      "<title>home</title>",
		rel:                    single,
		"https://m.test" + btn: fx(t, "download"),
	}}
	c := &Client{F: f, Base: "https://m.test", Sleep: noSleep}
	// the release page names the package, so the page check passes
	f.pages[rel] = single + " com.future.moviesByFawesomeAndroidTV"
	res, err := c.Fetch(context.Background(), AppRequest{Package: "com.future.moviesByFawesomeAndroidTV", Name: "Fawesome", Version: "19.7",
		Hint: "future-today-inc/fawesome-movies-tv-shows-android-tv", SDK: 31, DestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if f.refs["https://m.test"+btn] != rel {
		t.Errorf("download page referer %q, want the release page", f.refs["https://m.test"+btn])
	}
	if b, _ := os.ReadFile(res.Path); string(b) != zipBytes {
		t.Errorf("file %q", b)
	}
}

func TestUnusableVariantTableIsNotRescuedByTheButton(t *testing.T) {
	row := `<div class="table-row headerFont"><a class="accent_color" href="/v">v</a> BUNDLE arm64-v8a Android 7.0+</div>`
	if _, err := PickVariant(page(t, "<html>"+row+`<a class="downloadButton" href="/x/download/?key=1">d</a></html>`), "1", 31); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v", err)
	}
	if !hasVariantTable(page(t, "<html>"+row+"</html>")) {
		t.Error("a table of unusable rows still counts as a table")
	}
}
