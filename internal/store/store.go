// Package store keeps everything that must survive restarts: the devices and apps you chose
// (settings.json), what has been installed and what we learned about each app (state.json),
// and the last working wireless-debugging port per device. All in one data volume.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Source is a GitHub repository that publishes Morphe patch bundles (.mpp release assets).
type Source struct {
	ID         string `json:"id"`
	Repo       string `json:"repo"` // owner/name
	Prerelease bool   `json:"prerelease,omitempty"`
	// AllowPhone accepts phone builds that have no Android TV launcher (they may install but
	// not show in the TV launcher). Off unless the repository patches phone apps.
	AllowPhone bool `json:"allow_phone,omitempty"`
}

// AppChoice is one app on a device and which patch source supplies its patches.
type AppChoice struct {
	Package string `json:"package"`
	Source  string `json:"source"`
}

// UnmarshalJSON also accepts a plain package-name string (the first patch source is used).
func (a *AppChoice) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*a = AppChoice{Package: s}
		return nil
	}
	type plain AppChoice
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*a = AppChoice(p)
	return nil
}

// Device is a target Android TV / box.
type Device struct {
	ID   string      `json:"id"`
	Name string      `json:"name"`
	Host string      `json:"host"`
	Apps []AppChoice `json:"apps"`           // this device's own extra apps (on top of its sets)
	Sets []string    `json:"sets,omitempty"` // ids of the app sets it uses, in order
}

// AppSet is a named list of apps that can be given to several devices at once.
type AppSet struct {
	ID   string      `json:"id"`
	Name string      `json:"name"`
	Apps []AppChoice `json:"apps"`
}

// EffectiveApp is an app a device should have, and where that choice comes from.
type EffectiveApp struct {
	AppChoice
	Via string `json:"via"` // "device" for its own extras, otherwise "set:<id>"
}

// Effective works out the apps a device gets from a settings snapshot: its own extras first
// (they win), then each of its sets in order. One choice per package, the first one wins;
// sorted by package. Unknown set ids are ignored.
func Effective(st Settings, deviceID string) []EffectiveApp {
	var d *Device
	for i := range st.Devices {
		if st.Devices[i].ID == deviceID {
			d = &st.Devices[i]
		}
	}
	if d == nil {
		return nil
	}
	first := ""
	if len(st.Sources) > 0 {
		first = st.Sources[0].ID
	}
	seen := map[string]bool{}
	var out []EffectiveApp
	add := func(c AppChoice, via string) {
		if c.Source == "" {
			c.Source = first
		}
		if !seen[c.Package] {
			seen[c.Package] = true
			out = append(out, EffectiveApp{AppChoice: c, Via: via})
		}
	}
	for _, c := range d.Apps {
		add(c, "device")
	}
	for _, id := range d.Sets {
		for _, set := range st.Sets {
			if set.ID == id {
				for _, c := range set.Apps {
					add(c, "set:"+set.ID)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	return out
}

// Override is a per-app tweak edited by hand in settings.json (the UI never touches it).
type Override struct {
	APKMirror      string   `json:"apkmirror,omitempty"` // publisher/app path, if detection picks wrongly
	Sources        []string `json:"sources,omitempty"`   // "apkmirror" (the only network source)
	SignerSHA256   string   `json:"signer_sha256,omitempty"`
	Enable         []string `json:"enable,omitempty"` // Morphe patch names
	Disable        []string `json:"disable,omitempty"`
	OptionsFile    string   `json:"options_file,omitempty"`
	AllowUninstall *bool    `json:"allow_uninstall,omitempty"` // default true; false opts an app out
}

// CanUninstall says whether tv-patcher may uninstall the app to get past a signature mismatch or
// a downgrade. On unless the override sets allow_uninstall to false.
func (o Override) CanUninstall() bool { return o.AllowUninstall == nil || *o.AllowUninstall }

// Settings is settings.json.
type Settings struct {
	// SourcesSeeded is set once the first-run patch repositories have been added, so removing
	// every source later is respected (they are not added again on the next start).
	SourcesSeeded bool                `json:"sources_seeded,omitempty"`
	Sources       []Source            `json:"sources"`
	Sets          []AppSet            `json:"sets,omitempty"`
	Devices       []Device            `json:"devices"`
	Overrides     map[string]Override `json:"overrides,omitempty"`
}

// AppState is what we remember about an app across all devices.
type AppState struct {
	SignerSHA256   string `json:"signer_sha256,omitempty"`
	Slug           string `json:"apkmirror_slug,omitempty"`
	FetchFailures  int    `json:"fetch_failures,omitempty"`
	LastFetchError string `json:"last_fetch_error,omitempty"`
	LastFetchAlert int64  `json:"last_fetch_alert,omitempty"` // unix seconds
	// Builds records which CPU builds a version of the app offers (ABI names, "*" = universal),
	// learned from APKMirror or from a bundle we have. It lets the page refuse to give a device an
	// app it cannot run without downloading anything.
	Builds map[string][]string `json:"builds,omitempty"`
	// ReleaseURLs remembers each version's APKMirror release page, which often differs from what
	// the app's name suggests, so later lookups load it directly (one request, not several).
	ReleaseURLs map[string]string `json:"release_urls,omitempty"`
	// CacheFiles maps each bundle we downloaded into the cache to the app version it holds, so the
	// ones for older versions can be deleted once the newer one is installed.
	CacheFiles map[string]string `json:"cache_files,omitempty"`
	// Patched maps a build key (everything that decides what patching produces: the app version,
	// the patch bundle, the CPU list, the patch options) to the ContentID of the APK it produced,
	// so an identical build is never made twice.
	Patched map[string]string `json:"patched,omitempty"`
}

// Install is what we put on one device.
type Install struct {
	APKVersion       string `json:"apk_version"`
	PatchesVersion   string `json:"patches_version"`
	PatchSource      string `json:"patch_source,omitempty"`      // which patch source built it (empty = default)
	InstalledPackage string `json:"installed_package,omitempty"` // may differ from the app (Netflix clone)
	Source           string `json:"source,omitempty"`
	InstalledAt      string `json:"installed_at,omitempty"`
	// ContentID identifies the patched APK that is on the device (apk.ContentID). A rebuild with
	// the same id installs nothing: the device already has that exact app.
	ContentID string `json:"content_id,omitempty"`
}

// DeviceInfo is what we learned about a device the last time we reached it.
type DeviceInfo struct {
	ABIs     []string `json:"abis"` // in order of preference, as the device reports them
	SDK      int      `json:"sdk"`
	SeenAt   string   `json:"seen_at"`
	PairedAt string   `json:"paired_at,omitempty"` // set when pairing from the page succeeded
	Helper   string   `json:"helper,omitempty"`    // version of the boot helper app on the device
	// KeyExpiry is the device's adb_allowed_connection_time. "0" means Android never drops this
	// server's pairing key; anything else is how long an unused key lives (7 days by default).
	KeyExpiry string `json:"key_expiry,omitempty"`
}

type stateFile struct {
	Apps        map[string]*AppState   `json:"apps"`
	Installs    map[string]*Install    `json:"installs"` // "<device id>/<package>"
	Devices     map[string]*DeviceInfo `json:"device_info,omitempty"`
	LastProfile string                 `json:"apkmirror_last_profile,omitempty"`
}

// Store is safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	dir      string
	settings Settings
	state    stateFile
	ports    map[string]int
}

// Open loads (or starts empty) the files in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, ports: map[string]int{}}
	for name, v := range map[string]any{"settings.json": &s.settings, "state.json": &s.state, "adb_ports.json": &s.ports} {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // our own data directory
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, v); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if s.state.Apps == nil {
		s.state.Apps = map[string]*AppState{}
	}
	if s.state.Installs == nil {
		s.state.Installs = map[string]*Install{}
	}
	if s.state.Devices == nil {
		s.state.Devices = map[string]*DeviceInfo{}
	}
	return s, nil
}

func (s *Store) write(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}

// ---- settings ----

var hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// ValidHost accepts an IPv4 address or a plain hostname, never anything starting with "-".
func ValidHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		return ip.To4() != nil
	}
	if numeric.MatchString(h) {
		return false // looks like an IP but is not one (300.1.1.1)
	}
	return hostRE.MatchString(h)
}

var numeric = regexp.MustCompile(`^[0-9.]+$`)

var packageRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

// ValidPackage accepts an Android package name and nothing else: the name reaches adb command
// lines and file paths, so a shell metacharacter or a ".." in it must never get that far.
func ValidPackage(p string) bool { return len(p) <= 200 && packageRE.MatchString(p) }

// Settings returns a deep copy.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Settings{Overrides: map[string]Override{}, SourcesSeeded: s.settings.SourcesSeeded}
	out.Sources = append([]Source{}, s.settings.Sources...)
	for _, d := range s.settings.Devices {
		d.Apps = append([]AppChoice{}, d.Apps...)
		d.Sets = append([]string{}, d.Sets...)
		out.Devices = append(out.Devices, d)
	}
	for _, set := range s.settings.Sets {
		set.Apps = append([]AppChoice{}, set.Apps...)
		out.Sets = append(out.Sets, set)
	}
	maps.Copy(out.Overrides, s.settings.Overrides)
	return out
}

// Override returns the hand-edited tweaks for a package.
func (s *Store) Override(pkg string) Override {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings.Overrides[pkg]
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// AddDevice registers a device with a unique id derived from its name.
func (s *Store) AddDevice(name, host string) (Device, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return Device{}, errors.New("name must be 1-60 characters")
	}
	if !ValidHost(host) {
		return Device{}, errors.New("host must be an IPv4 address or a simple hostname")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	base := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		base = "device"
	}
	id := base
	for n := 2; s.deviceIndex(id) >= 0; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	for _, d := range s.settings.Devices {
		if d.Host == host {
			return Device{}, fmt.Errorf("%s is already added as %q", host, d.Name)
		}
	}
	d := Device{ID: id, Name: name, Host: host, Apps: []AppChoice{}}
	s.settings.Devices = append(s.settings.Devices, d)
	return d, s.write("settings.json", s.settings)
}

func (s *Store) deviceIndex(id string) int {
	for i, d := range s.settings.Devices {
		if d.ID == id {
			return i
		}
	}
	return -1
}

// RemoveDevice forgets a device and what was recorded on it. Nothing is uninstalled.
func (s *Store) RemoveDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.deviceIndex(id)
	if i < 0 {
		return errors.New("no such device")
	}
	s.settings.Devices = append(s.settings.Devices[:i], s.settings.Devices[i+1:]...)
	for k := range s.state.Installs {
		if strings.HasPrefix(k, id+"/") {
			delete(s.state.Installs, k)
		}
	}
	delete(s.ports, id)
	delete(s.state.Devices, id)
	if err := s.write("state.json", s.state); err != nil {
		return err
	}
	if err := s.write("adb_ports.json", s.ports); err != nil {
		return err
	}
	return s.write("settings.json", s.settings)
}

// SetApps replaces a device's app list. An app appears once per device (the first choice for
// a package wins); an empty source means the first configured one. The list is sorted by package.
func (s *Store) SetApps(id string, choices []AppChoice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.deviceIndex(id)
	if i < 0 {
		return errors.New("no such device")
	}
	apps, err := s.normalizeLocked(choices)
	if err != nil {
		return err
	}
	s.settings.Devices[i].Apps = apps
	return s.write("settings.json", s.settings)
}

// normalizeLocked validates app choices: known sources (an empty one means the first), one per
// package (the first wins), sorted by package.
func (s *Store) normalizeLocked(choices []AppChoice) ([]AppChoice, error) {
	seen := map[string]bool{}
	apps := []AppChoice{}
	for _, c := range choices {
		if !ValidPackage(c.Package) {
			return nil, fmt.Errorf("%q is not a valid package name", c.Package)
		}
		if c.Source == "" && len(s.settings.Sources) > 0 {
			c.Source = s.settings.Sources[0].ID
		}
		if s.sourceIndex(c.Source) < 0 {
			return nil, fmt.Errorf("unknown patch source %q (add a patch repository first)", c.Source)
		}
		if !seen[c.Package] {
			seen[c.Package] = true
			apps = append(apps, c)
		}
	}
	sort.Slice(apps, func(a, b int) bool { return apps[a].Package < apps[b].Package })
	return apps, nil
}

// ---- app sets ----

func (s *Store) setIndex(id string) int {
	for i, set := range s.settings.Sets {
		if set.ID == id {
			return i
		}
	}
	return -1
}

func (s *Store) nameTakenLocked(name, exceptID string) bool {
	for _, set := range s.settings.Sets {
		if set.ID != exceptID && strings.EqualFold(set.Name, name) {
			return true
		}
	}
	return false
}

// Assign makes exactly the given devices use a set (and no others) in a settings value.
func (st *Settings) Assign(setID string, devices []string) error {
	want := map[string]bool{}
	for _, id := range devices {
		known := false
		for _, d := range st.Devices {
			known = known || d.ID == id
		}
		if !known {
			return fmt.Errorf("no such device %q", id)
		}
		want[id] = true
	}
	for i := range st.Devices {
		d := &st.Devices[i]
		has := slices.Contains(d.Sets, setID)
		switch {
		case want[d.ID] && !has:
			d.Sets = append(d.Sets, setID)
		case !want[d.ID] && has:
			d.Sets = slices.DeleteFunc(d.Sets, func(x string) bool { return x == setID })
		}
	}
	return nil
}

// Preview returns a copy of the settings with change applied to it. Nothing is saved: the page
// uses it to see what each device would end up with before it allows a save.
func (s *Store) Preview(change func(*Settings)) Settings {
	cp := s.Settings()
	change(&cp)
	return cp
}

// Normalize validates app choices the same way saving does (known sources, one per package, sorted).
func (s *Store) Normalize(choices []AppChoice) ([]AppChoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.normalizeLocked(choices)
}

// AddSet creates a set and gives it to the listed devices.
func (s *Store) AddSet(name string, apps []AppChoice, devices []string) (AppSet, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return AppSet{}, errors.New("a set needs a name of 1-60 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nameTakenLocked(name, "") {
		return AppSet{}, fmt.Errorf("a set called %q already exists", name)
	}
	list, err := s.normalizeLocked(apps)
	if err != nil {
		return AppSet{}, err
	}
	base := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		base = "set"
	}
	id := base
	for n := 2; s.setIndex(id) >= 0; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	set := AppSet{ID: id, Name: name, Apps: list}
	s.settings.Sets = append(s.settings.Sets, set)
	if err := s.settings.Assign(id, devices); err != nil {
		s.settings.Sets = s.settings.Sets[:len(s.settings.Sets)-1]
		return AppSet{}, err
	}
	return set, s.write("settings.json", s.settings)
}

// UpdateSet changes a set's name and apps; devices (when not nil) becomes the exact list of
// devices that use it.
func (s *Store) UpdateSet(id, name string, apps []AppChoice, devices *[]string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return errors.New("a set needs a name of 1-60 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.setIndex(id)
	if i < 0 {
		return errors.New("no such set")
	}
	if s.nameTakenLocked(name, id) {
		return fmt.Errorf("a set called %q already exists", name)
	}
	list, err := s.normalizeLocked(apps)
	if err != nil {
		return err
	}
	if devices != nil {
		if err := s.settings.Assign(id, *devices); err != nil {
			return err
		}
	}
	s.settings.Sets[i].Name, s.settings.Sets[i].Apps = name, list
	return s.write("settings.json", s.settings)
}

// RemoveSet deletes a set and takes it off every device (nothing is uninstalled).
func (s *Store) RemoveSet(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.setIndex(id)
	if i < 0 {
		return errors.New("no such set")
	}
	_ = s.settings.Assign(id, nil)
	s.settings.Sets = append(s.settings.Sets[:i], s.settings.Sets[i+1:]...)
	return s.write("settings.json", s.settings)
}

// SetDeviceSets makes a device use exactly these sets, in this order.
func (s *Store) SetDeviceSets(deviceID string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.deviceIndex(deviceID)
	if i < 0 {
		return errors.New("no such device")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if s.setIndex(id) < 0 {
			return fmt.Errorf("no such set %q", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	s.settings.Devices[i].Sets = out
	return s.write("settings.json", s.settings)
}

// EffectiveApps returns the apps a device gets from its sets and its own extras.
func (s *Store) EffectiveApps(deviceID string) []EffectiveApp {
	return Effective(s.Settings(), deviceID)
}

// ---- patch sources ----

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// ValidRepo accepts an owner/name GitHub repository.
func ValidRepo(r string) bool {
	return repoRE.MatchString(r) && !strings.HasPrefix(r, ".") && !strings.Contains(r, "..")
}

// RepoOwner returns the owner part of owner/name.
func RepoOwner(r string) string { o, _, _ := strings.Cut(r, "/"); return o }

func (s *Store) sourceIndex(id string) int {
	for i, src := range s.settings.Sources {
		if src.ID == id {
			return i
		}
	}
	return -1
}

// SeedSources adds the first-run patch repositories (the environment's PATCHES_REPO list). It
// does nothing once it has run, so a repository you remove stays removed and a repository that
// disappears upstream never blocks anything.
func (s *Store) SeedSources(repos []string, prerelease bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings.SourcesSeeded {
		return nil
	}
	for _, repo := range repos {
		if !ValidRepo(repo) {
			return fmt.Errorf("PATCHES_REPO must list owner/name repositories: %q", repo)
		}
		if _, err := s.addSourceLocked(repo, prerelease, false); err != nil {
			return err
		}
	}
	s.settings.SourcesSeeded = true
	return s.write("settings.json", s.settings)
}

// AddSource registers another patch repository.
func (s *Store) AddSource(repo string, prerelease, allowPhone bool) (Source, error) {
	if !ValidRepo(repo) {
		return Source{}, errors.New("repository must look like owner/name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.addSourceLocked(repo, prerelease, allowPhone)
	if err != nil {
		return Source{}, err
	}
	return src, s.write("settings.json", s.settings)
}

func (s *Store) addSourceLocked(repo string, prerelease, allowPhone bool) (Source, error) {
	for _, src := range s.settings.Sources {
		if strings.EqualFold(src.Repo, repo) {
			return Source{}, fmt.Errorf("%s is already added", repo)
		}
	}
	base := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(repo), "-"), "-")
	id := base
	for n := 2; s.sourceIndex(id) >= 0; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	src := Source{ID: id, Repo: repo, Prerelease: prerelease, AllowPhone: allowPhone}
	s.settings.Sources = append(s.settings.Sources, src)
	return src, nil
}

// RemoveSource deletes a patch source and drops its apps from every device (nothing is
// uninstalled from the TVs). It returns how many device app choices were removed.
func (s *Store) RemoveSource(id string) (removed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.sourceIndex(id)
	if i < 0 {
		return 0, errors.New("no such patch source")
	}
	s.settings.Sources = append(s.settings.Sources[:i], s.settings.Sources[i+1:]...)
	for d := range s.settings.Devices {
		kept := s.settings.Devices[d].Apps[:0]
		for _, c := range s.settings.Devices[d].Apps {
			if c.Source == id {
				removed++
				continue
			}
			kept = append(kept, c)
		}
		s.settings.Devices[d].Apps = kept
	}
	return removed, s.write("settings.json", s.settings)
}

// Source returns a configured patch source; an empty id means the first one.
func (s *Store) Source(id string) (Source, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" && len(s.settings.Sources) > 0 {
		return s.settings.Sources[0], true
	}
	if i := s.sourceIndex(id); i >= 0 {
		return s.settings.Sources[i], true
	}
	return Source{}, false
}

// ---- state ----

// App returns what we know about an app.
func (s *Store) App(pkg string) AppState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.state.Apps[pkg]; a != nil {
		return *a
	}
	return AppState{}
}

// UpdateApp changes an app's state under the lock and saves it.
func (s *Store) UpdateApp(pkg string, fn func(*AppState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.state.Apps[pkg]
	if a == nil {
		a = &AppState{}
		s.state.Apps[pkg] = a
	}
	fn(a)
	return s.write("state.json", s.state)
}

// CacheFileSet is every downloaded bundle path recorded for any app.
func (s *Store) CacheFileSet() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, a := range s.state.Apps {
		for p := range a.CacheFiles {
			out[p] = true
		}
	}
	return out
}

// InstalledVersions lists the app version of every install on record.
func (s *Store) InstalledVersions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, in := range s.state.Installs {
		if in.APKVersion != "" {
			out = append(out, in.APKVersion)
		}
	}
	return out
}

// Builds returns the CPU builds recorded for an app version, and whether any are recorded.
func (s *Store) Builds(pkg, version string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.state.Apps[pkg]
	if a == nil {
		return nil, false
	}
	b, ok := a.Builds[version]
	return append([]string{}, b...), ok && len(b) > 0
}

// ReleaseURL returns the remembered APKMirror release page for an app version.
func (s *Store) ReleaseURL(pkg, version string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.state.Apps[pkg]; a != nil {
		return a.ReleaseURLs[version]
	}
	return ""
}

// SetReleaseURL remembers the release page of an app version (the newest few are kept).
func (s *Store) SetReleaseURL(pkg, version, url string) error {
	if url == "" || s.ReleaseURL(pkg, version) == url {
		return nil
	}
	return s.UpdateApp(pkg, func(a *AppState) {
		if a.ReleaseURLs == nil {
			a.ReleaseURLs = map[string]string{}
		}
		if _, ok := a.ReleaseURLs[version]; !ok && len(a.ReleaseURLs) >= 6 {
			for k := range a.ReleaseURLs {
				delete(a.ReleaseURLs, k)
				break
			}
		}
		a.ReleaseURLs[version] = url
	})
}

// SetBuilds records which CPU builds an app version offers. An empty list changes nothing.
func (s *Store) SetBuilds(pkg, version string, abis []string) error {
	if len(abis) == 0 {
		return nil
	}
	return s.UpdateApp(pkg, func(a *AppState) {
		if a.Builds == nil {
			a.Builds = map[string][]string{}
		}
		// keep only the newest few versions so the file stays small
		if _, ok := a.Builds[version]; !ok && len(a.Builds) >= 6 {
			for k := range a.Builds {
				delete(a.Builds, k)
				break
			}
		}
		a.Builds[version] = append([]string{}, abis...)
	})
}

// maxPatchedKeys is how many build keys an app remembers; a patches release that changes nothing
// for an app adds one, so a handful covers every build still on disk.
const maxPatchedKeys = 8

// PatchedID returns the ContentID of the APK a build key produced before, or "" when that exact
// build was never made (or is no longer remembered).
func (s *Store) PatchedID(pkg, key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.state.Apps[pkg]; a != nil {
		return a.Patched[key]
	}
	return ""
}

// SetPatchedID remembers what a build key produced (the newest few keys are kept).
func (s *Store) SetPatchedID(pkg, key, id string) error {
	if key == "" || id == "" || s.PatchedID(pkg, key) == id {
		return nil
	}
	return s.UpdateApp(pkg, func(a *AppState) {
		if a.Patched == nil {
			a.Patched = map[string]string{}
		}
		if _, ok := a.Patched[key]; !ok && len(a.Patched) >= maxPatchedKeys {
			for k := range a.Patched {
				delete(a.Patched, k)
				break
			}
		}
		a.Patched[key] = id
	})
}

// Install returns what is recorded on a device for a package.
func (s *Store) Install(dev, pkg string) (Install, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.state.Installs[dev+"/"+pkg]
	if i == nil {
		return Install{}, false
	}
	return *i, true
}

// SetInstall records a successful install.
func (s *Store) SetInstall(dev, pkg string, in Install) error {
	if in.InstalledPackage != "" && !ValidPackage(in.InstalledPackage) {
		return fmt.Errorf("%q is not a valid package name", in.InstalledPackage)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Installs[dev+"/"+pkg] = &in
	return s.write("state.json", s.state)
}

// DeviceInfo returns what we last learned about a device.
func (s *Store) DeviceInfo(dev string) (DeviceInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.state.Devices[dev]
	if d == nil {
		return DeviceInfo{}, false
	}
	out := *d // every field: a new one must reach the page and verifyDue without being listed here
	out.ABIs = append([]string{}, d.ABIs...)
	return out, true
}

// SetDeviceInfo records a device's CPU list and Android level (keeping when it was paired).
func (s *Store) SetDeviceInfo(dev string, in DeviceInfo) error {
	return s.UpdateDeviceInfo(dev, func(d *DeviceInfo) {
		d.ABIs, d.SDK, d.SeenAt = in.ABIs, in.SDK, in.SeenAt
		if in.PairedAt != "" {
			d.PairedAt = in.PairedAt
		}
	})
}

// UpdateDeviceInfo changes what we know about a device under the lock and saves it.
func (s *Store) UpdateDeviceInfo(dev string, fn func(*DeviceInfo)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.state.Devices[dev]
	if d == nil {
		d = &DeviceInfo{}
		s.state.Devices[dev] = d
	}
	fn(d)
	return s.write("state.json", s.state)
}

// LastProfile / SetLastProfile remember which browser profile APKMirror last accepted.
func (s *Store) LastProfile() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.LastProfile
}

// SetLastProfile saves the profile name.
func (s *Store) SetLastProfile(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.LastProfile == p {
		return nil
	}
	s.state.LastProfile = p
	return s.write("state.json", s.state)
}

// Port / SetPort remember the last working wireless-debugging port for a device.
func (s *Store) Port(dev string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ports[dev]
}

// SetPort saves it.
func (s *Store) SetPort(dev string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ports[dev] == port {
		return nil
	}
	s.ports[dev] = port
	return s.write("adb_ports.json", s.ports)
}
