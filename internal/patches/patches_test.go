package patches

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionOrder(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"7.8.100", "7.10.102", "7.5.102"}, "7.10.102"},
		{[]string{"26.12.1+rc1-2026.07.15", "26.16.0+rc2-2026.09.08", "26.9.2+rc1-2026.06.12"}, "26.16.0+rc2-2026.09.08"},
		{[]string{"16.8.0", "16.17.0", "16.12.0"}, "16.17.0"},
		{[]string{"13.0.1 build 25028"}, "13.0.1 build 25028"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := Newest(c.in); got != c.want {
			t.Errorf("Newest(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	got := SortVersions([]string{"1.9", "1.10", "1.2", "1.9"})
	if fmt.Sprint(got) != "[1.2 1.9 1.10]" {
		t.Errorf("SortVersions = %v", got)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"6.23.23+v15.5.0.70-armv7a": "6-23-23v15-5-0-70-armv7a",
		"13.0.1 build 25028":        "13-0-1-build-25028",
		"26.16.0+rc2-2026.09.08":    "26-16-0rc2-2026-09-08",
		"4.47.2_tv":                 "4-47-2_tv",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseListRealFile(t *testing.T) {
	data, err := os.ReadFile("testdata/patches-list.json")
	if err != nil {
		t.Fatal(err)
	}
	l, err := ParseList(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Apps) != 17 {
		t.Errorf("apps = %d, want 17", len(l.Apps))
	}
	want := map[string]string{
		"com.amazon.amazonvideo.livingroom": "6.23.23+v15.5.0.70-armv7a",
		"com.netflix.ninja":                 "13.0.1 build 25028",
		"com.disney.disneyplus":             "26.16.0+rc2-2026.09.08",
	}
	for pkg, v := range want {
		a, ok := l.App(pkg)
		if !ok || a.Newest != v || a.Name == pkg {
			t.Errorf("%s: %+v ok=%v", pkg, a, ok)
		}
	}
	if _, ok := l.App("nope"); ok {
		t.Error("unknown package found")
	}
}

func TestLatestReleaseAndDownload(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_, _ = fmt.Fprintf(w, `[
			 {"tag_name":"v9","draft":true,"assets":[{"name":"patches-9.mpp","size":3,"browser_download_url":"%[1]s/f9"}]},
			 {"tag_name":"v8","prerelease":true,"assets":[{"name":"patches-8.mpp","size":3,"browser_download_url":"%[1]s/f8"}]},
			 {"tag_name":"v7","assets":[{"name":"notes.txt","size":1,"browser_download_url":"x"}]},
			 {"tag_name":"v6","assets":[{"name":"patches-6.mpp","size":3,"browser_download_url":"%[1]s/f6"}]}]`, srv.URL)
		case "/f6":
			_, _ = w.Write([]byte("abc"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	rel, err := LatestRelease(context.Background(), srv.Client(), "o/r", "", false)
	if err != nil || rel.Tag != "v6" {
		t.Fatalf("got %+v %v (draft, prerelease and asset-less releases must be skipped)", rel, err)
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "patches-1.mpp"), []byte("old"), 0o600)
	path, err := Download(context.Background(), srv.Client(), rel, dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "abc" { //nolint:gosec // test temp dir
		t.Errorf("content %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "patches-1.mpp")); err == nil {
		t.Error("old bundle should be removed")
	}
	// second call must not hit the network (server would 404 /f6 nowhere else; just check it succeeds)
	if _, err := Download(context.Background(), srv.Client(), rel, dir); err != nil {
		t.Error(err)
	}
}

func TestCachedRemembersTheLastGoodRelease(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := Cached(dir); ok {
		t.Fatal("nothing cached yet")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("abc")) }))
	defer srv.Close()
	rel := &Release{Tag: "v7", Name: "patches-7.mpp", Size: 3, AssetURL: srv.URL}
	if _, err := Download(context.Background(), srv.Client(), rel, dir); err != nil {
		t.Fatal(err)
	}
	got, path, ok := Cached(dir)
	if !ok || got.Tag != "v7" || filepath.Base(path) != "patches-7.mpp" {
		t.Fatalf("%+v %q %v", got, path, ok)
	}
	// the upstream repository vanishes: the cached bundle is still there
	if b, _ := os.ReadFile(path); string(b) != "abc" { //nolint:gosec // test temp dir
		t.Error("cached bundle content")
	}
	// a damaged or truncated cache is not trusted
	_ = os.WriteFile(path, []byte("ab"), 0o600)
	if _, _, ok := Cached(dir); ok {
		t.Error("a truncated bundle must not be used")
	}
	_ = os.WriteFile(filepath.Join(dir, "release.json"), []byte(`{"tag":"v","name":"../../etc/passwd"}`), 0o600)
	if _, _, ok := Cached(dir); ok {
		t.Error("a metadata file naming a path outside the folder must be refused")
	}
}
