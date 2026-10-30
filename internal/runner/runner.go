// Package runner is one pass over every device: find the newest supported version of each
// chosen app, fetch and patch it once, install it where needed, and tell you what happened.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/abi"
	"github.com/sloccy/morphe-tv-patcher/internal/adb"
	"github.com/sloccy/morphe-tv-patcher/internal/config"
	"github.com/sloccy/morphe-tv-patcher/internal/helper"
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
	Pull(ctx context.Context, serial, devicePath, localPath string) error
	Uninstall(ctx context.Context, serial, pkg string) error
	Shell(ctx context.Context, serial, cmd string) (string, error)
	Playing(ctx context.Context, serial string) (bool, error)
	Reachable(ctx context.Context, host string, known ...int) (int, bool)
	Pair(ctx context.Context, host string, pairPort int, code string) error
	PairingPort(ctx context.Context, host string) int
	ConnectedPort(ctx context.Context, host string) int
	TCPIP(ctx context.Context, serial string, port int) error
}

// Deps are the outside world; tests replace them. Patch sources are passed in so each
// repository gets its own release, bundle and app list.
type Deps struct {
	Latest   func(ctx context.Context, src store.Source) (*patches.Release, error)
	Download func(ctx context.Context, src store.Source, rel *patches.Release) (string, error)
	Apps     func(ctx context.Context, src store.Source, rel *patches.Release, mpp string) (*patches.List, error)
	// Cached returns the last bundle downloaded for a source, used when the repository cannot be
	// reached (or is gone) so existing setups keep working.
	Cached func(src store.Source) (*patches.Release, string, bool)
	// Probe asks which CPU builds an app version offers without downloading it.
	Probe func(ctx context.Context, src store.Source, pkg, name, version string) ([]string, error)
	// PausedUntil says when APKMirror lookups resume after it asked us to slow down (zero = not paused).
	PausedUntil func() time.Time
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
	Manual   bool     // started from the page (Run now): connect to the TVs and check what is really installed
	DryRun   bool     // patch but do not install
	NoDevice bool     // do not touch adb at all (implies DryRun)
	ABIs     []string // with NoDevice: the CPU list to build for (default 32-bit)
	Source   string   // with NoDevice: the patch source id (default: the default source)
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

// SourceStatus is the state of one patch source's latest release.
type SourceStatus struct {
	ID      string `json:"id"`
	Repo    string `json:"repo"`
	Tag     string `json:"tag"`
	Error   string `json:"error,omitempty"`   // the source cannot be used at all
	Warning string `json:"warning,omitempty"` // unreachable, running on its last good bundle
}

// Status is what the UI shows about runs.
type Status struct {
	Running    bool           `json:"running"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Sources    []SourceStatus `json:"sources"`
	Results    []Result       `json:"results"`
}

// Runner holds the long-lived pieces.
type Runner struct {
	Cfg    config.Config
	Store  *store.Store
	ADB    ADB
	Deps   Deps
	Notify func(ctx context.Context, title, body string)
	Now    func() time.Time

	mu        sync.Mutex
	status    Status
	lists     map[string]*patches.List // patch source id -> the apps its latest bundle supports
	listCache map[string]*patches.List // "<id>@<tag>@<repo>" -> parsed list, so a release is read once
	wake      chan Options
	learn     learner
}

// New makes a Runner.
func New(cfg config.Config, st *store.Store, a ADB, d Deps, notify func(context.Context, string, string)) *Runner {
	return &Runner{Cfg: cfg, Store: st, ADB: a, Deps: d, Notify: notify, Now: time.Now, wake: make(chan Options, 1),
		lists: map[string]*patches.List{}, listCache: map[string]*patches.List{}}
}

// Status returns a copy of the current status.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Results = append([]Result{}, s.Results...)
	s.Sources = append([]SourceStatus{}, s.Sources...)
	return s
}

// Lists returns the apps each patch source supports (source id -> list); sources that have not
// loaded yet are missing.
func (r *Runner) Lists() map[string]*patches.List {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]*patches.List, len(r.lists))
	maps.Copy(out, r.lists)
	return out
}

// loaded is one patch source's latest release, bundle and app list.
type loaded struct {
	src  store.Source
	rel  *patches.Release
	mpp  string
	list *patches.List
	err  error
	warn string // set when the repository was unreachable and the last good bundle is used
}

// loadSource resolves a source's latest release, downloads the bundle and reads its apps. The
// parsed list is cached per release, so a release is read once however often this runs.
func (r *Runner) loadSource(ctx context.Context, src store.Source) *loaded {
	ld := &loaded{src: src}
	setStatus := func() {
		st := SourceStatus{ID: src.ID, Repo: src.Repo}
		if ld.rel != nil {
			st.Tag = ld.rel.Tag
		}
		if ld.err != nil {
			st.Error = ld.err.Error()
		}
		st.Warning = ld.warn
		r.mu.Lock()
		defer r.mu.Unlock()
		replaced := false
		for i := range r.status.Sources {
			if r.status.Sources[i].ID == src.ID {
				r.status.Sources[i], replaced = st, true
			}
		}
		if !replaced {
			r.status.Sources = append(r.status.Sources, st)
		}
	}
	defer setStatus()
	var fetchErr error
	if ld.rel, fetchErr = r.Deps.Latest(ctx, src); fetchErr != nil {
		fetchErr = fmt.Errorf("could not look up the latest release: %w", fetchErr)
	} else if ld.mpp, fetchErr = r.Deps.Download(ctx, src, ld.rel); fetchErr != nil {
		fetchErr = fmt.Errorf("could not download the patches: %w", fetchErr)
	}
	if fetchErr != nil {
		// the repository may be down, renamed or gone: keep working with the last good bundle
		cached, path, ok := (*patches.Release)(nil), "", false
		if r.Deps.Cached != nil {
			cached, path, ok = r.Deps.Cached(src)
		}
		if !ok {
			ld.rel, ld.err = nil, fetchErr
			return ld
		}
		ld.rel, ld.mpp = cached, path
		ld.warn = fmt.Sprintf("%v. Using the last good bundle (%s)", fetchErr, cached.Tag)
	}
	key := src.ID + "@" + ld.rel.Tag + "@" + src.Repo
	r.mu.Lock()
	cached := r.listCache[key]
	r.mu.Unlock()
	if cached == nil {
		if cached, ld.err = r.Deps.Apps(ctx, src, ld.rel, ld.mpp); ld.err != nil {
			ld.err = fmt.Errorf("could not read the patches: %w", ld.err)
			return ld
		}
		r.mu.Lock()
		r.listCache[key] = cached
		r.mu.Unlock()
	}
	ld.list = cached
	r.mu.Lock()
	r.lists[src.ID] = cached
	r.mu.Unlock()
	return ld
}

// RefreshLists loads every configured source so the page can list their apps straight away.
func (r *Runner) RefreshLists(ctx context.Context) {
	for _, src := range r.Store.Settings().Sources {
		if ld := r.loadSource(ctx, src); ld.err != nil {
			slog.Warn("patch source not loaded", "repo", src.Repo, "err", ld.err)
		}
	}
	r.LearnSelected(ctx)
}

// CheckRepo says whether a GitHub repository publishes a patch bundle, without saving anything.
func (r *Runner) CheckRepo(ctx context.Context, repo string, prerelease bool) error {
	rel, err := r.Deps.Latest(ctx, store.Source{Repo: repo, Prerelease: prerelease})
	if err != nil {
		return err
	}
	if rel.Size > patches.MaxBundleBytes {
		return fmt.Errorf("its bundle is %d MB, over the %d MB limit", rel.Size>>20, patches.MaxBundleBytes>>20)
	}
	return nil
}

// helperStep keeps the boot helper current on a reachable device and reports it as a result line
// only when it changed something or failed. It never stops the app updates.
func (r *Runner) helperStep(ctx context.Context, c *conn) []Result {
	res, err := r.ensureHelper(ctx, c)
	key := "helper:" + c.dev.ID
	line := Result{Device: c.dev.Name, Package: helper.Package, Name: "Boot helper"}
	switch {
	case err != nil:
		line.Message = "boot helper: " + err.Error()
		line.Alert = r.noteFailureAfter(key, err, false, min(2, r.Cfg.FailuresBeforeAlert))
		return []Result{line}
	case res.Installed:
		r.noteRecovered(key)
		line.OK, line.Changed = true, true
		line.Message = fmt.Sprintf("installed the boot helper (version %s); it switches Wireless debugging back on after the TV restarts", res.Version)
		return []Result{line}
	}
	r.noteRecovered(key)
	return nil
}

// DeviceCheck is what a successful connection test found.
type DeviceCheck struct {
	Port   int
	ABIs   []string
	SDK    int
	Helper string // "installed (version N)", "not part of this build", or why it failed
	Tweaks string // what was done about the TV settings, if anything
	// KeyExpiry is the TV's adb_allowed_connection_time; "0" means the pairing never expires.
	KeyExpiry string
}

// TestDevice connects to a device the way a run would (known port, mDNS, port scan) and reads
// its CPU. The error is plain advice the page can show as it is.
func (r *Runner) TestDevice(ctx context.Context, d store.Device) (DeviceCheck, error) {
	c, err := r.connect(ctx, d)
	if err != nil {
		if errors.Is(err, adb.ErrNotPaired) {
			return DeviceCheck{}, fmt.Errorf("wireless debugging is on, but this server is not paired with %s (the pairing may have been reset). Use Pair… again", d.Name)
		}
		return DeviceCheck{}, fmt.Errorf("could not reach %s at %s. The TV may be off or asleep, or Wireless debugging is switched off (a reboot does that): turn it on in Developer options. The pairing is kept", d.Name, d.Host)
	}
	r.noteRecovered("device:" + d.ID)
	chk := DeviceCheck{Port: r.Store.Port(d.ID), ABIs: c.abis, SDK: c.sdk}
	res, err := r.ensureHelper(ctx, &c)
	chk.KeyExpiry = res.KeyExpiry
	switch {
	case err != nil:
		chk.Helper = "problem: " + err.Error()
	case res.Skipped:
		chk.Helper = "not part of this build"
	case res.Installed:
		chk.Helper = "installed just now (version " + res.Version + ")"
	default:
		chk.Helper = "installed (version " + res.Version + ")"
	}
	// Last, because pinning the port restarts adbd and drops this connection.
	for _, l := range r.fixedPortStep(ctx, &c, true) {
		chk.Tweaks = l.Message
	}
	return chk, nil
}

// PairDevice registers this machine with a device's wireless debugging (one time), notes when
// it was paired, and then reads the device's CPU so the page can use it straight away. Reading
// the CPU is best effort: the pairing itself already worked if it fails.
func (r *Runner) PairDevice(ctx context.Context, d store.Device, pairPort int, code string) (cpuKnown bool, err error) {
	if err := r.ADB.Pair(ctx, d.Host, pairPort, code); err != nil {
		return false, err
	}
	_ = r.Store.UpdateDeviceInfo(d.ID, func(i *store.DeviceInfo) { i.PairedAt = r.Now().Format(time.RFC3339) })
	// Pairing leaves the adb server knowing the TV's current port, which is almost never the one
	// on record: without this the next connection dials the old one and looks like a dead TV.
	if p := r.ADB.ConnectedPort(ctx, d.Host); p > 0 {
		_ = r.Store.SetPort(d.ID, p)
	}
	cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if _, err := r.connect(cctx, d); err != nil {
		slog.Info("paired, but could not read the device yet", "device", d.Name, "err", err)
		return false, nil
	}
	return true, nil
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
	apps   []store.AppChoice // what this device should have (its sets plus its own extras)
	serial string
	abis   []string
	sdk    int
	// connected is false while the run is planning from what is on record. The TV is only
	// connected to (ensureConn) when something needs it, because every ADB session makes the
	// TV show a notice.
	connected bool
	connErr   error    // why connecting failed this run, so it is tried and reported once
	reported  bool     // the unreachable message has been given for this device
	extra     []Result // lines from the connection-time steps (boot helper, TV settings)
}

// keyExpirySafety is how long tv-patcher lets a TV go untouched while Android is still counting
// down to dropping this server's pairing key. Android deletes a key that has not connected for
// adb_allowed_connection_time, 7 days by default, so a TV whose firmware would not take the
// helper's "never expire" is connected to well inside that week rather than on its edge.
const keyExpirySafety = 120 * time.Hour

// verifyDue says whether a scheduled run should connect to this TV just to check what is
// really installed: when it has not been reached for VerifyEvery, or never. A TV that has not
// confirmed its pairing keys never expire is reached sooner, whatever VerifyEvery says.
func (r *Runner) verifyDue(info store.DeviceInfo, known bool) bool {
	if !known || len(info.ABIs) == 0 {
		return true
	}
	every := r.Cfg.VerifyEvery
	if info.KeyExpiry != "0" && every > keyExpirySafety {
		every = keyExpirySafety
	}
	seen, err := time.Parse(time.RFC3339, info.SeenAt)
	return err != nil || r.Now().Sub(seen) > every
}

// ensureConn connects to the TV once per run, and then runs the steps that belong to a
// connection: keeping the boot helper current and the TV settings applied.
func (r *Runner) ensureConn(ctx context.Context, c *conn) error {
	if c.connected {
		return nil
	}
	if c.connErr != nil {
		return c.connErr
	}
	got, err := r.connect(ctx, c.dev)
	if err != nil {
		c.connErr = err
		return err
	}
	c.serial, c.abis, c.sdk, c.connected = got.serial, got.abis, got.sdk, true
	r.noteRecovered("device:" + c.dev.ID)
	c.extra = append(c.extra, r.helperStep(ctx, c)...)
	c.extra = append(c.extra, r.fixedPortStep(ctx, c, false)...)
	return nil
}

// unreachable is the result line for a TV that could not be connected to.
func (r *Runner) unreachable(d store.Device, err error) Result {
	// Losing the pairing needs somebody at the TV, so it is said at once and not dressed up as a
	// TV that might just be asleep.
	if errors.Is(err, adb.ErrNotPaired) {
		return Result{Device: d.Name, Alert: r.noteFailureAfter("device:"+d.ID, err, false, 1),
			Message: fmt.Sprintf("%s (%s) answers but no longer accepts this server: pair it again from the page (Pair…). Android drops a pairing key it has not seen for a week, which the boot helper switches off once it is installed.", d.Name, d.Host)}
	}
	// a TV that stays unreachable usually has Wireless debugging switched off (a reboot does that),
	// so say so after two cycles in a row rather than waiting for the usual three
	alert := r.noteFailureAfter("device:"+d.ID, err, false, min(2, r.Cfg.FailuresBeforeAlert))
	return Result{Device: d.Name, Alert: alert, Message: fmt.Sprintf("%s (%s) is not reachable: %v. If the TV restarted, turn Wireless debugging back on in Developer options (the pairing is kept; only the switch resets).", d.Name, d.Host, err)}
}

// appKey is one app from one patch source; devices that picked the same pair share the work.
type appKey struct{ pkg, src string }

func (r *Runner) pass(ctx context.Context, o Options) []Result {
	var out []Result

	// connect
	var conns []conn
	settings := r.Store.Settings()
	if o.NoDevice {
		src := o.Source // empty means the first patch source
		var choices []store.AppChoice
		if len(o.Packages) > 0 {
			for _, p := range o.Packages {
				choices = append(choices, store.AppChoice{Package: p, Source: src})
			}
		} else {
			seen := map[string]bool{}
			for _, d := range settings.Devices {
				for _, e := range store.Effective(settings, d.ID) {
					if !seen[e.Package+"\x00"+e.Source] {
						seen[e.Package+"\x00"+e.Source] = true
						choices = append(choices, e.AppChoice)
					}
				}
			}
		}
		conns = append(conns, conn{dev: store.Device{ID: "", Name: "(no device)"}, apps: choices, abis: abi.Normalize(o.ABIs)})
	} else {
		for _, d := range settings.Devices {
			if o.DeviceID != "" && d.ID != o.DeviceID {
				continue
			}
			var apps []store.AppChoice
			for _, e := range store.Effective(settings, d.ID) {
				apps = append(apps, e.AppChoice)
			}
			if len(apps) == 0 {
				continue
			}
			info, known := r.Store.DeviceInfo(d.ID)
			c := conn{dev: d, abis: info.ABIs, sdk: info.SDK}
			// Plan from what is on record. Connect now only for a manual run, a forced one, a TV
			// not yet seen for VerifyEvery (or never); otherwise only when an install needs it.
			if o.Manual || o.Force || r.verifyDue(info, known) {
				if err := r.ensureConn(ctx, &c); err != nil {
					out = append(out, r.unreachable(d, err))
					continue
				}
			}
			c.apps = apps
			conns = append(conns, c)
		}
	}

	// which apps, from which patch source, on which devices
	byApp := map[appKey][]*conn{}
	for i := range conns {
		for _, c := range conns[i].apps {
			if o.Package == "" || o.Package == c.Package {
				id := c.Source
				if src, ok := r.Store.Source(id); ok {
					id = src.ID // an empty source means the first one
				}
				k := appKey{c.Package, id}
				byApp[k] = append(byApp[k], &conns[i])
			}
		}
	}
	keys := make([]appKey, 0, len(byApp))
	for k := range byApp {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pkg != keys[j].pkg {
			return keys[i].pkg < keys[j].pkg
		}
		return keys[i].src < keys[j].src
	})

	// load each patch source that is actually used, once
	srcs := map[string]*loaded{}
	for _, k := range keys {
		if _, done := srcs[k.src]; done {
			continue
		}
		src, ok := r.Store.Source(k.src)
		if !ok {
			srcs[k.src] = &loaded{src: store.Source{ID: k.src}, err: errors.New("this patch source was removed")}
			continue
		}
		srcs[k.src] = r.loadSource(ctx, src)
	}

	warned := map[string]bool{}
	for _, k := range keys {
		ld := srcs[k.src]
		if ld.err != nil {
			alert := r.noteFailure("source:"+k.src, ld.err, false)
			for i, c := range byApp[k] {
				out = append(out, Result{Device: c.dev.Name, Package: k.pkg, Name: patches.PrettyName(k.pkg), OK: false, Alert: alert && i == 0,
					Message: fmt.Sprintf("%s: patch source %s unavailable: %v", patches.PrettyName(k.pkg), ld.src.Repo, ld.err)})
			}
			continue
		}
		if ld.warn != "" {
			// running on the cached bundle: say so (once the failure repeats), then carry on
			if r.noteFailure("source:"+k.src, errors.New(ld.warn), false) && !warned[k.src] {
				warned[k.src] = true
				out = append(out, Result{OK: false, Alert: true, Message: fmt.Sprintf("patch source %s is unreachable and its apps are running on a cached bundle: %s. If the repository is gone for good, remove it on the page.", ld.src.Repo, ld.warn)})
			}
		} else {
			r.noteRecovered("source:" + k.src)
		}
		out = append(out, r.app(ctx, o, ld, k.pkg, byApp[k])...)
	}
	for i := range conns {
		out = append(out, conns[i].extra...)
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
	if p := r.Cfg.TVFixedADBPort; p > 0 && !slices.Contains(known, p) {
		known = append(known, p)
	}
	port, err := r.ADB.Connect(ctx, d.Host, known...)
	if err != nil {
		// Wireless debugging picks a new port every time Android switches it back on, so a
		// remembered one that has gone must be forgotten: kept, it is dialled first forever.
		if errors.Is(err, adb.ErrUnreachable) {
			_ = r.Store.SetPort(d.ID, 0)
		}
		return conn{}, err
	}
	_ = r.Store.SetPort(d.ID, port)
	serial := fmt.Sprintf("%s:%d", d.Host, port)
	abis, err := r.ADB.ABIs(ctx, serial)
	if err != nil {
		return conn{}, err
	}
	sdk, _ := r.ADB.SDK(ctx, serial)
	_ = r.Store.SetDeviceInfo(d.ID, store.DeviceInfo{ABIs: abis, SDK: sdk, SeenAt: r.Now().Format(time.RFC3339)})
	return conn{dev: d, serial: serial, abis: abis, sdk: sdk}, nil
}

type patched struct {
	path, installedPkg string
	err                error
}

func (r *Runner) app(ctx context.Context, o Options, ld *loaded, pkg string, cs []*conn) []Result {
	rel, src := ld.rel, ld.src
	var out []Result
	name := pkg
	newest := ""
	if a, ok := ld.list.App(pkg); ok {
		name, newest = a.Name, a.Target
	}
	res := func(c *conn, ok bool, msg string) Result {
		return Result{Device: c.dev.Name, Package: pkg, Name: name, OK: ok, Message: msg}
	}
	if newest == "" {
		for _, c := range cs {
			out = append(out, res(c, false, fmt.Sprintf("%s: not supported by %s %s", name, src.Repo, rel.Tag)))
		}
		return out
	}

	// who needs it
	var need []*conn
	for _, c := range cs {
		in, has := r.Store.Install(c.dev.ID, pkg)
		if !o.Force && has && in.APKVersion == newest && in.PatchesVersion == rel.Tag && !o.NoDevice &&
			in.PatchSource == src.ID {
			ip := in.InstalledPackage
			if ip == "" {
				ip = pkg
			}
			if !c.connected { // not connected this run: the record is what we go by
				out = append(out, res(c, true, fmt.Sprintf("%s on %s: up to date (%s, patches %s)", name, c.dev.Name, newest, rel.Tag)))
				continue
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

	// Devices with the same CPU list share one fetch and one patched file; a device with a
	// different list (a 64-bit box next to a 32-bit one) gets its own build.
	ov := r.Store.Override(pkg)
	groups := map[string][]*conn{}
	var keys []string
	for _, c := range need {
		k := abi.Key(c.abis)
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], c)
	}
	sort.Strings(keys)
	recovered := false
	for _, key := range keys {
		gc := groups[key]
		out = append(out, r.appGroup(ctx, o, ld, pkg, name, newest, ov, gc, key, &recovered)...)
	}
	r.prune(pkg)
	return out
}

// appGroup fetches, patches and installs one app for devices that share a CPU list.
func (r *Runner) appGroup(ctx context.Context, o Options, ld *loaded, pkg, name, newest string,
	ov store.Override, gc []*conn, key string, recovered *bool) []Result {
	rel, mpp, src := ld.rel, ld.mpp, ld.src
	var out []Result
	res := func(c *conn, ok bool, msg string) Result {
		return Result{Device: c.dev.Name, Package: pkg, Name: name, OK: ok, Message: msg}
	}
	abis := gc[0].abis
	sdk := 0
	for _, c := range gc {
		if c.sdk > 0 && (sdk == 0 || c.sdk < sdk) {
			sdk = c.sdk
		}
	}
	fetched, err := r.Deps.Fetch(ctx, sources.App{Package: pkg, Name: name, Version: newest, Sources: ov.Sources,
		SignerPin: strings.ToLower(ov.SignerSHA256), SlugOverride: ov.APKMirror, ABIs: abis, AllowPhone: src.AllowPhone}, sdk)
	if err != nil {
		fe, _ := errors.AsType[*sources.FetchError](err)
		if fe != nil {
			_ = r.Store.SetBuilds(pkg, newest, fe.Available)
		}
		if fe != nil && fe.NoBuild {
			// the release exists, but not for this CPU: say so once, it is not a download failure
			alert := r.noteNoBuild(pkg, newest, key)
			msg := fmt.Sprintf("%s: no build of %s runs on %s (%s). The app or its patches only offer other CPU architectures",
				name, newest, gc[0].dev.Name, strings.Join(abis, ","))
			for i, c := range gc {
				x := res(c, false, msg)
				x.Alert = alert && i == 0
				out = append(out, x)
			}
			return out
		}
		alert := r.noteFailure(pkg, err, fe != nil && fe.Fatal)
		n := r.Store.App(pkg).FetchFailures
		msg := fmt.Sprintf("%s: cannot get %s (%d cycle%s in a row): %v. Drop the .apkm into %s/ (newest supported version; no fallback to older)",
			name, newest, n, plural(n), err, filepath.Join(r.Cfg.Input(), pkg))
		for i, c := range gc {
			x := res(c, false, msg)
			x.Alert = alert && i == 0 // one message per app and CPU group, not per device
			out = append(out, x)
		}
		return out
	}
	_ = r.Store.SetBuilds(pkg, newest, fetched.Available)
	if filepath.Dir(fetched.Path) == filepath.Clean(r.Cfg.Cache()) {
		_ = r.Store.UpdateApp(pkg, func(a *store.AppState) {
			if a.CacheFiles == nil {
				a.CacheFiles = map[string]string{}
			}
			a.CacheFiles[fetched.Path] = newest
		})
	}
	if r.noteRecovered(pkg) {
		*recovered = true
	}

	// Patch once for this CPU list, unless this very build was made before and its result is
	// still on disk. id identifies what came out, so the devices below can tell whether they
	// already have it.
	out2 := r.outputPath(pkg, newest, rel.Tag, key, src.ID)
	bk := r.buildKey(pkg, newest, src.ID, mpp, key, fetched.Path, ov)
	var patchErr error
	id := ""
	if !o.Force {
		id = r.reuse(pkg, bk, out2)
	}
	if id != "" {
		slog.Info("reusing the patched apk already built from these inputs", "package", pkg,
			"version", newest, "file", filepath.Base(out2))
	} else {
		patchErr = r.Deps.Patch(ctx, morphe.Options{Jar: r.Cfg.MorpheJar, MPP: mpp, Input: fetched.Path, Out: out2,
			Keystore: r.Cfg.Keystore(), BytecodeMode: r.Cfg.BytecodeMode,
			OptionsFile: ov.OptionsFile, Enable: ov.Enable, Disable: ov.Disable})
		if patchErr == nil {
			if id = contentID(pkg, out2); id != "" {
				_ = r.Store.SetPatchedID(pkg, bk, id)
			}
		}
	}
	p := &patched{path: out2, err: patchErr, installedPkg: pkg}
	if patchErr == nil {
		// A clone patch legitimately renames the package, but the name comes from the patched
		// APK's own manifest and ends up in adb command lines and file paths, so take it only
		// when it really looks like a package name.
		if ip, err := r.Deps.PackageOf(out2); err == nil && ip != "" {
			if store.ValidPackage(ip) {
				p.installedPkg = ip
			} else {
				slog.Warn("patched apk declares an implausible package name; keeping the original",
					"package", pkg, "declared", ip)
			}
		}
	}

	for _, c := range gc {
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
		prev, cur := "", store.Install{}
		if in, ok := r.Store.Install(c.dev.ID, pkg); ok {
			prev, cur = in.APKVersion, in
		}
		// This release changes nothing for this app: the rebuild is the very APK the device
		// already has. Record the new release and leave the TV alone, so no connection is made,
		// nothing shows on the screen and the app is not restarted. A run that is connected to the
		// TV anyway asks it first, the way the up-to-date check does, so Run now still notices an
		// app that was removed by hand.
		if !o.Force && id != "" && cur.ContentID == id && cur.APKVersion == newest && r.stillThere(ctx, c, cur, pkg) {
			cur.PatchesVersion, cur.PatchSource = rel.Tag, src.ID
			_ = r.Store.SetInstall(c.dev.ID, pkg, cur)
			msg := fmt.Sprintf("%s on %s: patches %s change nothing for it, keeping the installed %s",
				name, c.dev.Name, rel.Tag, newest)
			x := res(c, true, msg)
			if *recovered { // or the news that the download problem is over never goes out
				x.Message, x.Alert, *recovered = "recovered, "+msg, true, false
			}
			out = append(out, x)
			continue
		}
		// Nothing playing on the TV may be interrupted by an install; the next scheduled run tries
		// again. Run now, Force and dry runs go ahead regardless. This is asked only once it is
		// clear the device really needs the new build, so a release that changes nothing for an app
		// never reaches the TV at all.
		if !o.Manual && !o.Force && r.playing(ctx, c) {
			out = append(out, res(c, true, fmt.Sprintf("%s on %s: update to %s is waiting, something is playing; the next scheduled run will try again", name, c.dev.Name, newest)))
			continue
		}
		if err := r.ensureConn(ctx, c); err != nil {
			x := Result{Device: c.dev.Name, Message: fmt.Sprintf("%s on %s: not installed, the TV is not reachable", name, c.dev.Name)}
			if !c.reported { // the explanation and the alert once per TV, not once per app
				x, c.reported = r.unreachable(c.dev, err), true
			}
			x.Package, x.Name = pkg, name
			out = append(out, x)
			continue
		}
		if err := r.install(ctx, c, pkg, p, ov); err != nil {
			x := res(c, false, fmt.Sprintf("%s on %s: %v", name, c.dev.Name, err))
			x.Alert = true
			out = append(out, x)
			continue
		}
		_ = r.Store.SetInstall(c.dev.ID, pkg, store.Install{APKVersion: newest, PatchesVersion: rel.Tag, PatchSource: src.ID,
			InstalledPackage: p.installedPkg, Source: fetched.Detail, InstalledAt: r.Now().Format(time.RFC3339),
			ContentID: id})
		r.dropOld(pkg, newest)
		if prev == "" {
			prev = "none"
		}
		msg := fmt.Sprintf("%s on %s: %s -> %s (patches %s) %s", name, c.dev.Name, prev, newest, rel.Tag, via)
		if *recovered {
			msg = "recovered, " + msg
			*recovered = false
		}
		x := res(c, true, msg)
		x.Changed, x.Alert = true, true
		out = append(out, x)
	}
	return out
}

// playing connects to the TV and says whether something is playing on it. A TV that cannot be
// reached, or a check that fails, counts as idle so the normal flow reports the problem and an
// unreliable check can never hold updates back for good.
func (r *Runner) playing(ctx context.Context, c *conn) bool {
	if err := r.ensureConn(ctx, c); err != nil {
		return false
	}
	busy, err := r.ADB.Playing(ctx, c.serial)
	if err != nil {
		slog.Warn("could not tell whether the TV is playing", "device", c.dev.Name, "err", err)
		return false
	}
	return busy
}

// noteNoBuild records that a version has no build for a CPU list and reports whether this is
// the first time (so the message goes out once, not every cycle).
func (r *Runner) noteNoBuild(pkg, version, key string) bool {
	k := "nobuild:" + pkg + ":" + version + ":" + key
	first := r.Store.App(k).LastFetchAlert == 0
	if first {
		now := r.Now().Unix()
		_ = r.Store.UpdateApp(k, func(a *store.AppState) { a.LastFetchAlert = now })
	}
	return first
}

func (r *Runner) install(ctx context.Context, c *conn, pkg string, p *patched, ov store.Override) error {
	ok, msg := r.ADB.Install(ctx, c.serial, p.path)
	if ok {
		return nil
	}
	why := ""
	switch {
	case strings.Contains(msg, "UPDATE_INCOMPATIBLE"):
		// signed with a different key than the installed copy (e.g. installed by Morphe Manager)
		why = "signature differs from the installed app"
	case strings.Contains(msg, "VERSION_DOWNGRADE"):
		// Android only goes back to an older version of a non-debuggable app by uninstalling first
		why = "the installed app is newer than this build, and Android can only go back by uninstalling it"
	}
	if why != "" {
		if ov.CanUninstall() && p.installedPkg != "com.netflix.ninja" {
			if err := r.ADB.Uninstall(ctx, c.serial, p.installedPkg); err != nil {
				return err
			}
			if ok, msg = r.ADB.Install(ctx, c.serial, p.path); ok {
				return nil
			}
		} else {
			return fmt.Errorf("%s. Uninstall %s on the TV (you will have to log in again) or remove allow_uninstall=false for %s in settings.json", why, p.installedPkg, pkg)
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
	return r.noteFailureAfter(key, err, fatal, r.Cfg.FailuresBeforeAlert)
}

// noteFailureAfter is noteFailure with its own number of failed cycles before the alert.
func (r *Runner) noteFailureAfter(key string, err error, fatal bool, after int) bool {
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
		if cooled && (fatal || a.FetchFailures >= after) {
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

// dropOld runs after an app was installed at version: the downloaded bundles for older versions
// (that no patch source still targets) and the patched files for any other version are no longer needed, so they are deleted. Files you dropped into the input
// folder are never touched.
func (r *Runner) dropOld(pkg, version string) {
	cache := filepath.Clean(r.Cfg.Cache())
	// another patch source may still aim at a different version of this app
	wanted := map[string]bool{version: true}
	for _, l := range r.Lists() {
		if a, ok := l.App(pkg); ok {
			wanted[a.Target] = true
		}
	}
	_ = r.Store.UpdateApp(pkg, func(a *store.AppState) {
		for p, v := range a.CacheFiles {
			if wanted[v] || patches.VersionLess(version, v) { // needed, or newer than what was installed
				if _, err := os.Stat(p); err != nil {
					delete(a.CacheFiles, p) // already gone
				}
				continue
			}
			if filepath.Dir(p) == cache {
				_ = os.Remove(p)
			}
			delete(a.CacheFiles, p)
		}
		// a remembered build of a version that is gone can never be reused again
		for k := range a.Patched {
			v, _, ok := strings.Cut(k, "|")
			if !ok || (!wanted[v] && !patches.VersionLess(version, v)) {
				delete(a.Patched, k)
			}
		}
	})
	keep := pkg + "-" + safe(version) + "-"
	for _, pat := range []string{pkg + "-*.apk", pkg + "-*.result.json"} {
		old, _ := filepath.Glob(filepath.Join(r.Cfg.Output(), pat))
		for _, f := range old {
			if !strings.HasPrefix(filepath.Base(f), keep) {
				_ = os.Remove(f)
			}
		}
	}
}

// prune keeps the newest few patched files and bundles. Each kind is counted on its own: with
// both in one budget an app built for several CPU lists lost one of its APKs to the result files
// of another, and with it the chance to reuse that build.
func (r *Runner) prune(pkg string) {
	keepNewest(r.Cfg.Output(), []string{pkg + "-*.apk"}, 4)
	keepNewest(r.Cfg.Output(), []string{pkg + "-*.result.json"}, 4)
	r.pruneCache()
}

// cacheMaxAge is how long a downloaded bundle that nothing on record points to is kept.
const cacheMaxAge = 30 * day

// pruneCache removes only bundles that cannot be needed again. A downloaded bundle is never
// deleted just because the cache is "full": every app's current bundle stays, so a new patches
// release or a new TV never means downloading it from APKMirror a second time. Bundles recorded
// for an app are removed by dropOld once a newer version is installed. A file nothing records
// (from before that tracking) stays while its name carries an installed version, and otherwise
// goes after cacheMaxAge.
func (r *Runner) pruneCache() {
	tracked := r.Store.CacheFileSet()
	versions := r.Store.InstalledVersions()
	installed := make([]string, 0, len(versions))
	for _, v := range versions {
		installed = append(installed, "-"+patches.Slug(v)+"-")
	}
	entries, _ := os.ReadDir(r.Cfg.Cache())
	for _, en := range entries {
		switch filepath.Ext(en.Name()) {
		case ".apkm", ".xapk", ".apks", ".apk":
		default:
			continue
		}
		p := filepath.Join(r.Cfg.Cache(), en.Name())
		if en.IsDir() || tracked[p] || r.Now().Sub(mtime(p)) < cacheMaxAge {
			continue
		}
		if slices.ContainsFunc(installed, func(v string) bool { return strings.Contains(en.Name(), v) }) {
			continue
		}
		_ = os.Remove(p)
	}
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
