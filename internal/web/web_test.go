package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/patches"
	"github.com/sloccy/morphe-tv-patcher/internal/runner"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

const defSrc = "owner-default" // the repository seeded on the first run

type fakeBackend struct {
	lists     map[string]*patches.List
	status    runner.Status
	triggered []runner.Options
	queueFull bool
	pairErr   error
	paired    []string
	cpuKnown  bool
	problems  []runner.Problem // what Compat reports as known mismatches
	unchecked []string
	compatSaw map[string][]string // device id -> the packages it would receive, per Compat call
	checkErr  error
	checked   []string
	refreshed atomic.Int32 // written by the background refresh, read by the test
	warnings  []runner.Problem
	learning  runner.Learning
	testErr   error
	hold      chan struct{} // when set, Compat blocks until it is closed
	compatN   atomic.Int32
}

func (f *fakeBackend) TestDevice(ctx context.Context, d store.Device) (runner.DeviceCheck, error) {
	if f.testErr != nil {
		return runner.DeviceCheck{}, f.testErr
	}
	return runner.DeviceCheck{Port: 41234, ABIs: []string{"arm64-v8a"}, SDK: 34}, nil
}
func (f *fakeBackend) Warnings() []runner.Problem { return f.warnings }
func (f *fakeBackend) Learning() runner.Learning  { return f.learning }

func (f *fakeBackend) Status() runner.Status { return f.status }
func (f *fakeBackend) Trigger(o runner.Options) bool {
	if f.queueFull {
		return false
	}
	f.triggered = append(f.triggered, o)
	return true
}
func (f *fakeBackend) Lists() map[string]*patches.List { return f.lists }
func (f *fakeBackend) CheckRepo(ctx context.Context, repo string, prerelease bool) error {
	f.checked = append(f.checked, repo)
	return f.checkErr
}
func (f *fakeBackend) RefreshLists(context.Context) { f.refreshed.Add(1) }
func (f *fakeBackend) PairDevice(ctx context.Context, d store.Device, port int, code string) (bool, error) {
	f.paired = append(f.paired, d.Host+" "+code)
	return f.cpuKnown, f.pairErr
}
func (f *fakeBackend) Compat(ctx context.Context, st store.Settings, ids []string) (problems []runner.Problem, unchecked []string) {
	f.compatN.Add(1)
	if f.hold != nil {
		<-f.hold
	}
	if f.compatSaw == nil {
		f.compatSaw = map[string][]string{}
	}
	for _, id := range ids {
		eff := store.Effective(st, id)
		pk := make([]string, 0, len(eff))
		for _, e := range eff {
			pk = append(pk, e.Package)
		}
		f.compatSaw[id] = pk
	}
	return f.problems, f.unchecked
}

type ring struct{ lines []string }

func (r ring) Lines() []string { return r.lines }

func setup(t *testing.T) (srv *httptest.Server, s *Server, fb *fakeBackend) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile("../patches/testdata/patches-list.json")
	list, _ := patches.ParseList(data)
	_ = st.SeedSources([]string{"owner/default"}, false)
	fb = &fakeBackend{lists: map[string]*patches.List{defSrc: list}}
	s = &Server{Store: st, Backend: fb, Log: ring{[]string{"line one", "line two"}}, Version: "test"}
	srv = httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, s, fb
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, hdr map[string]string) (status int, respBody string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
	if body != "" || method == "POST" || method == "PUT" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if k == "Host" { // net/http only honours req.Host, not a Host header entry
			req.Host = v
			continue
		}
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPageAndHealth(t *testing.T) {
	srv, _, _ := setup(t)
	code, body := do(t, srv, "GET", "/", "", nil)
	if code != 200 || !strings.Contains(body, "TV Patcher") {
		t.Errorf("%d %.80q", code, body)
	}
	if code, body := do(t, srv, "GET", "/api/health", "", nil); code != 200 || body != "ok" {
		t.Errorf("%d %q", code, body)
	}
	if code, _ := do(t, srv, "GET", "/nope", "", nil); code != 404 {
		t.Errorf("unknown path: %d", code)
	}
}

func TestDeviceLifecycleAndState(t *testing.T) {
	srv, _, fb := setup(t)
	code, body := do(t, srv, "POST", "/api/devices", `{"name":"Living room","host":"10.0.0.6"}`, nil)
	if code != 201 || !strings.Contains(body, `"id":"living-room"`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, body = do(t, srv, "PUT", "/api/devices/living-room/apps", `{"apps":["com.netflix.ninja","com.amazon.amazonvideo.livingroom"]}`, nil); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	fb.status = runner.Status{Results: []runner.Result{{Device: "Living room", Message: "ok", OK: true}}}
	code, body = do(t, srv, "GET", "/api/state", "", nil)
	var st struct {
		Apps    []map[string]any `json:"apps"`
		Devices []struct {
			ID   string            `json:"id"`
			Apps []store.AppChoice `json:"apps"`
			Last []runner.Result   `json:"last"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || code != 200 {
		t.Fatalf("%d %v %s", code, err, body)
	}
	if len(st.Apps) != 17 || len(st.Devices) != 1 || len(st.Devices[0].Apps) != 2 || len(st.Devices[0].Last) != 1 {
		t.Errorf("apps %d devices %+v", len(st.Apps), st.Devices)
	}
	if code, _ = do(t, srv, "DELETE", "/api/devices/living-room", "", nil); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ = do(t, srv, "DELETE", "/api/devices/living-room", "", nil); code != 404 {
		t.Errorf("delete again: %d", code)
	}
}

func TestInputValidation(t *testing.T) {
	srv, s, fb := setup(t)
	_, _ = s.Store.AddDevice("TV", "10.0.0.6")
	bad := []struct{ method, path, body string }{
		{"POST", "/api/devices", `{"name":"x","host":"-oProxyCommand=evil"}`},
		{"POST", "/api/devices", `{"name":"x","host":"10.0.0.6; rm -rf /"}`},
		{"POST", "/api/devices", `{"name":"","host":"10.0.0.9"}`},
		{"POST", "/api/devices", `{"name":"x","host":"10.0.0.9","extra":1}`},
		{"POST", "/api/devices", `not json`},
		{"PUT", "/api/devices/tv/apps", `{"apps":["com.evil.unknown"]}`},
		{"POST", "/api/devices/tv/pair", `{"pair_port":80,"code":"123456"}`},
		{"POST", "/api/devices/tv/pair", `{"pair_port":40000,"code":"12345"}`},
		{"POST", "/api/devices/tv/pair", `{"pair_port":40000,"code":"abcdef"}`},
		{"POST", "/api/run", `{"device":"nope"}`},
	}
	for _, c := range bad {
		if code, body := do(t, srv, c.method, c.path, c.body, nil); code < 400 || code >= 500 {
			t.Errorf("%s %s %s -> %d %s (want a 4xx)", c.method, c.path, c.body, code, body)
		}
	}
	if len(fb.paired) != 0 || len(fb.triggered) != 0 {
		t.Error("bad requests must not reach adb or the scheduler")
	}
	if got := len(s.Store.Settings().Devices); got != 1 {
		t.Errorf("devices %d: bad requests must not change anything", got)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	srv, _, _ := setup(t)
	big := `{"name":"` + strings.Repeat("a", 100_000) + `","host":"10.0.0.9"}`
	if code, _ := do(t, srv, "POST", "/api/devices", big, nil); code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d", code)
	}
}

func TestCrossSiteProtections(t *testing.T) {
	srv, s, _ := setup(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	body := `{"name":"TV","host":"10.0.0.6"}`
	// another web page: its Origin does not match our Host
	if code, _ := do(t, srv, "POST", "/api/devices", body, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Errorf("foreign origin: %d", code)
	}
	// a plain HTML form cannot send application/json
	if code, _ := do(t, srv, "POST", "/api/devices", body, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); code != 415 {
		t.Errorf("form post: %d", code)
	}
	// same-origin is fine
	if code, b := do(t, srv, "POST", "/api/devices", body, map[string]string{"Origin": "http://" + host}); code != 201 {
		t.Errorf("same origin: %d %s", code, b)
	}
	// DNS rebinding: an attacker's name pointing at this machine
	if code, _ := do(t, srv, "GET", "/api/state", "", map[string]string{"Host": "evil.example.com"}); code != 403 {
		t.Errorf("rebinding host: %d", code)
	}
	// named access is allowed only for configured suffixes
	s.AllowedHostSuffixes = []string{".sloccy.com"}
	if code, _ := do(t, srv, "GET", "/api/health", "", map[string]string{"Host": "tvpatcher.sloccy.com"}); code != 200 {
		t.Errorf("allowed suffix: %d", code)
	}
	if code, _ := do(t, srv, "GET", "/api/health", "", map[string]string{"Host": "sloccy.com.evil.net"}); code != 403 {
		t.Errorf("suffix must match the end: %d", code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _, _ := setup(t)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("headers %v", resp.Header)
	}
}

func TestPairRunAndLog(t *testing.T) {
	srv, s, fb := setup(t)
	_, _ = s.Store.AddDevice("TV", "10.0.0.6")
	if code, b := do(t, srv, "POST", "/api/devices/tv/pair", `{"pair_port":38000,"code":"123456"}`, nil); code != 200 || !strings.Contains(b, "10.0.0.6") {
		t.Errorf("%d %s", code, b)
	}
	if len(fb.paired) != 1 || fb.paired[0] != "10.0.0.6 123456" {
		t.Errorf("%v", fb.paired)
	}
	fb.pairErr = errors.New("pairing failed: wrong code")
	if code, b := do(t, srv, "POST", "/api/devices/tv/pair", `{"pair_port":38000,"code":"000000"}`, nil); code != http.StatusBadGateway || !strings.Contains(b, "wrong code") {
		t.Errorf("%d %s", code, b)
	}
	if code, _ := do(t, srv, "POST", "/api/devices/ghost/pair", `{"pair_port":38000,"code":"123456"}`, nil); code != 404 {
		t.Errorf("unknown device: %d", code)
	}
	if code, _ := do(t, srv, "POST", "/api/run", `{"device":"tv","force":true}`, nil); code != 202 {
		t.Errorf("run: %d", code)
	}
	if code, _ := do(t, srv, "POST", "/api/run", ``, nil); code != 202 {
		t.Errorf("run with no body: %d", code)
	}
	if len(fb.triggered) != 2 || fb.triggered[0].DeviceID != "tv" || !fb.triggered[0].Force {
		t.Errorf("%+v", fb.triggered)
	}
	fb.queueFull = true
	if code, _ := do(t, srv, "POST", "/api/run", `{}`, nil); code != 409 {
		t.Errorf("queued: %d", code)
	}
	if code, b := do(t, srv, "GET", "/api/log", "", nil); code != 200 || !strings.Contains(b, "line two") {
		t.Errorf("%d %s", code, b)
	}
}

func TestAppListNotLoadedYet(t *testing.T) {
	srv, s, fb := setup(t)
	fb.lists = nil
	_, _ = s.Store.AddDevice("TV", "10.0.0.6")
	if code, _ := do(t, srv, "PUT", "/api/devices/tv/apps", `{"apps":["com.netflix.ninja"]}`, nil); code != 503 {
		t.Errorf("got %d", code)
	}
	if code, b := do(t, srv, "GET", "/api/state", "", nil); code != 200 || !strings.Contains(b, `"apps":[]`) {
		t.Errorf("%d %s", code, b)
	}
}

func TestNoSecretsInResponses(t *testing.T) {
	srv, _, _ := setup(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "SECRET123")
	for _, p := range []string{"/api/state", "/api/log", "/"} {
		if _, body := do(t, srv, "GET", p, "", nil); strings.Contains(body, "SECRET123") || strings.Contains(strings.ToLower(body), "keystore") && p != "/" {
			t.Errorf("%s leaked something: %.100s", p, body)
		}
	}
}

func TestDeviceCardCarriesCPUInfo(t *testing.T) {
	srv, s, _ := setup(t)
	d, _ := s.Store.AddDevice("Bedroom", "10.0.0.7")
	_ = s.Store.SetDeviceInfo(d.ID, store.DeviceInfo{ABIs: []string{"arm64-v8a", "armeabi-v7a", "armeabi"}, SDK: 34, SeenAt: "now"})
	_, body := do(t, srv, "GET", "/api/state", "", nil)
	var st struct {
		Devices []struct {
			Info *store.DeviceInfo `json:"info"`
			Is64 bool              `json:"is64"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || len(st.Devices) != 1 || !st.Devices[0].Is64 || st.Devices[0].Info == nil || st.Devices[0].Info.SDK != 34 {
		t.Errorf("%v %s", err, body)
	}
	d2, _ := s.Store.AddDevice("Old box", "10.0.0.8")
	_ = s.Store.SetDeviceInfo(d2.ID, store.DeviceInfo{ABIs: []string{"armeabi-v7a", "armeabi"}, SDK: 31})
	_, body = do(t, srv, "GET", "/api/state", "", nil)
	if !strings.Contains(body, `"is64":false`) {
		t.Errorf("a 32-bit box must not be reported as 64-bit: %s", body)
	}
}

// ---- patch sources ----

func twoSources(t *testing.T, s *Server, fb *fakeBackend) store.Source {
	t.Helper()
	src, err := s.Store.AddSource("MorpheApp/morphe-patches", false, true)
	if err != nil {
		t.Fatal(err)
	}
	st := map[string][]string{"com.google.android.youtube": {"20.1.0"}, "com.netflix.ninja": {"1.0"}}
	fb.lists[src.ID] = patches.FromCLI("v1.48.0", st, st, nil)
	return src
}

func TestStateMergesAppsAcrossSourcesDefaultFirst(t *testing.T) {
	srv, s, fb := setup(t)
	src := twoSources(t, s, fb)
	_, body := do(t, srv, "GET", "/api/state", "", nil)
	var st struct {
		Apps []struct {
			Package string `json:"package"`
			Name    string `json:"name"`
			Sources []struct {
				ID     string `json:"id"`
				Newest string `json:"newest"`
			} `json:"sources"`
		} `json:"apps"`
		Sources []map[string]any `json:"sources"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Apps) != 18 { // 17 from the default source + YouTube; Netflix appears once with two sources
		t.Errorf("apps %d", len(st.Apps))
	}
	for _, a := range st.Apps {
		switch a.Package {
		case "com.netflix.ninja":
			if len(a.Sources) != 2 || a.Sources[0].ID != defSrc || a.Sources[1].ID != src.ID || a.Name != "Netflix Android TV" {
				t.Errorf("netflix %+v", a)
			}
		case "com.google.android.youtube":
			if len(a.Sources) != 1 || a.Sources[0].ID != src.ID {
				t.Errorf("youtube %+v", a)
			}
		}
	}
	if len(st.Sources) != 2 || st.Sources[1]["allow_phone"] != true {
		t.Errorf("sources %v", st.Sources)
	}
}

func TestAddSourceFlow(t *testing.T) {
	srv, s, fb := setup(t)
	good := `{"repo":"MorpheApp/morphe-patches","trust":true}`
	if code, _ := do(t, srv, "POST", "/api/sources", `{"repo":"MorpheApp/morphe-patches"}`, nil); code != 400 {
		t.Errorf("without the trust confirmation: %d", code)
	}
	for _, bad := range []string{`{"repo":"nope","trust":true}`, `{"repo":"a/b/c","trust":true}`, `{"repo":"-x/y;z","trust":true}`, `{"repo":"","trust":true}`, `{"repo":"a/b","trust":true,"extra":1}`} {
		if code, _ := do(t, srv, "POST", "/api/sources", bad, nil); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if len(fb.checked) != 0 {
		t.Error("invalid input must not cause GitHub lookups")
	}
	fb.checkErr = errors.New("no release with an .mpp asset")
	if code, b := do(t, srv, "POST", "/api/sources", good, nil); code != http.StatusBadGateway || !strings.Contains(b, ".mpp") {
		t.Errorf("a repo without a bundle: %d %s", code, b)
	}
	if len(s.Store.Settings().Sources) != 1 {
		t.Error("a refused repository must not be saved")
	}
	fb.checkErr = nil
	if code, b := do(t, srv, "POST", "/api/sources", `{"repo":"https://github.com/MorpheApp/morphe-patches/","trust":true,"allow_phone":true}`, nil); code != 201 || !strings.Contains(b, `"allow_phone":true`) {
		t.Fatalf("a full GitHub URL is fine: %d %s", code, b)
	}
	if got := s.Store.Settings().Sources; len(got) != 2 || got[1].Repo != "MorpheApp/morphe-patches" || !got[1].AllowPhone {
		t.Errorf("%+v", got)
	}
	for range 50 { // the refresh runs in the background
		if fb.refreshed.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fb.refreshed.Load() == 0 {
		t.Error("adding a source should load its app list")
	}
	if code, _ := do(t, srv, "POST", "/api/sources", good, nil); code != 400 {
		t.Errorf("duplicate: %d", code)
	}
}

func TestOwnerAllowListWhenSet(t *testing.T) {
	srv, s, fb := setup(t)
	s.AllowedOwners = []string{"morpheapp"}
	if code, b := do(t, srv, "POST", "/api/sources", `{"repo":"someone-else/patches","trust":true}`, nil); code != 403 || !strings.Contains(b, "PATCH_SOURCES_ALLOWED_OWNERS") {
		t.Errorf("%d %s", code, b)
	}
	if len(fb.checked) != 0 {
		t.Error("a disallowed owner is refused before any network call")
	}
	if code, _ := do(t, srv, "POST", "/api/sources", `{"repo":"MorpheApp/morphe-patches","trust":true}`, nil); code != 201 {
		t.Errorf("allowed owner (any case): %d", code)
	}
}

func TestRemoveSource(t *testing.T) {
	srv, s, fb := setup(t)
	src := twoSources(t, s, fb)
	d, _ := s.Store.AddDevice("TV", "10.0.0.6")
	_ = s.Store.SetApps(d.ID, []store.AppChoice{{Package: "com.google.android.youtube", Source: src.ID}, {Package: "com.netflix.ninja"}})
	if code, _ := do(t, srv, "DELETE", "/api/sources/nope", "", nil); code != 404 {
		t.Errorf("unknown: %d", code)
	}
	if code, b := do(t, srv, "DELETE", "/api/sources/"+src.ID, "", nil); code != 200 || !strings.Contains(b, `"apps_dropped":1`) {
		t.Errorf("%d %s", code, b)
	}
	if apps := s.Store.Settings().Devices[0].Apps; len(apps) != 1 || apps[0].Package != "com.netflix.ninja" {
		t.Errorf("%+v", apps)
	}
}

func TestSetAppsWithSourceChoices(t *testing.T) {
	srv, s, fb := setup(t)
	src := twoSources(t, s, fb)
	_, _ = s.Store.AddDevice("TV", "10.0.0.6")
	ok := `{"apps":[{"package":"com.google.android.youtube","source":"` + src.ID + `"},{"package":"com.netflix.ninja","source":"owner-default"},"com.disney.disneyplus"]}`
	if code, b := do(t, srv, "PUT", "/api/devices/tv/apps", ok, nil); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if apps := s.Store.Settings().Devices[0].Apps; len(apps) != 3 || apps[0].Package != "com.disney.disneyplus" || apps[0].Source != defSrc {
		t.Errorf("a plain package name means the default source: %+v", apps)
	}
	bad := map[string]int{
		`{"apps":[{"package":"com.netflix.ninja","source":"ghost"}]}`:                  400,
		`{"apps":[{"package":"com.google.android.youtube","source":"owner-default"}]}`: 400, // not in that source's bundle
		`{"apps":[{"package":"com.not.real","source":"` + src.ID + `"}]}`:              400,
	}
	for body, want := range bad {
		if code, _ := do(t, srv, "PUT", "/api/devices/tv/apps", body, nil); code != want {
			t.Errorf("%s: %d", body, code)
		}
	}
	delete(fb.lists, src.ID) // a source whose list has not loaded yet
	if code, _ := do(t, srv, "PUT", "/api/devices/tv/apps", ok, nil); code != 503 {
		t.Errorf("unloaded source: %d", code)
	}
}

func TestTheFirstRepositoryCanBeRemovedToo(t *testing.T) {
	srv, s, _ := setup(t)
	if code, _ := do(t, srv, "DELETE", "/api/sources/"+defSrc, "", nil); code != 200 {
		t.Fatalf("every patch source is removable: %d", code)
	}
	if len(s.Store.Settings().Sources) != 0 {
		t.Error("source still there")
	}
	_, body := do(t, srv, "GET", "/api/state", "", nil)
	if !strings.Contains(body, `"sources":[]`) || !strings.Contains(body, `"apps":[]`) {
		t.Errorf("an empty configuration should render, not crash: %s", body)
	}
}

func TestStateShowsAnUnreachableSourcesWarning(t *testing.T) {
	srv, _, fb := setup(t)
	fb.status = runner.Status{Sources: []runner.SourceStatus{{ID: defSrc, Repo: "owner/default", Tag: "v1", Warning: "HTTP 404. Using the last good bundle (v1)"}}}
	_, body := do(t, srv, "GET", "/api/state", "", nil)
	if !strings.Contains(body, `"warning":"HTTP 404. Using the last good bundle (v1)"`) {
		t.Errorf("%s", body)
	}
}

// ---- app sets ----

func twoTVs(t *testing.T, s *Server) (first, second store.Device) {
	t.Helper()
	a, _ := s.Store.AddDevice("Living room", "10.0.0.6")
	b, _ := s.Store.AddDevice("Bedroom", "10.0.0.7")
	return a, b
}

func TestCreateASetAndGiveItToBothTVs(t *testing.T) {
	srv, s, fb := setup(t)
	a, b := twoTVs(t, s)
	fb.unchecked = []string{"Bedroom has not been reached yet"}
	body := `{"name":"Streaming","apps":[{"package":"com.netflix.ninja"},"com.disney.disneyplus"],"devices":["` + a.ID + `","` + b.ID + `"]}`
	code, resp := do(t, srv, "POST", "/api/sets", body, nil)
	if code != 201 || !strings.Contains(resp, `"unchecked":["Bedroom has not been reached yet"]`) {
		t.Fatalf("%d %s", code, resp)
	}
	for _, d := range []store.Device{a, b} {
		if got := strings.Join(fb.compatSaw[d.ID], ","); got != "com.disney.disneyplus,com.netflix.ninja" {
			t.Errorf("the check must see what %s would receive (before saving): %q", d.Name, got)
		}
		if got := s.Store.EffectiveApps(d.ID); len(got) != 2 || got[0].Via != "set:streaming" {
			t.Errorf("%s: %+v", d.Name, got)
		}
	}
	if sets := s.Store.Settings().Sets; len(sets) != 1 || sets[0].Apps[0].Source != defSrc {
		t.Errorf("%+v", sets)
	}
}

func TestBlockedSaveSavesNothing(t *testing.T) {
	srv, s, fb := setup(t)
	a, b := twoTVs(t, s)
	fb.problems = []runner.Problem{{Device: "Bedroom", Package: "com.netflix.ninja", Name: "Netflix", Reason: "Netflix only has armeabi-v7a builds; Bedroom runs arm64-v8a"}}
	body := `{"name":"Streaming","apps":[{"package":"com.netflix.ninja"}],"devices":["` + a.ID + `","` + b.ID + `"]}`
	code, resp := do(t, srv, "POST", "/api/sets", body, nil)
	if code != http.StatusConflict {
		t.Fatalf("%d %s", code, resp)
	}
	var got struct {
		Error    string           `json:"error"`
		Problems []runner.Problem `json:"problems"`
	}
	if err := json.Unmarshal([]byte(resp), &got); err != nil || len(got.Problems) != 1 || got.Problems[0].Device != "Bedroom" || !strings.Contains(got.Error, "not saved") {
		t.Errorf("%v %+v", err, got)
	}
	if len(s.Store.Settings().Sets) != 0 || len(s.Store.EffectiveApps(a.ID)) != 0 {
		t.Error("a blocked save must change nothing, for any device")
	}
	// the same set for the device that can run it is accepted
	fb.problems = nil
	if code, _ := do(t, srv, "POST", "/api/sets", `{"name":"Streaming","apps":[{"package":"com.netflix.ninja"}],"devices":["`+a.ID+`"]}`, nil); code != 201 {
		t.Errorf("%d", code)
	}
	// editing the set so it would break a device is blocked as well, and leaves the set as it was
	fb.problems = []runner.Problem{{Device: "Living room", Package: "com.disney.disneyplus", Name: "Disney+", Reason: "x"}}
	if code, _ := do(t, srv, "PUT", "/api/sets/streaming", `{"name":"Streaming","apps":[{"package":"com.disney.disneyplus"}]}`, nil); code != 409 {
		t.Errorf("%d", code)
	}
	if apps := s.Store.Settings().Sets[0].Apps; len(apps) != 1 || apps[0].Package != "com.netflix.ninja" {
		t.Errorf("%+v", apps)
	}
}

func TestEditingASetChecksWhoUsesItAndNewDevices(t *testing.T) {
	srv, s, fb := setup(t)
	a, b := twoTVs(t, s)
	_, _ = s.Store.AddSet("Streaming", []store.AppChoice{{Package: "com.netflix.ninja"}}, []string{a.ID})
	// change the apps without naming devices: the device using it is checked
	if code, _ := do(t, srv, "PUT", "/api/sets/streaming", `{"name":"Streaming","apps":[{"package":"com.disney.disneyplus"}]}`, nil); code != 200 {
		t.Fatalf("%d", code)
	}
	if _, ok := fb.compatSaw[a.ID]; !ok {
		t.Error("the device that already uses the set must be checked")
	}
	if got := strings.Join(fb.compatSaw[a.ID], ","); got != "com.disney.disneyplus" {
		t.Errorf("%q", got)
	}
	// push it to the second TV as well in the same save
	if code, _ := do(t, srv, "PUT", "/api/sets/streaming", `{"name":"Streaming","apps":[{"package":"com.disney.disneyplus"}],"devices":["`+a.ID+`","`+b.ID+`"]}`, nil); code != 200 {
		t.Fatalf("%d", code)
	}
	if got := s.Store.EffectiveApps(b.ID); len(got) != 1 {
		t.Errorf("%+v", got)
	}
	// and take it off the first
	if code, _ := do(t, srv, "PUT", "/api/sets/streaming", `{"name":"Streaming","apps":[{"package":"com.disney.disneyplus"}],"devices":["`+b.ID+`"]}`, nil); code != 200 {
		t.Fatalf("%d", code)
	}
	if got := s.Store.EffectiveApps(a.ID); len(got) != 0 {
		t.Errorf("%+v", got)
	}
	if code, _ := do(t, srv, "PUT", "/api/sets/ghost", `{"name":"x","apps":[]}`, nil); code != 404 {
		t.Errorf("unknown set: %d", code)
	}
}

func TestDeviceSetsAndExtrasAreCheckedToo(t *testing.T) {
	srv, s, fb := setup(t)
	a, _ := twoTVs(t, s)
	_, _ = s.Store.AddSet("Streaming", []store.AppChoice{{Package: "com.netflix.ninja"}}, nil)
	if code, _ := do(t, srv, "PUT", "/api/devices/"+a.ID+"/sets", `{"sets":["streaming"]}`, nil); code != 200 {
		t.Fatalf("%d", code)
	}
	if got := strings.Join(fb.compatSaw[a.ID], ","); got != "com.netflix.ninja" {
		t.Errorf("%q", got)
	}
	for _, c := range []struct{ path, body string }{
		{"/api/devices/" + a.ID + "/sets", `{"sets":["ghost"]}`},
		{"/api/devices/ghost/sets", `{"sets":[]}`},
	} {
		if code, _ := do(t, srv, "PUT", c.path, c.body, nil); code < 400 || code >= 500 {
			t.Errorf("%s: %d", c.path, code)
		}
	}
	fb.problems = []runner.Problem{{Device: "Living room", Package: "com.disney.disneyplus", Name: "Disney+", Reason: "x"}}
	if code, _ := do(t, srv, "PUT", "/api/devices/"+a.ID+"/apps", `{"apps":[{"package":"com.disney.disneyplus"}]}`, nil); code != 409 {
		t.Errorf("extras are checked: %d", code)
	}
	if got := s.Store.Settings().Devices[0].Apps; len(got) != 0 {
		t.Errorf("%+v", got)
	}
	if code, _ := do(t, srv, "PUT", "/api/devices/"+a.ID+"/sets", `{"sets":["streaming"]}`, nil); code != 409 {
		t.Errorf("a set that breaks the device is refused: %d", code)
	}
}

func TestSetValidationAndRemoval(t *testing.T) {
	srv, s, fb := setup(t)
	a, _ := twoTVs(t, s)
	bad := map[string]int{
		`{"name":"","apps":[]}`:                                                  400,
		`{"name":"x","apps":[{"package":"com.not.real"}]}`:                       400,
		`{"name":"x","apps":[{"package":"com.netflix.ninja","source":"ghost"}]}`: 400,
		`{"name":"x","apps":[],"devices":["ghost"]}`:                             400,
		`{"name":"x","apps":[],"extra":1}`:                                       400,
		`{"name":"` + strings.Repeat("n", 70) + `","apps":[]}`:                   400,
	}
	for body, want := range bad {
		if code, _ := do(t, srv, "POST", "/api/sets", body, nil); code != want {
			t.Errorf("%s: %d", body, code)
		}
	}
	if len(s.Store.Settings().Sets) != 0 {
		t.Error("nothing saved")
	}
	if code, _ := do(t, srv, "POST", "/api/sets", `{"name":"Empty","apps":[]}`, nil); code != 201 {
		t.Errorf("an empty set is allowed: %d", code)
	}
	if code, _ := do(t, srv, "POST", "/api/sets", `{"name":"empty","apps":[]}`, nil); code != 400 {
		t.Errorf("names are unique: %d", code)
	}
	delete(fb.lists, defSrc)
	if code, _ := do(t, srv, "POST", "/api/sets", `{"name":"S2","apps":[{"package":"com.netflix.ninja"}]}`, nil); code != 503 {
		t.Errorf("source not loaded: %d", code)
	}
	_ = a
	if code, _ := do(t, srv, "DELETE", "/api/sets/empty", "", nil); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do(t, srv, "DELETE", "/api/sets/empty", "", nil); code != 404 {
		t.Errorf("delete again: %d", code)
	}
	if code, _ := do(t, srv, "POST", "/api/sets", `{"name":"x","apps":[]}`, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Errorf("cross-origin: %d", code)
	}
}

func TestStateShowsSetsAndWhereEachAppComesFrom(t *testing.T) {
	srv, s, _ := setup(t)
	a, b := twoTVs(t, s)
	_, _ = s.Store.AddSet("Streaming", []store.AppChoice{{Package: "com.netflix.ninja"}}, []string{a.ID, b.ID})
	_ = s.Store.SetApps(a.ID, []store.AppChoice{{Package: "com.disney.disneyplus"}})
	_, body := do(t, srv, "GET", "/api/state", "", nil)
	var st struct {
		Sets []struct {
			ID      string   `json:"id"`
			Devices []string `json:"devices"`
		} `json:"sets"`
		Devices []struct {
			ID        string `json:"id"`
			Effective []struct {
				Package string `json:"package"`
				Via     string `json:"via"`
			} `json:"effective"`
			Sets []string `json:"sets"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Sets) != 1 || len(st.Sets[0].Devices) != 2 {
		t.Errorf("%+v", st.Sets)
	}
	living := make([]string, 0, len(st.Devices[0].Effective))
	for _, e := range st.Devices[0].Effective {
		living = append(living, e.Package+"<"+e.Via)
	}
	if strings.Join(living, " ") != "com.disney.disneyplus<device com.netflix.ninja<set:streaming" || len(st.Devices[0].Sets) != 1 {
		t.Errorf("%v", living)
	}
}

func TestPairMessageMentionsTheCPUWhenRead(t *testing.T) {
	srv, s, fb := setup(t)
	d, _ := s.Store.AddDevice("TV", "10.0.0.6")
	_ = s.Store.SetDeviceInfo(d.ID, store.DeviceInfo{ABIs: []string{"arm64-v8a", "armeabi-v7a"}, SDK: 34})
	fb.cpuKnown = true
	if _, b := do(t, srv, "POST", "/api/devices/tv/pair", `{"pair_port":38000,"code":"123456"}`, nil); !strings.Contains(b, "arm64-v8a") {
		t.Errorf("%s", b)
	}
	fb.cpuKnown = false
	if _, b := do(t, srv, "POST", "/api/devices/tv/pair", `{"pair_port":38000,"code":"123456"}`, nil); !strings.Contains(b, "could not be read") {
		t.Errorf("%s", b)
	}
}

func TestStateCarriesLearningAndWarningsAndAnETag(t *testing.T) {
	srv, _, fb := setup(t)
	fb.learning = runner.Learning{Pending: []string{"Netflix"}}
	fb.warnings = []runner.Problem{{Device: "TV", Package: "com.netflix.ninja", Name: "Netflix", Reason: "x"}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/state", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" || !strings.Contains(string(b), `"pending":["Netflix"]`) || !strings.Contains(string(b), `"warnings":[{`) {
		t.Fatalf("%q %s", etag, b)
	}
	if code, _ := do(t, srv, "GET", "/api/state", "", map[string]string{"If-None-Match": etag}); code != http.StatusNotModified {
		t.Errorf("unchanged state should be 304, got %d", code)
	}
	fb.learning = runner.Learning{}
	if code, _ := do(t, srv, "GET", "/api/state", "", map[string]string{"If-None-Match": etag}); code != 200 {
		t.Errorf("changed state must be sent again, got %d", code)
	}
	if code, _ := do(t, srv, "GET", "/", "", nil); code != 200 {
		t.Fatal(code)
	}
}

func TestOverlappingSavesDoNotStartMoreWork(t *testing.T) {
	srv, s, fb := setup(t)
	_, _ = s.Store.AddDevice("TV", "10.0.0.6")
	fb.hold = make(chan struct{})
	body := `{"apps":["com.netflix.ninja"]}`
	first := make(chan int, 1)
	go func() { code, _ := do(t, srv, "PUT", "/api/devices/tv/apps", body, nil); first <- code }()
	for fb.compatN.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	for range 5 {
		if code, b := do(t, srv, "PUT", "/api/devices/tv/apps", body, nil); code != http.StatusConflict || !strings.Contains(b, "still being checked") {
			t.Errorf("%d %s", code, b)
		}
	}
	close(fb.hold)
	if code := <-first; code != 200 {
		t.Errorf("the first save should finish normally: %d", code)
	}
	if fb.compatN.Load() != 1 {
		t.Errorf("only one check ran, got %d", fb.compatN.Load())
	}
}

func TestTestConnectionButton(t *testing.T) {
	srv, s, fb := setup(t)
	_, _ = s.Store.AddDevice("TV", "10.0.0.6")
	code, b := do(t, srv, "POST", "/api/devices/tv/test", "", nil)
	if code != 200 || !strings.Contains(b, "port 41234") || !strings.Contains(b, "arm64-v8a") || !strings.Contains(b, "API 34") {
		t.Fatalf("%d %s", code, b)
	}
	fb.testErr = errors.New("could not reach TV. Wireless debugging is switched off")
	if code, b := do(t, srv, "POST", "/api/devices/tv/test", "", nil); code != 502 || !strings.Contains(b, "switched off") {
		t.Errorf("%d %s", code, b)
	}
	if code, _ := do(t, srv, "POST", "/api/devices/ghost/test", "", nil); code != 404 {
		t.Errorf("unknown device: %d", code)
	}
	if code, _ := do(t, srv, "POST", "/api/devices/tv/test", "", map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Errorf("cross-site POST must be refused: %d", code)
	}
}
