// Package runner is one pass over every device: find the newest supported version of each
// chosen app, fetch and patch it once, install it where needed, and tell you what happened.
package runner

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/config"
	"github.com/sloccy/morphe-tv-patcher/internal/morphe"
	"github.com/sloccy/morphe-tv-patcher/internal/patches"
	"github.com/sloccy/morphe-tv-patcher/internal/sources"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

const day = 24 * time.Hour

// ADB is the part of the adb client the runner uses.
type ADB interface {
	Connect(ctx context.Context, host string, known ...int) (int, error)
	ABIs(ctx context.Context, serial string) ([]string, error)
	SDK(ctx context.Context, serial string) (int, error)
	InstalledVersion(ctx context.Context, serial, pkg string) (string, error)
	Install(ctx context.Context, serial, apk string) (bool, string)
	Uninstall(ctx context.Context, serial, pkg string) error
	Pair(ctx context.Context, host string, pairPort int, code string) error
}

// Deps are the outside world; tests replace them.
type Deps struct {
	Latest      func(ctx context.Context) (*patches.Release, error)
	Download    func(ctx context.Context, rel *patches.Release) (string, error)
	List        func(ctx context.Context, tag string) (*patches.List, error)
	CLIVersions func(ctx context.Context, mpp, pkg string) []string
	Fetch       func(ctx context.Context, a sources.App, sdk int) (*sources.Fetched, error)
	Patch       func(ctx context.Context, o morphe.Options) error
	PackageOf   func(path string) (string, error)
}

// Options select what a run does.
type Options struct {
	DeviceID string   // only this device ("" = all)
	Package  string   // only this app
	Packages []string // with NoDevice: the apps to patch
	Force    bool     // reinstall even if up to date
	DryRun   bool     // patch but do not install
	NoDevice bool     // do not touch adb at all (implies DryRun)
}

// Result is the outcome for one app on one device (or a device/run-level problem).
type Result struct {
	Device  string    `json:"device"`
	Package string    `json:"package,omitempty"`
	Name    string    `json:"name,omitempty"`
	OK      bool      `json:"ok"`
	Changed bool      `json:"changed"` // something was patched or installed
	Alert   bool      `json:"-"`       // worth a Telegram message
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Status is what the UI shows about runs.
type Status struct {
	Running        bool      `json:"running"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	PatchesVersion string    `json:"patches_version"`
	Results        []Result  `json:"results"`
}

// Runner holds the long-lived pieces.
type Runner struct {
	Cfg    config.Config
	Store  *store.Store
	ADB    ADB
	Deps   Deps
	Notify func(ctx context.Context, title, body string)
	Now    func() time.Time

	mu     sync.Mutex
	status Status
	list   *patches.List
	wake   chan Options
}

// New makes a Runner.
func New(cfg config.Config, st *store.Store, a ADB, d Deps, notify func(context.Context, string, string)) *Runner {
	return &Runner{Cfg: cfg, Store: st, ADB: a, Deps: d, Notify: notify, Now: time.Now, wake: make(chan Options, 1)}
}

// Status returns a copy of the current status.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Results = append([]Result{}, s.Results...)
	return s
}

// List returns the apps the current patch bundle supports (nil until first loaded).
func (r *Runner) List() *patches.List {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.list
}

// RefreshList loads the app list without doing a run, so the UI is useful straight away.
func (r *Runner) RefreshList(ctx context.Context) error {
	rel, err := r.Deps.Latest(ctx)
	if err != nil {
		return err
	}
	l, err := r.Deps.List(ctx, rel.Tag)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.list = l
	if r.status.PatchesVersion == "" {
		r.status.PatchesVersion = rel.Tag
	}
	r.mu.Unlock()
	return nil
}

// Pair registers this machine with a device's wireless debugging (one time).
func (r *Runner) Pair(ctx context.Context, host string, pairPort int, code string) error {
	return r.ADB.Pair(ctx, host, pairPort, code)
}

// Trigger asks the scheduler for a run now. It returns false if one is already queued.
func (r *Runner) Trigger(o Options) bool {
	select {
	case r.wake <- o:
		return true
	default:
		return false
	}
}

// Loop runs once at start-up, then every Interval or when triggered, until ctx ends.
// One goroutine, so a manual and a scheduled run can never overlap.
func (r *Runner) Loop(ctx context.Context) {
	opts := Options{}
	for {
		r.Run(ctx, opts)
		timer := time.NewTimer(r.Cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case opts = <-r.wake:
		case <-timer.C:
			opts = Options{}
		}
		timer.Stop()
	}
}

// Run does one pass and sends any alerts.
func (r *Runner) Run(ctx context.Context, o Options) []Result {
	r.mu.Lock()
	r.status.Running, r.status.StartedAt = true, r.Now()
	r.mu.Unlock()

	results := r.pass(ctx, o)

	var alerts []string
	failed := false
	for i := range results {
		results[i].At = r.Now()
		failed = failed || !results[i].OK
		if results[i].Alert {
			alerts = append(alerts, results[i].Message)
		}
	}
	r.mu.Lock()
	r.status.Running, r.status.FinishedAt, r.status.Results = false, r.Now(), results
	r.mu.Unlock()
	if len(alerts) > 0 && r.Notify != nil {
		title := "updated"
		if failed {
			title = "PROBLEMS"
		}
		r.Notify(ctx, title, strings.Join(alerts, "\n"))
	}
	return results
}

type conn struct {
	dev    store.Device
	serial string
	abis   []string
	sdk    int
}

func (r *Runner) pass(ctx context.Context, o Options) []Result {
	var out []Result
	fail := func(dev, msg string, alert bool) { out = append(out, Result{Device: dev, Message: msg, Alert: alert}) }

	rel, err := r.Deps.Latest(ctx)
	if err != nil {
		fail("", "could not look up the patches release: "+err.Error(), true)
		return out
	}
	mpp, err := r.Deps.Download(ctx, rel)
	if err != nil {
		fail("", "could not download the patches: "+err.Error(), true)
		return out
	}
	r.mu.Lock()
	r.status.PatchesVersion = rel.Tag
	r.mu.Unlock()
	list, err := r.Deps.List(ctx, rel.Tag)
	if err != nil {
		slog.Warn("patches-list.json unavailable; using the Morphe CLI's view only", "err", err)
		list = nil
	}
	if list != nil {
		r.mu.Lock()
		r.list = list
		r.mu.Unlock()
	}

	// connect
	var conns []conn
	settings := r.Store.Settings()
	if o.NoDevice {
		pkgs := o.Packages
		if len(pkgs) == 0 {
			seen := map[string]bool{}
			for _, d := range settings.Devices {
				for _, p := range d.Apps {
					if !seen[p] {
						seen[p] = true
						pkgs = append(pkgs, p)
					}
				}
			}
		}
		conns = append(conns, conn{dev: store.Device{ID: "", Name: "(no device)", Apps: pkgs}})
	} else {
		for _, d := range settings.Devices {
			if o.DeviceID != "" && d.ID != o.DeviceID {
				continue
			}
			if len(d.Apps) == 0 {
				continue
			}
			c, err := r.connect(ctx, d)
			if err != nil {
				alert := r.noteFailure("device:"+d.ID, err, false)
				fail(d.Name, fmt.Sprintf("%s (%s) is not reachable: %v", d.Name, d.Host, err), alert)
				continue
			}
			r.noteRecovered("device:" + d.ID)
			conns = append(conns, c)
		}
	}

	// which apps, on which devices
	byPkg := map[string][]*conn{}
	for i := range conns {
		for _, p := range conns[i].dev.Apps {
			if o.Package == "" || o.Package == p {
				byPkg[p] = append(byPkg[p], &conns[i])
			}
		}
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	for _, pkg := range pkgs {
		out = append(out, r.app(ctx, o, rel, mpp, list, pkg, byPkg[pkg])...)
	}
	if len(out) == 0 {
		out = append(out, Result{OK: true, Message: "nothing to do: no device has any apps selected"})
	}
	return out
}

func (r *Runner) connect(ctx context.Context, d store.Device) (conn, error) {
	var known []int
	if p := r.Store.Port(d.ID); p > 0 {
		known = append(known, p)
	}
	port, err := r.ADB.Connect(ctx, d.Host, known...)
	if err != nil {
		return conn{}, err
	}
	_ = r.Store.SetPort(d.ID, port)
	serial := fmt.Sprintf("%s:%d", d.Host, port)
	abis, err := r.ADB.ABIs(ctx, serial)
	if err != nil {
		return conn{}, err
	}
	sdk, _ := r.ADB.SDK(ctx, serial)
	return conn{dev: d, serial: serial, abis: abis, sdk: sdk}, nil
}

type patched struct {
	path, installedPkg string
	err                error
}

func (r *Runner) app(ctx context.Context, o Options, rel *patches.Release, mpp string, list *patches.List,
	pkg string, cs []*conn) []Result {
	var out []Result
	name := pkg
	var versions []string
	if list != nil {
		if a, ok := list.App(pkg); ok {
			name, versions = a.Name, a.Versions
		}
	}
	if r.Deps.CLIVersions != nil {
		versions = append(versions, r.Deps.CLIVersions(ctx, mpp, pkg)...)
	}
	newest := patches.Newest(versions)
	res := func(c *conn, ok bool, msg string) Result {
		return Result{Device: c.dev.Name, Package: pkg, Name: name, OK: ok, Message: msg}
	}
	if newest == "" {
		for _, c := range cs {
			out = append(out, res(c, false, fmt.Sprintf("%s: not supported by this patch bundle (%s)", name, rel.Tag)))
		}
		return out
	}

	// who needs it
	var need []*conn
	for _, c := range cs {
		in, has := r.Store.Install(c.dev.ID, pkg)
		if !o.Force && has && in.APKVersion == newest && in.PatchesVersion == rel.Tag && !o.NoDevice {
			ip := in.InstalledPackage
			if ip == "" {
				ip = pkg
			}
			if v, _ := r.ADB.InstalledVersion(ctx, c.serial, ip); v != "" {
				out = append(out, res(c, true, fmt.Sprintf("%s on %s: up to date (%s, patches %s)", name, c.dev.Name, newest, rel.Tag)))
				continue
			}
		}
		need = append(need, c)
	}
	if len(need) == 0 {
		return out
	}

	// fetch + verify once
	ov := r.Store.Override(pkg)
	sdk := 0
	for _, c := range need {
		if c.sdk > 0 && (sdk == 0 || c.sdk < sdk) {
			sdk = c.sdk
		}
	}
	fetched, err := r.Deps.Fetch(ctx, sources.App{Package: pkg, Name: name, Version: newest, Sources: ov.Sources,
		SignerPin: strings.ToLower(ov.SignerSHA256), SlugOverride: ov.APKMirror}, sdk)
	if err != nil {
		var fe *sources.FetchError
		fatal := errors.As(err, &fe) && fe.Fatal
		alert := r.noteFailure(pkg, err, fatal)
		n := r.Store.App(pkg).FetchFailures
		msg := fmt.Sprintf("%s: cannot get %s (%d cycle%s in a row): %v. Drop the .apkm into %s/ (newest supported version; no fallback to older)",
			name, newest, n, plural(n), err, filepath.Join(r.Cfg.Input(), pkg))
		for _, c := range need {
			x := res(c, false, msg)
			x.Alert = alert && c == need[0] // one message per app, not per device
			out = append(out, x)
		}
		return out
	}
	recovered := r.noteRecovered(pkg)

	// patch once per ABI set
	cache := map[string]*patched{}
	patchFor := func(c *conn) *patched {
		abis := append([]string{}, c.abis...)
		sort.Strings(abis)
		key := strings.Join(abis, ",")
		if p := cache[key]; p != nil {
			return p
		}
		p := &patched{}
		cache[key] = p
		suffix := ""
		if key != "" {
			h := fnv.New32a()
			_, _ = h.Write([]byte(key))
			suffix = "-" + strconv.FormatUint(uint64(h.Sum32()), 16)
		}
		p.path = filepath.Join(r.Cfg.Output(), fmt.Sprintf("%s-%s-%s%s.apk", pkg, safe(newest), rel.Tag, suffix))
		p.err = r.Deps.Patch(ctx, morphe.Options{Jar: r.Cfg.MorpheJar, MPP: mpp, Input: fetched.Path, Out: p.path,
			Keystore: r.Cfg.Keystore(), BytecodeMode: r.Cfg.BytecodeMode, StripLibs: c.abis,
			OptionsFile: ov.OptionsFile, Enable: ov.Enable, Disable: ov.Disable})
		p.installedPkg = pkg
		if p.err == nil {
			if ip, err := r.Deps.PackageOf(p.path); err == nil && ip != "" {
				p.installedPkg = ip
			}
		}
		return p
	}

	for _, c := range need {
		p := patchFor(c)
		if p.err != nil {
			x := res(c, false, fmt.Sprintf("%s: patching %s failed: %v", name, newest, p.err))
			x.Alert = true
			out = append(out, x)
			continue
		}
		via := "via " + fetched.Detail
		if o.NoDevice || o.DryRun {
			x := res(c, true, fmt.Sprintf("%s: patched %s %s (dry run, not installed) -> %s", name, newest, via, filepath.Base(p.path)))
			x.Changed = true
			out = append(out, x)
			continue
		}
		prev := ""
		if in, ok := r.Store.Install(c.dev.ID, pkg); ok {
			prev = in.APKVersion
		}
		if err := r.install(ctx, c, pkg, p, ov); err != nil {
			x := res(c, false, fmt.Sprintf("%s on %s: %v", name, c.dev.Name, err))
			x.Alert = true
			out = append(out, x)
			continue
		}
		_ = r.Store.SetInstall(c.dev.ID, pkg, store.Install{APKVersion: newest, PatchesVersion: rel.Tag,
			InstalledPackage: p.installedPkg, Source: fetched.Detail, InstalledAt: r.Now().Format(time.RFC3339)})
		if prev == "" {
			prev = "none"
		}
		msg := fmt.Sprintf("%s on %s: %s -> %s (patches %s) %s", name, c.dev.Name, prev, newest, rel.Tag, via)
		if recovered {
			msg = "recovered, " + msg
			recovered = false
		}
		x := res(c, true, msg)
		x.Changed, x.Alert = true, true
		out = append(out, x)
	}
	r.prune(pkg)
	return out
}

func (r *Runner) install(ctx context.Context, c *conn, pkg string, p *patched, ov store.Override) error {
	ok, msg := r.ADB.Install(ctx, c.serial, p.path)
	if ok {
		return nil
	}
	if strings.Contains(msg, "UPDATE_INCOMPATIBLE") {
		// signed with a different key than the installed copy (e.g. installed by Morphe Manager)
		if ov.AllowUninstall && p.installedPkg != "com.netflix.ninja" {
			if err := r.ADB.Uninstall(ctx, c.serial, p.installedPkg); err != nil {
				return err
			}
			if ok, msg = r.ADB.Install(ctx, c.serial, p.path); ok {
				return nil
			}
		} else {
			return fmt.Errorf("signature differs from the installed app. Uninstall %s on the TV (you will have to log in again) or set allow_uninstall for %s in settings.json", p.installedPkg, pkg)
		}
	}
	if len(msg) > 300 {
		msg = msg[len(msg)-300:]
	}
	return fmt.Errorf("install failed: %s", strings.TrimSpace(msg))
}

// noteFailure counts a failed cycle for key and reports whether to alert: only after
// FailuresBeforeAlert in a row (a wrong or tampered file alerts at once), then at most daily.
func (r *Runner) noteFailure(key string, err error, fatal bool) bool {
	now := r.Now()
	alert := false
	_ = r.Store.UpdateApp(key, func(a *store.AppState) {
		a.FetchFailures++
		msg := err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		a.LastFetchError = msg
		cooled := now.Unix()-a.LastFetchAlert > int64(day/time.Second)
		if cooled && (fatal || a.FetchFailures >= r.Cfg.FailuresBeforeAlert) {
			alert = true
			a.LastFetchAlert = now.Unix()
		}
	})
	return alert
}

// noteRecovered clears the failure counters and says whether an alert had gone out before.
func (r *Runner) noteRecovered(key string) bool {
	was := r.Store.App(key).LastFetchAlert != 0
	if r.Store.App(key).FetchFailures != 0 || was {
		_ = r.Store.UpdateApp(key, func(a *store.AppState) { a.FetchFailures, a.LastFetchError, a.LastFetchAlert = 0, "", 0 })
	}
	return was
}

// prune keeps the newest few patched files and bundles.
func (r *Runner) prune(pkg string) {
	keepNewest(r.Cfg.Output(), []string{pkg + "-*.apk", pkg + "-*.result.json"}, 4)
	keepNewest(r.Cfg.Cache(), []string{"*.apkm", "*.xapk", "*.apks"}, 6)
}

func keepNewest(dir string, patterns []string, keep int) {
	var files []string
	for _, p := range patterns {
		m, _ := filepath.Glob(filepath.Join(dir, p))
		files = append(files, m...)
	}
	sort.Slice(files, func(i, j int) bool { return mtime(files[i]).After(mtime(files[j])) })
	for _, f := range files[min(keep, len(files)):] {
		_ = os.Remove(f)
	}
}

func mtime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func safe(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, " ", "_"), "/", "_") }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
