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
	Apps []AppChoice `json:"apps"` // apps to keep installed
}

// Override is a per-app tweak edited by hand in settings.json (the UI never touches it).
type Override struct {
	APKMirror      string   `json:"apkmirror,omitempty"` // publisher/app path, if detection picks wrongly
	Sources        []string `json:"sources,omitempty"`   // order of: apkmirror, apkpure
	SignerSHA256   string   `json:"signer_sha256,omitempty"`
	Enable         []string `json:"enable,omitempty"` // Morphe patch names
	Disable        []string `json:"disable,omitempty"`
	OptionsFile    string   `json:"options_file,omitempty"`
	AllowUninstall bool     `json:"allow_uninstall,omitempty"`
}

// Settings is settings.json.
type Settings struct {
	// SourcesSeeded is set once the first-run patch repositories have been added, so removing
	// every source later is respected (they are not added again on the next start).
	SourcesSeeded bool                `json:"sources_seeded,omitempty"`
	Sources       []Source            `json:"sources"`
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
}

// Install is what we put on one device.
type Install struct {
	APKVersion       string `json:"apk_version"`
	PatchesVersion   string `json:"patches_version"`
	PatchSource      string `json:"patch_source,omitempty"`      // which patch source built it (empty = default)
	InstalledPackage string `json:"installed_package,omitempty"` // may differ from the app (Netflix clone)
	Source           string `json:"source,omitempty"`
	InstalledAt      string `json:"installed_at,omitempty"`
}

// DeviceInfo is what we learned about a device the last time we reached it.
type DeviceInfo struct {
	ABIs   []string `json:"abis"` // in order of preference, as the device reports them
	SDK    int      `json:"sdk"`
	SeenAt string   `json:"seen_at"`
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

// Settings returns a deep copy.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Settings{Overrides: map[string]Override{}, SourcesSeeded: s.settings.SourcesSeeded}
	out.Sources = append([]Source{}, s.settings.Sources...)
	for _, d := range s.settings.Devices {
		d.Apps = append([]AppChoice{}, d.Apps...)
		out.Devices = append(out.Devices, d)
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
	seen := map[string]bool{}
	apps := []AppChoice{}
	for _, c := range choices {
		if c.Source == "" && len(s.settings.Sources) > 0 {
			c.Source = s.settings.Sources[0].ID
		}
		if s.sourceIndex(c.Source) < 0 {
			return fmt.Errorf("unknown patch source %q (add a patch repository first)", c.Source)
		}
		if !seen[c.Package] {
			seen[c.Package] = true
			apps = append(apps, c)
		}
	}
	sort.Slice(apps, func(a, b int) bool { return apps[a].Package < apps[b].Package })
	s.settings.Devices[i].Apps = apps
	return s.write("settings.json", s.settings)
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
	return DeviceInfo{ABIs: append([]string{}, d.ABIs...), SDK: d.SDK, SeenAt: d.SeenAt}, true
}

// SetDeviceInfo records a device's CPU list and Android level.
func (s *Store) SetDeviceInfo(dev string, in DeviceInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Devices[dev] = &in
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
