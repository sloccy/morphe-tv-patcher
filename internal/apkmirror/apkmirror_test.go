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
	b, err := os.ReadFile(filepath.Join("testdata", "apkmirror-"+name+".html")) //nolint:gosec // fixed test data directory
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
	vhref, err := PickVariant(page(t, release), version, 31, nil)
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
	flPath, _, _ := strings.Cut(fl, "?")
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if gate != nil && gate(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/":
			_, _ = fmt.Fprint(w, "<html><title>APKMirror</title></html>")
		case r.URL.Path == "/apk/"+slug+"/":
			_, _ = fmt.Fprintf(w, `<html><title>app</title><a href="%s">6.23.23</a><a href="/apk/%s/x-6-24-8-release/">new</a></html>`, relPath, slug)
		case r.URL.Path == relPath:
			_, _ = fmt.Fprint(w, release)
		case r.URL.Path == vhref:
			_, _ = fmt.Fprint(w, variant)
		case r.URL.Path == strings.SplitN(btn, "?", 2)[0]:
			_, _ = fmt.Fprint(w, dl)
		case r.URL.Path == flPath:
			if r.Header.Get("Referer") == "" {
				http.Error(w, "no referer", http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, zipBytes)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func req(t *testing.T) AppRequest {
	t.Helper()
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
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, "<title>Just a moment...</title>")
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
	srv := mirror(t, func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	r := req(t)
	r.Hint = slug
	_, _, err := Download(context.Background(), r, Options{Base: srv.URL, Backoff: []time.Duration{time.Millisecond}, Sleep: noSleep})
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("got %v", err)
	}
}

func TestNotFoundServedAs200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "<html><head><title>Page Not Found - APKMirror</title></head></html>")
	}))
	defer srv.Close()
	_, err := NewDirect(Profiles[0]).Page(context.Background(), srv.URL+"/x", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v", err)
	}
}

func TestDownloadMustBeAZip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, "<html>captcha</html>") }))
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, "<title>s</title>"+body) }))
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
	vhref, _ := PickVariant(page(t, release), version, 31, nil)
	btn, _ := downloadButton(page(t, variant))
	fl, _ := fileLink(page(t, dl))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("s") == pkg:
			_, _ = fmt.Fprintf(w, `<title>s</title><a href="/apk/%s/">tv</a>`, slug)
		case r.URL.Path == "/":
			_, _ = fmt.Fprint(w, "<title>x</title>")
		case r.URL.Path == "/apk/wrong/prime-video-phone/prime-video-phone-"+"6-23-23v15-5-0-70-armv7a-release/":
			_, _ = fmt.Fprint(w, "<title>phone</title>nothing here")
		case r.URL.Path == "/apk/"+slug+"/prime-video-android-tv-android-tv-6-23-23v15-5-0-70-armv7a-release/":
			_, _ = fmt.Fprint(w, release)
		case r.URL.Path == vhref:
			_, _ = fmt.Fprint(w, variant)
		case r.URL.Path == strings.SplitN(btn, "?", 2)[0]:
			_, _ = fmt.Fprint(w, dl)
		case r.URL.Path == strings.SplitN(fl, "?", 2)[0]:
			_, _ = fmt.Fprint(w, zipBytes)
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
		return fmt.Sprintf(`<div class="table-row headerFont"><div class="table-cell"><a class="accent_color" href=%q>v</a> %s</div></div>`, href, cells)
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
		got, err := PickVariant(page(t, "<html>"+c.rows+"</html>"), c.ver, c.sdk, nil)
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
	href, err := PickVariant(page(t, fx(t, "release")), version, 31, nil)
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
		var b strings.Builder
		b.WriteString("<title>app</title>")
		for _, h := range hrefs {
			fmt.Fprintf(&b, `<a href=%q>r</a>`, h)
		}
		return b.String()
	}
	cases := []struct{ name, slug, version, href string }{
		{"apk in url (Peacock)", "p/peacock", "7.8.100", "/apk/p/peacock/peacock-tv-stream-7-8-100-apk-release/"},
		{"underscore kept (ViX)", "u/vix", "4.47.2_tv", "/apk/u/vix/vix-tv-4-47-2_tv-release/"},
		{"numeric core (Pluto leanback)", "pl/pluto", "5.66.0-leanback", "/apk/pl/pluto/plutotv-5-66-0-release/"},
		{"re-upload counter (Paramount+)", "c/paramount-2", "16.19.0", "/apk/c/paramount-2/paramount-android-tv-16-19-0-2-release/"},
	}
	for _, c := range cases {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/apk/" + c.slug + "/":
				_, _ = fmt.Fprint(w, appPage(c.href, "/apk/"+c.slug+"/other-1-0-release/"))
			case c.href:
				_, _ = fmt.Fprint(w, "<title>release</title>"+c.slug)
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
		_ = os.WriteFile(p, apktest.Zip(files), 0o600)
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
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, "<title>Just a moment...</title>")
			return
		}
		_, _ = fmt.Fprint(w, zipBytes)
	}))
	defer target.Close()
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		_, _ = fmt.Fprint(w, `{"status":"ok","solution":{"response":"<html><title>ok</title></html>","status":200,"userAgent":"Sidecar-Chrome/152","cookies":[{"name":"cf_clearance","value":"solved"}]}}`)
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
	return os.WriteFile(dest, []byte(zipBytes), 0o600)
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
	if _, err := PickVariant(page(t, "<html>"+row+`<a class="downloadButton" href="/x/download/?key=1">d</a></html>`), "1", 31, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v", err)
	}
	if !hasVariantTable(page(t, "<html>"+row+"</html>")) {
		t.Error("a table of unusable rows still counts as a table")
	}
}

func TestPickReleasePrefersExactOverCounterSuffix(t *testing.T) {
	links := []string{"/apk/p/a/x-16-19-0-2-release/", "/apk/p/a/x-16-19-0-release/", "/apk/p/a/x-16-20-0-release/"}
	if got := pickRelease(links, []string{"16-19-0"}); got != "/apk/p/a/x-16-19-0-release/" {
		t.Errorf("got %q", got)
	}
	if got := pickRelease(links[:1], []string{"16-19-0"}); got != links[0] {
		t.Errorf("suffixed link should be used when it is the only one: %q", got)
	}
	if got := pickRelease(links, []string{"9-9-9"}); got != "" {
		t.Errorf("got %q", got)
	}
}

// realRows builds a release page from the variant rows harvested from live APKMirror pages
// (testdata/arch-rows.tsv: "app<TAB>row text"). Row i links to /v/<app>/<i>.
func realRows(t *testing.T, app string) *Page {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "arch-rows.tsv")) //nolint:gosec // fixed test data
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("<html><title>" + app + "</title>")
	i := 0
	for line := range strings.SplitSeq(string(b), "\n") {
		name, row, ok := strings.Cut(line, "\t")
		if !ok || name != app {
			continue
		}
		fmt.Fprintf(&sb, `<div class="table-row headerFont"><a class="accent_color" href="/v/%s/%d">v</a> %s</div>`, app, i, row)
		i++
	}
	sb.WriteString("</html>")
	return page(t, sb.String())
}

func TestPickVariantOnRealRowsForEachKindOfDevice(t *testing.T) {
	var (
		armv7  = []string{"armeabi-v7a", "armeabi"}
		both   = []string{"arm64-v8a", "armeabi-v7a", "armeabi"}
		only64 = []string{"arm64-v8a"}
	)
	// Peacock rows: 0 = armeabi-v7a (needs Android 12L), 1 = arm64-v8a + armeabi-v7a (Android 6), 2 = arm64-v8a (Android 10)
	cases := []struct {
		app, version string
		device       []string
		sdk          int
		want         string // "" means no build for this CPU
	}{
		{"peacock", "7.8.100", armv7, 31, "/v/peacock/1"}, // the 32-bit-only row needs 12L; combined fits an Android 12 box
		{"peacock", "7.8.100", armv7, 32, "/v/peacock/1"}, // equal match: the combined build offers more ABIs
		{"peacock", "7.8.100", both, 33, "/v/peacock/1"},
		{"peacock", "7.8.100", only64, 33, "/v/peacock/1"},
		{"hbo", "7.9.0.61", armv7, 31, "/v/hbo/2"},
		{"twitch", "13.0.0.2", armv7, 28, "/v/twitch/1"},
		{"netflix", "13.0.1 build 25028", armv7, 31, "/v/netflix/0"},
		{"netflix", "13.0.1 build 25028", both, 31, "/v/netflix/0"}, // a 64-bit box still runs the only (32-bit) build
		{"netflix", "13.0.1 build 25028", only64, 31, ""},           // an arm64-only box cannot
		{"disney", "26.16.0+rc2-2026.09.08", only64, 31, "/v/disney/0"},
	}
	for _, c := range cases {
		got, err := PickVariant(realRows(t, c.app), c.version, c.sdk, c.device)
		if c.want == "" {
			if !errors.Is(err, ErrNoBuild) || !errors.Is(err, ErrNotFound) {
				t.Errorf("%s on %v: got %q %v, want a no-build error", c.app, c.device, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s v%s on %v (sdk %d): got %q %v, want %q", c.app, c.version, c.device, c.sdk, got, err, c.want)
		}
	}
}

func TestSDKTooOldIsNotReportedAsNoBuild(t *testing.T) {
	// the only 32-bit row needs Android 12L; on Android 10 that is an SDK problem, not a CPU one
	row := `<div class="table-row headerFont"><a class="accent_color" href="/v">v</a> 1.0 BUNDLE armeabi-v7a Android 12L+ 320dpi</div>`
	_, err := PickVariant(page(t, "<html>"+row+"</html>"), "1.0", 29, []string{"armeabi-v7a"})
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoBuild) {
		t.Errorf("got %v", err)
	}
}

// countingFetcher serves one saved release/variant/download chain and counts file downloads.
type countingFetcher struct {
	*scripted
	files int
}

func (f *countingFetcher) File(ctx context.Context, u, ref, dest string) error {
	f.files++
	return f.scripted.File(ctx, u, ref, dest)
}

func TestSameVariantIsNotDownloadedTwiceAndDifferentVariantsDoNotShareAName(t *testing.T) {
	single := fx(t, "single-release")
	btn, _ := downloadButton(page(t, single))
	rel := "https://m.test/apk/p/app/app-1-release/"
	mk := func(release string) *countingFetcher {
		return &countingFetcher{scripted: &scripted{pages: map[string]string{
			"https://m.test/":      "<title>home</title>",
			rel:                    release + " com.x",
			"https://m.test" + btn: fx(t, "download"),
		}}}
	}
	dir := t.TempDir()
	f := mk(single)
	c := &Client{F: f, Base: "https://m.test", Sleep: noSleep}
	req := AppRequest{Package: "com.x", Version: "1", Hint: "p/app", SDK: 31, DestDir: dir}
	r1, err := c.Fetch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Fetch(context.Background(), req)
	if err != nil || r1.Path != r2.Path || f.files != 1 {
		t.Fatalf("second fetch must reuse the cached file: %q %q files=%d %v", r1.Path, r2.Path, f.files, err)
	}

	// a release with a variant table names the file by the chosen variant
	row := func(href, cells string) string {
		return fmt.Sprintf(`<div class="table-row headerFont"><a class="accent_color" href=%q>v</a> %s</div>`, href, cells)
	}
	table := "<html>" + row("/v/arm64", "1 BUNDLE arm64-v8a Android 7.0+") + row("/v/armv7", "1 BUNDLE armeabi-v7a Android 7.0+") + "</html>"
	f2 := &countingFetcher{scripted: &scripted{pages: map[string]string{
		"https://m.test/":                        "<title>home</title>",
		rel:                                      table + " com.x",
		"https://m.test/v/arm64":                 "<html><a class=\"downloadButton\" href=\"/d/arm64/download/?key=1\">d</a></html>",
		"https://m.test/v/armv7":                 "<html><a class=\"downloadButton\" href=\"/d/armv7/download/?key=2\">d</a></html>",
		"https://m.test/d/arm64/download/?key=1": fx(t, "download"),
		"https://m.test/d/armv7/download/?key=2": fx(t, "download"),
	}}}
	c2 := &Client{F: f2, Base: "https://m.test", Sleep: noSleep}
	dir2 := t.TempDir()
	r64, err := c2.Fetch(context.Background(), AppRequest{Package: "com.x", Version: "1", Hint: "p/app", SDK: 31, DestDir: dir2, ABIs: []string{"arm64-v8a"}})
	if err != nil {
		t.Fatal(err)
	}
	r32, err := c2.Fetch(context.Background(), AppRequest{Package: "com.x", Version: "1", Hint: "p/app", SDK: 31, DestDir: dir2, ABIs: []string{"armeabi-v7a"}})
	if err != nil || r64.Path == r32.Path || f2.files != 2 {
		t.Fatalf("different variants must be cached under different names: %q %q files=%d %v", r64.Path, r32.Path, f2.files, err)
	}
}

func TestAvailableABIsOnRealRows(t *testing.T) {
	cases := map[string]string{
		"peacock": "arm64-v8a,armeabi-v7a", // all three variants together
		"netflix": "armeabi-v7a",
		"disney":  "arm64-v8a,armeabi-v7a",
	}
	for app, want := range cases {
		if got := strings.Join(AvailableABIs(realRows(t, app)), ","); got != want {
			t.Errorf("%s: %q, want %q", app, got, want)
		}
	}
	row := `<div class="table-row headerFont"><a class="accent_color" href="/v">v</a> 1.0 BUNDLE universal Android 5.0+</div>`
	if got := AvailableABIs(page(t, "<html>"+row+"</html>")); len(got) != 1 || got[0] != "*" {
		t.Errorf("universal builds are reported as *: %v", got)
	}
	if got := AvailableABIs(page(t, "<html>no table</html>")); len(got) != 0 {
		t.Errorf("a release without a table says nothing: %v", got)
	}
}

func TestResultCarriesAvailableAndNoBuildCarriesItToo(t *testing.T) {
	srv := mirror(t, nil)
	c := &Client{F: NewDirect(Profiles[0]), Base: srv.URL, Sleep: noSleep}
	r := req(t)
	r.Hint = slug
	res, err := c.Fetch(context.Background(), r)
	if err != nil || len(res.Available) != 1 || res.Available[0] != "armeabi-v7a" {
		t.Fatalf("%+v %v", res, err)
	}
	// an arm64-only device: the release only has a 32-bit build, and the error says what it has
	r2 := req(t)
	r2.Hint, r2.ABIs = slug, []string{"arm64-v8a"}
	_, err = c.Fetch(context.Background(), r2)
	nb, ok := errors.AsType[*NoBuildError](err)
	if !ok || !errors.Is(err, ErrNoBuild) || !errors.Is(err, ErrNotFound) || len(nb.Available) != 1 || nb.Available[0] != "armeabi-v7a" {
		t.Fatalf("%v", err)
	}
}

func TestProbeLearnsBuildsWithoutDownloading(t *testing.T) {
	var files int
	srv := mirror(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "download.php") {
			files++
		}
		return false
	})
	_ = srv
	// Probe uses Download(), which builds its own clients against the real site; exercise the
	// same path through a Client against the local mirror instead.
	c := &Client{F: NewDirect(Profiles[0]), Base: srv.URL, Sleep: noSleep}
	r := req(t)
	r.Hint, r.DryRun = slug, true
	res, err := c.Fetch(context.Background(), r)
	if err != nil || res.Path != srv.URL+relPath || files != 0 || len(res.Available) != 1 {
		t.Fatalf("%+v %v files=%d", res, err, files)
	}
}

// challengeSite answers 403 until a request carries the cookie value in *valid.
func challengeSite(valid *atomic.Pointer[string]) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("cf_clearance")
		if c == nil || c.Value != *valid.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, "<title>Just a moment...</title>")
			return
		}
		if strings.HasSuffix(r.URL.Path, ".php") {
			_, _ = fmt.Fprint(w, zipBytes)
			return
		}
		_, _ = fmt.Fprint(w, "<html><title>real</title></html>")
	}))
}

// counterSidecar hands out the cookie currently held in *issue and counts solves.
func counterSidecar(issue *atomic.Pointer[string], solves *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		solves.Add(1)
		v := *issue.Load()
		_, _ = fmt.Fprintf(w, `{"status":"ok","solution":{"response":"<html><title>solved</title></html>","status":200,"userAgent":"Sidecar/1","cookies":[{"name":"cf_clearance","value":%q}]}}`, v)
	}))
}

func TestFlareSolvesOnceForManyPages(t *testing.T) {
	var valid, issue atomic.Pointer[string]
	valid.Store(new("c1"))
	issue.Store(new("c1"))
	var solves atomic.Int32
	site := challengeSite(&valid)
	defer site.Close()
	sc := counterSidecar(&issue, &solves)
	defer sc.Close()

	f := NewFlare(sc.URL)
	for _, p := range []string{"/a", "/b", "/c"} {
		if _, err := f.Page(context.Background(), site.URL+p, ""); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(t.TempDir(), "f.apkm")
	if err := f.File(context.Background(), site.URL+"/d.php", site.URL, dest); err != nil {
		t.Fatal(err)
	}
	if n := solves.Load(); n != 1 {
		t.Errorf("solver called %d times, want 1", n)
	}
}

func TestFlareSolvesAgainWhenClearanceExpires(t *testing.T) {
	var valid, issue atomic.Pointer[string]
	valid.Store(new("c1"))
	issue.Store(new("c1"))
	var solves atomic.Int32
	site := challengeSite(&valid)
	defer site.Close()
	sc := counterSidecar(&issue, &solves)
	defer sc.Close()

	f := NewFlare(sc.URL)
	if _, err := f.Page(context.Background(), site.URL+"/a", ""); err != nil {
		t.Fatal(err)
	}
	valid.Store(new("c2"))
	issue.Store(new("c2"))
	if _, err := f.Page(context.Background(), site.URL+"/b", ""); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "f.apkm")
	valid.Store(new("c3"))
	issue.Store(new("c3"))
	if err := f.File(context.Background(), site.URL+"/d.php", site.URL, dest); err != nil {
		t.Fatal(err)
	}
	if n := solves.Load(); n != 3 {
		t.Errorf("solver called %d times, want 3", n)
	}
}
