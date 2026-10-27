// Package config reads settings from environment variables (the same style as Lantern).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is everything set by the environment.
type Config struct {
	DataDir   string
	MorpheJar string
	// PatchesRepos seeds the patch sources on the very first run only; after that the list lives in
	// settings.json (edited from the page) and every source can be removed.
	PatchesRepos []string
	Prerelease   bool
	BytecodeMode string
	Interval     time.Duration

	FlareSolverrURL     string
	DirectOK            bool // false with APKMIRROR_DIRECT_DISABLED=true (tests the other routes)
	FailuresBeforeAlert int
	// AllowUnverifiedSignature accepts app files when apksigner is not installed, so no signature
	// can be checked. The image always has apksigner; this is for runs without the Android SDK.
	AllowUnverifiedSignature bool
	// BootHelperAPK is the tiny app that switches Wireless debugging back on after a TV reboot;
	// the image carries it. Empty or missing means the step is skipped.
	BootHelperAPK string
	// VerifyEvery is how often a scheduled run connects to a TV only to check what is really
	// installed; in between it plans from its own records and connects when something needs
	// installing. Run now and Force always connect.
	VerifyEvery time.Duration
	// BackgroundProcessLimit is the TV's "Background process limit" developer setting (0 = leave it
	// alone). It is set over ADB whenever tv-patcher connects. PersistTVFlags also turns off
	// Google's flag sync on the TV so the value survives a restart (off by default).
	BackgroundProcessLimit int
	PersistTVFlags         bool
	// MirrorGap is the minimum time between APKMirror requests; MirrorCooldown is the first pause
	// after it pushes back (doubling while it keeps doing so).
	MirrorGap      time.Duration
	MirrorCooldown time.Duration

	WebBind string
	WebPort int
	// AllowedHostSuffixes lists extra Host header endings the web UI answers to (e.g. ".sloccy.com"),
	// besides IP addresses and localhost. This stops DNS-rebinding from other web pages.
	AllowedHostSuffixes []string
	// PatchSourcesAllowedOwners limits which GitHub owners can be added as patch sources from the
	// page. Empty (the default) allows any repository.
	PatchSourcesAllowedOwners []string

	TelegramToken string
	TelegramChat  string
	GitHubToken   string
}

// Dirs under the data volume.
func (c Config) Cache() string    { return filepath.Join(c.DataDir, "work", "cache") }
func (c Config) Input() string    { return filepath.Join(c.DataDir, "work", "input") }
func (c Config) Output() string   { return filepath.Join(c.DataDir, "work", "output") }
func (c Config) Keystore() string { return filepath.Join(c.DataDir, "keystore", "morphe.keystore") }

// PatchBundles holds each patch source's latest bundle. It lives in the small state volume (not the
// cache) so the last good bundles are in the weekly backup and survive a repository disappearing.
func (c Config) PatchBundles() string { return filepath.Join(c.DataDir, "patches") }

// Ensure creates the working directories.
func (c Config) Ensure() error {
	for _, d := range []string{c.Cache(), c.Input(), c.Output(), filepath.Dir(c.Keystore()), c.PatchBundles()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	return nil
}

// FromEnv builds a Config; getenv is os.Getenv in production.
func FromEnv(getenv func(string) string) (Config, error) {
	def := func(k, d string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return d
	}
	flag := func(k string) bool {
		v := strings.ToLower(strings.TrimSpace(getenv(k)))
		return v == "1" || v == "true" || v == "yes"
	}
	c := Config{
		DataDir:                   def("DATA_DIR", "/data"),
		MorpheJar:                 def("MORPHE_JAR", "/opt/morphe/morphe.jar"),
		PatchesRepos:              splitRepos(def("PATCHES_REPO", "ajstrick81/morphe-androidtv-patches")),
		Prerelease:                flag("PATCHES_PRERELEASE"),
		BytecodeMode:              def("BYTECODE_MODE", "STRIP_SAFE"),
		FlareSolverrURL:           strings.TrimRight(def("FLARESOLVERR_URL", ""), "/"),
		DirectOK:                  !flag("APKMIRROR_DIRECT_DISABLED"),
		AllowUnverifiedSignature:  flag("ALLOW_UNVERIFIED_SIGNATURES"),
		BootHelperAPK:             def("BOOT_HELPER_APK", "/opt/bootadb/bootadb.apk"),
		WebBind:                   def("WEB_BIND", "0.0.0.0"),
		AllowedHostSuffixes:       splitList(def("ALLOWED_HOST_SUFFIXES", "")),
		PatchSourcesAllowedOwners: splitList(def("PATCH_SOURCES_ALLOWED_OWNERS", "")),
		TelegramToken:             def("TELEGRAM_BOT_TOKEN", ""),
		TelegramChat:              def("TELEGRAM_CHAT_ID", ""),
		GitHubToken:               def("GITHUB_TOKEN", ""),
	}
	var err error
	if c.Interval, err = time.ParseDuration(def("INTERVAL", "6h")); err != nil || c.Interval < time.Minute {
		return c, fmt.Errorf("INTERVAL must be a duration of at least 1m (e.g. 6h): %q", getenv("INTERVAL"))
	}
	if c.MirrorGap, err = time.ParseDuration(def("APKMIRROR_MIN_INTERVAL", "4s")); err != nil || c.MirrorGap < 0 {
		return c, fmt.Errorf("APKMIRROR_MIN_INTERVAL must be a duration (e.g. 4s): %q", getenv("APKMIRROR_MIN_INTERVAL"))
	}
	if c.MirrorCooldown, err = time.ParseDuration(def("APKMIRROR_COOLDOWN", "5m")); err != nil || c.MirrorCooldown < time.Second {
		return c, fmt.Errorf("APKMIRROR_COOLDOWN must be a duration of at least 1s (e.g. 5m): %q", getenv("APKMIRROR_COOLDOWN"))
	}
	if c.VerifyEvery, err = time.ParseDuration(def("VERIFY_EVERY", "168h")); err != nil || c.VerifyEvery < time.Hour {
		return c, fmt.Errorf("VERIFY_EVERY must be a duration of at least 1h (e.g. 168h): %q", getenv("VERIFY_EVERY"))
	}
	if c.BackgroundProcessLimit, err = strconv.Atoi(def("BACKGROUND_PROCESS_LIMIT", "0")); err != nil || c.BackgroundProcessLimit < 0 || c.BackgroundProcessLimit > 100 {
		return c, fmt.Errorf("BACKGROUND_PROCESS_LIMIT must be 0 (off) to 100: %q", getenv("BACKGROUND_PROCESS_LIMIT"))
	}
	c.PersistTVFlags = flag("TV_PERSIST_FLAGS")
	if c.WebPort, err = strconv.Atoi(def("WEB_PORT", "8093")); err != nil || c.WebPort < 1 || c.WebPort > 65535 {
		return c, fmt.Errorf("WEB_PORT must be a port number: %q", getenv("WEB_PORT"))
	}
	if c.FailuresBeforeAlert, err = strconv.Atoi(def("FETCH_FAILURES_BEFORE_ALERT", "3")); err != nil || c.FailuresBeforeAlert < 1 {
		return c, fmt.Errorf("FETCH_FAILURES_BEFORE_ALERT must be 1 or more: %q", getenv("FETCH_FAILURES_BEFORE_ALERT"))
	}
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitRepos reads a comma or space separated list of owner/name repositories.
func splitRepos(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
