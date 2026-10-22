// Package web is the small control page: pick devices and apps, pair, run now.
// It has no login (LAN/Tailscale only, like the other homelab tools), so every
// state-changing request is checked against requests coming from other web pages.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/abi"
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
	Lists() map[string]*patches.List // patch source id -> the apps its latest bundle supports
	PairDevice(ctx context.Context, d store.Device, pairPort int, code string) (cpuKnown bool, err error)
	Compat(ctx context.Context, st store.Settings, deviceIDs []string) ([]runner.Problem, []string)
	CheckRepo(ctx context.Context, repo string, prerelease bool) error
	RefreshLists(ctx context.Context)
}

// Server serves the API and the page.
type Server struct {
	Store               *store.Store
	Backend             Backend
	Log                 interface{ Lines() []string }
	Version             string
	AllowedHostSuffixes []string
	// AllowedOwners restricts patch sources added from the page to these GitHub owners
	// (lower case). Empty means any repository.
	AllowedOwners []string
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
	mux.HandleFunc("PUT /api/devices/{id}/sets", s.setDeviceSets)
	mux.HandleFunc("POST /api/sets", s.addSet)
	mux.HandleFunc("PUT /api/sets/{id}", s.updateSet)
	mux.HandleFunc("DELETE /api/sets/{id}", s.removeSet)
	mux.HandleFunc("POST /api/sources", s.addSource)
	mux.HandleFunc("DELETE /api/sources/{id}", s.removeSource)
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

type appSource struct {
	ID           string `json:"id"`
	Newest       string `json:"newest"`
	Experimental bool   `json:"experimental"`
}

type appRow struct {
	Package string      `json:"package"`
	Name    string      `json:"name"`
	Sources []appSource `json:"sources"` // every patch source that supports it (the default first)
}

type sourceRow struct {
	store.Source
	Tag     string `json:"tag"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
	Apps    int    `json:"apps"`
}

// mergedApps lists each package once, with the sources that support it.
func mergedApps(sources []store.Source, lists map[string]*patches.List) []appRow {
	byPkg := map[string]*appRow{}
	for _, src := range sources { // settings order: the default source is first
		l := lists[src.ID]
		if l == nil {
			continue
		}
		for _, a := range l.Apps {
			if a.Newest == "" {
				continue
			}
			row := byPkg[a.Package]
			if row == nil {
				row = &appRow{Package: a.Package, Name: a.Name}
				byPkg[a.Package] = row
			}
			row.Sources = append(row.Sources, appSource{ID: src.ID, Newest: a.Newest, Experimental: a.Experimental})
		}
	}
	rows := make([]appRow, 0, len(byPkg))
	for _, r := range byPkg {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Package < rows[j].Package
	})
	return rows
}

type deviceRow struct {
	store.Device
	Effective []store.EffectiveApp     `json:"effective"` // everything it gets: its sets plus its own extras
	Installs  map[string]store.Install `json:"installs"`
	Last      []runner.Result          `json:"last"`
	Info      *store.DeviceInfo        `json:"info,omitempty"` // CPU list and Android level, once reached
	Is64      bool                     `json:"is64"`
}

type setRow struct {
	store.AppSet
	Devices []string `json:"devices"` // ids of the devices that use it
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	st := s.Backend.Status()
	settings := s.Store.Settings()
	lists := s.Backend.Lists()
	out := map[string]any{"version": s.Version, "status": st, "apps": mergedApps(settings.Sources, lists),
		"allowed_owners": s.AllowedOwners}
	srcRows := make([]sourceRow, 0, len(settings.Sources))
	for _, src := range settings.Sources {
		row := sourceRow{Source: src}
		for _, ss := range st.Sources {
			if ss.ID == src.ID {
				row.Tag, row.Error, row.Warning = ss.Tag, ss.Error, ss.Warning
			}
		}
		if l := lists[src.ID]; l != nil {
			row.Apps = len(l.Apps)
		}
		srcRows = append(srcRows, row)
	}
	out["sources"] = srcRows
	devs := make([]deviceRow, 0, len(settings.Devices))
	for _, d := range settings.Devices {
		row := deviceRow{Device: d, Installs: map[string]store.Install{}, Effective: store.Effective(settings, d.ID)}
		if info, ok := s.Store.DeviceInfo(d.ID); ok {
			row.Info, row.Is64 = &info, abi.Is64(info.ABIs)
		}
		for _, e := range row.Effective {
			if in, ok := s.Store.Install(d.ID, e.Package); ok {
				row.Installs[e.Package] = in
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
	setRows := make([]setRow, 0, len(settings.Sets))
	for _, set := range settings.Sets {
		row := setRow{AppSet: set, Devices: []string{}}
		for _, d := range settings.Devices {
			if slices.Contains(d.Sets, set.ID) {
				row.Devices = append(row.Devices, d.ID)
			}
		}
		setRows = append(setRows, row)
	}
	out["sets"] = setRows
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

// validateChoices checks app choices against the loaded patch sources and returns them
// normalised (known source, one per package, sorted).
func (s *Server) validateChoices(in []store.AppChoice) (apps []store.AppChoice, code int, msg string) {
	lists := s.Backend.Lists()
	for i := range in {
		c := &in[i]
		src, ok := s.Store.Source(c.Source) // an empty source means the first one
		if !ok {
			return nil, http.StatusBadRequest, "unknown patch source: " + c.Source
		}
		c.Source = src.ID
		l := lists[c.Source]
		if l == nil {
			return nil, http.StatusServiceUnavailable, "that patch source has not loaded yet; try again in a moment"
		}
		if _, ok := l.App(c.Package); !ok {
			return nil, http.StatusBadRequest, "unknown app for that patch source: " + c.Package
		}
	}
	norm, err := s.Store.Normalize(in)
	if err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}
	return norm, 0, ""
}

// conflict answers 409 with the apps that devices cannot run. It returns true when it answered.
func (s *Server) conflict(w http.ResponseWriter, r *http.Request, st store.Settings, deviceIDs []string) (unchecked []string, blocked bool) {
	problems, unchecked := s.Backend.Compat(r.Context(), st, deviceIDs)
	if len(problems) == 0 {
		return unchecked, false
	}
	names := map[string]bool{}
	for _, p := range problems {
		names[p.Device] = true
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":     fmt.Sprintf("not saved: %d app(s) cannot run on %d device(s)", len(problems), len(names)),
		"problems":  problems,
		"unchecked": unchecked,
	})
	return unchecked, true
}

func (s *Server) setApps(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Apps []store.AppChoice `json:"apps"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	apps, code, msg := s.validateChoices(in.Apps)
	if code != 0 {
		fail(w, code, msg)
		return
	}
	if !s.hasDevice(id) {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	preview := s.Store.Preview(func(st *store.Settings) {
		for i := range st.Devices {
			if st.Devices[i].ID == id {
				st.Devices[i].Apps = apps
			}
		}
	})
	unchecked, blocked := s.conflict(w, r, preview, []string{id})
	if blocked {
		return
	}
	if err := s.Store.SetApps(id, apps); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": len(apps), "unchecked": unchecked})
}

func (s *Server) hasDevice(id string) bool {
	for _, d := range s.Store.Settings().Devices {
		if d.ID == id {
			return true
		}
	}
	return false
}

func (s *Server) setDeviceSets(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Sets []string `json:"sets"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	if !s.hasDevice(id) {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	known := map[string]bool{}
	for _, set := range s.Store.Settings().Sets {
		known[set.ID] = true
	}
	for _, sid := range in.Sets {
		if !known[sid] {
			fail(w, http.StatusBadRequest, "no such set: "+sid)
			return
		}
	}
	preview := s.Store.Preview(func(st *store.Settings) {
		for i := range st.Devices {
			if st.Devices[i].ID == id {
				st.Devices[i].Sets = in.Sets
			}
		}
	})
	unchecked, blocked := s.conflict(w, r, preview, []string{id})
	if blocked {
		return
	}
	if err := s.Store.SetDeviceSets(id, in.Sets); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sets": len(in.Sets), "unchecked": unchecked})
}

type setBody struct {
	Name    string            `json:"name"`
	Apps    []store.AppChoice `json:"apps"`
	Devices *[]string         `json:"devices"`
}

func (s *Server) addSet(w http.ResponseWriter, r *http.Request) {
	var in setBody
	if !decode(w, r, &in) {
		return
	}
	apps, code, msg := s.validateChoices(in.Apps)
	if code != 0 {
		fail(w, code, msg)
		return
	}
	devices := []string{}
	if in.Devices != nil {
		devices = *in.Devices
	}
	preview := s.Store.Preview(func(st *store.Settings) {
		st.Sets = append(st.Sets, store.AppSet{ID: "\x00new", Name: in.Name, Apps: apps})
		_ = st.Assign("\x00new", devices)
	})
	unchecked, blocked := s.conflict(w, r, preview, devices)
	if blocked {
		return
	}
	set, err := s.Store.AddSet(in.Name, apps, devices)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Info("app set created", "set", set.ID, "apps", len(set.Apps), "devices", len(devices))
	writeJSON(w, http.StatusCreated, map[string]any{"set": set, "unchecked": unchecked})
}

func (s *Server) updateSet(w http.ResponseWriter, r *http.Request) {
	var in setBody
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	apps, code, msg := s.validateChoices(in.Apps)
	if code != 0 {
		fail(w, code, msg)
		return
	}
	cur := s.Store.Settings()
	found := false
	var using []string
	for _, set := range cur.Sets {
		found = found || set.ID == id
	}
	if !found {
		fail(w, http.StatusNotFound, "no such set")
		return
	}
	for _, d := range cur.Devices {
		if slices.Contains(d.Sets, id) {
			using = append(using, d.ID)
		}
	}
	affected := using // devices that use it now keep being checked, and so do newly added ones
	if in.Devices != nil {
		affected = slices.Concat(using, *in.Devices)
	}
	preview := s.Store.Preview(func(st *store.Settings) {
		for i := range st.Sets {
			if st.Sets[i].ID == id {
				st.Sets[i].Name, st.Sets[i].Apps = in.Name, apps
			}
		}
		if in.Devices != nil {
			_ = st.Assign(id, *in.Devices)
		}
	})
	still := affected
	if in.Devices != nil {
		still = *in.Devices // a device taken off the set no longer gets its apps
	}
	unchecked, blocked := s.conflict(w, r, preview, still)
	if blocked {
		return
	}
	if err := s.Store.UpdateSet(id, in.Name, apps, in.Devices); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Info("app set updated", "set", id, "apps", len(apps))
	writeJSON(w, http.StatusOK, map[string]any{"apps": len(apps), "unchecked": unchecked})
}

func (s *Server) removeSet(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RemoveSet(r.PathValue("id")); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	slog.Info("app set removed", "set", r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) addSource(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Repo       string `json:"repo"`
		Prerelease bool   `json:"prerelease"`
		AllowPhone bool   `json:"allow_phone"`
		Trust      bool   `json:"trust"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Repo = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(in.Repo, "https://github.com/"), "http://github.com/"))
	in.Repo = strings.TrimSuffix(in.Repo, "/")
	if !in.Trust {
		fail(w, http.StatusBadRequest, "tick the box to confirm you trust this repository: its patches run on this server and are installed on your TVs")
		return
	}
	if !store.ValidRepo(in.Repo) {
		fail(w, http.StatusBadRequest, "enter the repository as owner/name")
		return
	}
	if len(s.AllowedOwners) > 0 && !slices.Contains(s.AllowedOwners, strings.ToLower(store.RepoOwner(in.Repo))) {
		fail(w, http.StatusForbidden, "that owner is not in PATCH_SOURCES_ALLOWED_OWNERS ("+strings.Join(s.AllowedOwners, ", ")+")")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Backend.CheckRepo(ctx, in.Repo, in.Prerelease); err != nil {
		fail(w, http.StatusBadGateway, "could not use that repository: "+err.Error())
		return
	}
	src, err := s.Store.AddSource(in.Repo, in.Prerelease, in.AllowPhone)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Warn("patch source added", "repo", src.Repo, "allow_phone", src.AllowPhone, "prerelease", src.Prerelease)
	go s.Backend.RefreshLists(context.WithoutCancel(r.Context())) // keep going after the response is sent
	writeJSON(w, http.StatusCreated, src)
}

func (s *Server) removeSource(w http.ResponseWriter, r *http.Request) {
	removed, err := s.Store.RemoveSource(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	slog.Warn("patch source removed", "id", r.PathValue("id"), "apps_dropped", removed)
	writeJSON(w, http.StatusOK, map[string]int{"apps_dropped": removed})
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
	var dev store.Device
	for _, d := range s.Store.Settings().Devices {
		if d.ID == r.PathValue("id") {
			dev = d
		}
	}
	if dev.ID == "" {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	cpu, err := s.Backend.PairDevice(ctx, dev, in.PairPort, in.Code)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	msg := "paired with " + dev.Host
	if cpu {
		if info, ok := s.Store.DeviceInfo(dev.ID); ok {
			msg += " (" + strings.Join(info.ABIs, ", ") + ")"
		}
	} else {
		msg += ". Its CPU could not be read yet; that happens on the first run."
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
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
