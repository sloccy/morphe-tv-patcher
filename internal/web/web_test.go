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
	"testing"

	"github.com/sloccy/morphe-tv-patcher/internal/patches"
	"github.com/sloccy/morphe-tv-patcher/internal/runner"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

type fakeBackend struct {
	list      *patches.List
	status    runner.Status
	triggered []runner.Options
	queueFull bool
	pairErr   error
	paired    []string
}

func (f *fakeBackend) Status() runner.Status { return f.status }
func (f *fakeBackend) Trigger(o runner.Options) bool {
	if f.queueFull {
		return false
	}
	f.triggered = append(f.triggered, o)
	return true
}
func (f *fakeBackend) List() *patches.List { return f.list }
func (f *fakeBackend) Pair(ctx context.Context, host string, port int, code string) error {
	f.paired = append(f.paired, host+" "+code)
	return f.pairErr
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
	fb = &fakeBackend{list: list}
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
	fb.status = runner.Status{PatchesVersion: "v1.42.2", Results: []runner.Result{{Device: "Living room", Message: "ok", OK: true}}}
	code, body = do(t, srv, "GET", "/api/state", "", nil)
	var st struct {
		Apps    []map[string]any `json:"apps"`
		Devices []struct {
			ID   string          `json:"id"`
			Apps []string        `json:"apps"`
			Last []runner.Result `json:"last"`
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
	fb.list = nil
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
