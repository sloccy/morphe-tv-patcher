package patches

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixed test data directory
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The CLI's own view of the current repository's bundle must agree with patches-list.json.
func TestCLIParseMatchesPatchesListJSON(t *testing.T) {
	fromJSON, err := ParseList(readFile(t, "patches-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	cli := ParseListPatches(readFile(t, "list-patches-ajstrick81.txt"))
	if len(cli) != len(fromJSON.Apps) {
		t.Fatalf("packages: CLI %d, json %d", len(cli), len(fromJSON.Apps))
	}
	fromCLI := FromCLI("v1.42.2", cli, cli, fromJSON.Names())
	for _, a := range fromJSON.Apps {
		b, ok := fromCLI.App(a.Package)
		if !ok || b.Newest != a.Newest || strings.Join(b.Versions, "|") != strings.Join(a.Versions, "|") || b.Name != a.Name {
			t.Errorf("%s: json %+v, cli %+v (%v)", a.Package, a, b, ok)
		}
	}
}

func TestSecondRepositoryBundle(t *testing.T) {
	stable := ParseListPatches(readFile(t, "list-patches-morphe.txt"))
	all := ParseListPatches(readFile(t, "list-patches-morphe-x.txt"))
	l := FromCLI("v1.48.0", stable, all, nil)
	if len(l.Apps) != 3 {
		t.Fatalf("apps: %+v", l.Apps)
	}
	yt, ok := l.App("com.google.android.youtube")
	if !ok || yt.Name != "Youtube" || yt.Experimental {
		t.Fatalf("%+v", yt)
	}
	if len(yt.Versions) != 8 || len(stable["com.google.android.youtube"]) != 4 {
		t.Errorf("all %d stable %d", len(yt.Versions), len(stable["com.google.android.youtube"]))
	}
	// the target is the newest STABLE version; the newer experimental ones are listed but not chosen
	if yt.Newest != Newest(stable["com.google.android.youtube"]) {
		t.Errorf("newest %q, want the newest stable %q", yt.Newest, Newest(stable["com.google.android.youtube"]))
	}
	if all["com.google.android.youtube"][0] == "" {
		t.Error("versions should not be empty strings")
	}
	if rd, _ := l.App("com.reddit.frontpage"); rd.Name != "Reddit Frontpage" || rd.Newest == "" {
		t.Errorf("%+v", rd)
	}
	if ytm, _ := l.App("com.google.android.apps.youtube.music"); ytm.Name != "Youtube Music" {
		t.Errorf("%+v", ytm)
	}
}

func TestExperimentalOnlyPackageIsFlagged(t *testing.T) {
	stable := map[string][]string{"a.app": nil}
	all := map[string][]string{"a.app": {"2.0", "1.0"}}
	a, _ := FromCLI("t", stable, all, nil).App("a.app")
	if a.Newest != "2.0" || !a.Experimental {
		t.Errorf("%+v", a)
	}
}

func TestPatchesListJSONExperimentalVersionsAreNotChosenByDefault(t *testing.T) {
	data := `{"version":"1","appNames":{"p":"P"},"patches":[{"compatibility":[{"packageName":"p","targets":[
	  {"version":"1.0","experimental":false},{"version":"2.0","experimental":true}]}]}]}`
	l, err := ParseList([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := l.App("p"); a.Newest != "1.0" || a.Experimental || len(a.Versions) != 2 {
		t.Errorf("%+v", a)
	}
}

func TestParseListPatchesEdgeCases(t *testing.T) {
	out := "Index: 0\nName: A\nCompatible packages:\n\tPackage name: p.one\n\tCompatible versions:\n\t\t1.0\n\t\t2.0\n\n" +
		"Index: 1\nName: B\nCompatible packages:\n\tPackage name: p.one\n\tCompatible versions:\n\t\t2.0\n\t\t3.0\n\n" +
		"Index: 2\nName: Any version\nCompatible packages:\n\tPackage name: p.two\n\nIndex: 3\nName: Universal\n"
	got := ParseListPatches([]byte(out))
	if strings.Join(got["p.one"], ",") != "1.0,2.0,3.0" {
		t.Errorf("versions are merged without duplicates: %v", got["p.one"])
	}
	if v, ok := got["p.two"]; !ok || len(v) != 0 {
		t.Errorf("a package whose patches name no versions has none: %v %v", v, ok)
	}
	if len(got) != 2 {
		t.Errorf("universal patches are not apps: %v", got)
	}
}

func TestPrettyName(t *testing.T) {
	cases := map[string]string{
		"com.google.android.apps.youtube.music": "Youtube Music",
		"com.reddit.frontpage":                  "Reddit Frontpage",
		"tv.pluto.android":                      "Pluto",
		"com.x":                                 "X",
		"tv.android":                            "tv.android",
	}
	for in, want := range cases {
		if got := PrettyName(in); got != want {
			t.Errorf("PrettyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchListMissingFileIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer srv.Close()
	// FetchList builds a raw.githubusercontent.com URL, so check the 404 mapping on get() directly
	_, err := get(context.Background(), srv.Client(), srv.URL+"/x", "", 100)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("got %v", err)
	}
}

func TestDownloadLimitsAndPerSourceFolders(t *testing.T) {
	dir := t.TempDir()
	if _, err := Download(context.Background(), http.DefaultClient, &Release{Name: "patches-1.mpp", Size: MaxBundleBytes + 1, AssetURL: "http://x"}, dir); err == nil {
		t.Error("oversized bundles must be refused before downloading")
	}
	for _, name := range []string{"../evil.mpp", "notes.txt", "sub/x.mpp"} {
		if _, err := Download(context.Background(), http.DefaultClient, &Release{Name: name, Size: 3, AssetURL: "http://x"}, dir); err == nil {
			t.Errorf("%q must be refused", name)
		}
	}
	// two repositories keep separate folders, so pruning one never touches the other
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	_ = os.MkdirAll(a, 0o750)
	_ = os.MkdirAll(b, 0o750)
	_ = os.WriteFile(filepath.Join(b, "patches-9.mpp"), []byte("keep"), 0o600)
	_ = os.WriteFile(filepath.Join(a, "patches-1.mpp"), []byte("old"), 0o600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("new")) }))
	defer srv.Close()
	if _, err := Download(context.Background(), srv.Client(), &Release{Name: "patches-2.mpp", Size: 3, AssetURL: srv.URL}, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a, "patches-1.mpp")); err == nil {
		t.Error("old bundle in the same folder should go")
	}
	if _, err := os.Stat(filepath.Join(b, "patches-9.mpp")); err != nil {
		t.Error("another source's bundle must survive")
	}
}
