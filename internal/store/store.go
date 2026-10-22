// Package store keeps everything that must survive restarts: the devices and apps you chose
// (settings.json), what has been installed and what we learned about each app (state.json),
// and the last working wireless-debugging port per device. All in one data volume.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Device is a target Android TV / box.
type Device struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Host string   `json:"host"`
	Apps []string `json:"apps"` // package names to keep installed
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
	Devices   []Device            `json:"devices"`
	Overrides map[string]Override `json:"overrides,omitempty"`
}

// AppState is what we remember about an app across all devices.
type AppState struct {
	SignerSHA256   string `json:"signer_sha256,omitempty"`
	Slug           string `json:"apkmirror_slug,omitempty"`
	FetchFailures  int    `json:"fetch_failures,omitempty"`
	LastFetchError string `json:"last_fetch_error,omitempty"`
	LastFetchAlert int64  `json:"last_fetch_alert,omitempty"` // unix seconds
}

// Install is what we put on one device.
type Install struct {
	APKVersion       string `json:"apk_version"`
	PatchesVersion   string `json:"patches_version"`
	InstalledPackage string `json:"installed_package,omitempty"` // may differ from the app (Netflix clone)
	Source           string `json:"source,omitempty"`
	InstalledAt      string `json:"installed_at,omitempty"`
}

type stateFile struct {
	Apps        map[string]*AppState `json:"apps"`
	Installs    map[string]*Install  `json:"installs"` // "<device id>/<package>"
	LastProfile string               `json:"apkmirror_last_profile,omitempty"`
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, ports: map[string]int{}}
	for name, v := range map[string]any{"settings.json": &s.settings, "state.json": &s.state, "adb_ports.json": &s.ports} {
		b, err := os.ReadFile(filepath.Join(dir, name))
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
	out := Settings{Overrides: map[string]Override{}}
	for _, d := range s.settings.Devices {
		d.Apps = append([]string{}, d.Apps...)
		out.Devices = append(out.Devices, d)
	}
	for k, v := range s.settings.Overrides {
		out.Overrides[k] = v
	}
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
	d := Device{ID: id, Name: name, Host: host, Apps: []string{}}
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
	if err := s.write("state.json", s.state); err != nil {
		return err
	}
	if err := s.write("adb_ports.json", s.ports); err != nil {
		return err
	}
	return s.write("settings.json", s.settings)
}

// SetApps replaces a device's app list (sorted, de-duplicated).
func (s *Store) SetApps(id string, pkgs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.deviceIndex(id)
	if i < 0 {
		return errors.New("no such device")
	}
	seen := map[string]bool{}
	apps := []string{}
	for _, p := range pkgs {
		if !seen[p] {
			seen[p] = true
			apps = append(apps, p)
		}
	}
	sort.Strings(apps)
	s.settings.Devices[i].Apps = apps
	return s.write("settings.json", s.settings)
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
