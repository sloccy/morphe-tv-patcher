// Package web is the small control page: pick devices and apps, pair, run now.
// It has no login (LAN/Tailscale only, like the other homelab tools), so every
// state-changing request is checked against requests coming from other web pages.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/patches"
	"github.com/sloccy/morphe-tv-patcher/internal/runner"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

//go:embed ui/index.html
var ui embed.FS

// Backend is what the server needs from the runner.
type Backend interface {
	Status() runner.Status
	Trigger(runner.Options) bool
	List() *patches.List
	Pair(ctx context.Context, host string, pairPort int, code string) error
}

// Server serves the API and the page.
type Server struct {
	Store               *store.Store
	Backend             Backend
	Log                 interface{ Lines() []string }
	Version             string
	AllowedHostSuffixes []string
}

const maxBody = 64 << 10

// Handler returns the routed, guarded handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/log", s.log)
	mux.HandleFunc("POST /api/devices", s.addDevice)
	mux.HandleFunc("DELETE /api/devices/{id}", s.removeDevice)
	mux.HandleFunc("PUT /api/devices/{id}/apps", s.setApps)
	mux.HandleFunc("POST /api/devices/{id}/pair", s.pair)
	mux.HandleFunc("POST /api/run", s.run)
	return s.guard(mux)
}

// guard rejects requests that look like they come from another web page: a Host that is not
// an IP/localhost/allowed name (DNS rebinding), a mismatched Origin, or a non-JSON body.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostOK(r.Host) {
			http.Error(w, "unknown host name", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
			if r.Method != http.MethodDelete && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostOK(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" || net.ParseIP(h) != nil {
		return true
	}
	for _, suf := range s.AllowedHostSuffixes {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	b, _ := ui.ReadFile("ui/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			fail(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			fail(w, http.StatusBadRequest, "bad request body")
		}
		return false
	}
	return true
}

type appRow struct {
	Package      string `json:"package"`
	Name         string `json:"name"`
	Newest       string `json:"newest"`
	Experimental bool   `json:"experimental"`
}

type deviceRow struct {
	store.Device
	Installs map[string]store.Install `json:"installs"`
	Last     []runner.Result          `json:"last"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	st := s.Backend.Status()
	out := map[string]any{"version": s.Version, "status": st, "apps": []appRow{}}
	if l := s.Backend.List(); l != nil {
		rows := make([]appRow, 0, len(l.Apps))
		for _, a := range l.Apps {
			rows = append(rows, appRow{a.Package, a.Name, a.Newest, a.Experimental})
		}
		out["apps"] = rows
	}
	settings := s.Store.Settings()
	devs := make([]deviceRow, 0, len(settings.Devices))
	for _, d := range settings.Devices {
		row := deviceRow{Device: d, Installs: map[string]store.Install{}}
		for _, p := range d.Apps {
			if in, ok := s.Store.Install(d.ID, p); ok {
				row.Installs[p] = in
			}
		}
		for _, res := range st.Results {
			if res.Device == d.Name {
				row.Last = append(row.Last, res)
			}
		}
		devs = append(devs, row)
	}
	out["devices"] = devs
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	lines := []string{}
	if s.Log != nil {
		lines = s.Log.Lines()
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Host string }
	if !decode(w, r, &in) {
		return
	}
	d, err := s.Store.AddDevice(in.Name, strings.TrimSpace(in.Host))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Info("device added", "id", d.ID, "host", d.Host)
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) removeDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RemoveDevice(r.PathValue("id")); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	slog.Info("device removed", "id", r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setApps(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Apps []string `json:"apps"`
	}
	if !decode(w, r, &in) {
		return
	}
	l := s.Backend.List()
	if l == nil {
		fail(w, http.StatusServiceUnavailable, "the app list has not loaded yet; try again in a moment")
		return
	}
	for _, p := range in.Apps {
		if _, ok := l.App(p); !ok {
			fail(w, http.StatusBadRequest, "unknown app: "+p)
			return
		}
	}
	if err := s.Store.SetApps(r.PathValue("id"), in.Apps); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	apps := append([]string{}, in.Apps...)
	sort.Strings(apps)
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

var codeRE = regexp.MustCompile(`^\d{6}$`)

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PairPort int    `json:"pair_port"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.PairPort < 1024 || in.PairPort > 65535 || !codeRE.MatchString(in.Code) {
		fail(w, http.StatusBadRequest, "pairing port must be 1024-65535 and the code six digits")
		return
	}
	var host string
	for _, d := range s.Store.Settings().Devices {
		if d.ID == r.PathValue("id") {
			host = d.Host
		}
	}
	if host == "" {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.Backend.Pair(ctx, host, in.PairPort, in.Code); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "paired with " + host})
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Device string `json:"device"`
		Force  bool   `json:"force"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	if in.Device != "" {
		found := false
		for _, d := range s.Store.Settings().Devices {
			found = found || d.ID == in.Device
		}
		if !found {
			fail(w, http.StatusNotFound, "no such device")
			return
		}
	}
	if !s.Backend.Trigger(runner.Options{DeviceID: in.Device, Force: in.Force}) {
		fail(w, http.StatusConflict, "a run is already queued")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}
