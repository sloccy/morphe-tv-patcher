// tv-patcher keeps patched Android TV apps installed on your Google TVs.
//
//	tv-patcher run          web page + scheduler (the container default)
//	tv-patcher once         one pass now, print the results
//	tv-patcher pair         register a device (command-line alternative to the web page)
//	tv-patcher fetch        download and verify one app (debugging / CI)
//	tv-patcher resolve      find the APKMirror page for apps without downloading (CI)
//	tv-patcher healthcheck  for the Docker HEALTHCHECK
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/adb"
	"github.com/sloccy/morphe-tv-patcher/internal/apkmirror"
	"github.com/sloccy/morphe-tv-patcher/internal/config"
	"github.com/sloccy/morphe-tv-patcher/internal/morphe"
	"github.com/sloccy/morphe-tv-patcher/internal/notify"
	"github.com/sloccy/morphe-tv-patcher/internal/patches"
	"github.com/sloccy/morphe-tv-patcher/internal/runner"
	"github.com/sloccy/morphe-tv-patcher/internal/sources"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
	"github.com/sloccy/morphe-tv-patcher/internal/verify"
	"github.com/sloccy/morphe-tv-patcher/internal/web"
)

// Set at build time: -ldflags "-X main.version=... -X main.commit=..."
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "run":
		err = cmdRun()
	case "once":
		err = cmdOnce(args)
	case "pair":
		err = cmdPair(args)
	case "fetch":
		err = cmdFetch(args)
	case "resolve":
		err = cmdResolve(args)
	case "healthcheck":
		err = cmdHealth()
	case "version":
		fmt.Printf("tv-patcher %s (%s)\n", version, commit)
	default:
		err = fmt.Errorf("unknown command %q (run, once, pair, fetch, healthcheck, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type app struct {
	cfg    config.Config
	st     *store.Store
	runner *runner.Runner
	ring   *runner.Ring
	hc     *http.Client
}

func setup() (*app, error) {
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, hc: &http.Client{Timeout: 5 * time.Minute}}
	a.ring = runner.NewRing(300, os.Stderr)
	slog.SetDefault(a.ring.Logger())
	if err := cfg.Ensure(); err != nil {
		return nil, err
	}
	if a.st, err = store.Open(cfg.DataDir + "/state"); err != nil {
		return nil, err
	}
	env := sources.DefaultEnv()
	env.InputDir, env.CacheDir, env.Store = cfg.Input(), cfg.Cache(), a.st
	env.FlareURL, env.DirectOK, env.APKPureOK = cfg.FlareSolverrURL, cfg.DirectOK, cfg.APKPureOK
	deps := runner.Deps{
		Latest: func(ctx context.Context) (*patches.Release, error) {
			return patches.LatestRelease(ctx, a.hc, cfg.PatchesRepo, cfg.GitHubToken, cfg.Prerelease)
		},
		Download: func(ctx context.Context, rel *patches.Release) (string, error) {
			return patches.Download(ctx, a.hc, rel, cfg.Cache())
		},
		List: func(ctx context.Context, tag string) (*patches.List, error) {
			return patches.FetchList(ctx, a.hc, cfg.PatchesRepo, tag, cfg.GitHubToken)
		},
		CLIVersions: func(ctx context.Context, mpp, pkg string) []string {
			return morphe.ListVersions(ctx, cfg.MorpheJar, mpp, pkg)
		},
		Fetch: func(ctx context.Context, x sources.App, sdk int) (*sources.Fetched, error) {
			return sources.Fetch(ctx, &env, x, sdk)
		},
		Patch:     morphe.Patch,
		PackageOf: verify.PackageOf,
	}
	tg := &notify.Telegram{Token: cfg.TelegramToken, Chat: cfg.TelegramChat}
	a.runner = runner.New(cfg, a.st, adb.New(), deps, tg.Send)
	return a, nil
}

func cmdRun() error {
	a, err := setup()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr: fmt.Sprintf("%s:%d", a.cfg.WebBind, a.cfg.WebPort),
		Handler: (&web.Server{Store: a.st, Backend: a.runner, Log: a.ring, Version: version,
			AllowedHostSuffixes: a.cfg.AllowedHostSuffixes}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
	}
	go func() {
		slog.Info("web ui listening", "addr", srv.Addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web server stopped", "err", err)
			stop()
		}
	}()
	go func() { // so the page can list apps before the first run finishes
		if err := a.runner.RefreshList(ctx); err != nil {
			slog.Warn("could not load the app list yet", "err", err)
		}
	}()
	a.runner.Loop(ctx)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, strings.Split(v, ",")...); return nil }

func cmdOnce(args []string) error {
	fs := flag.NewFlagSet("once", flag.ExitOnError)
	device := fs.String("device", "", "only this device id")
	force := fs.Bool("force", false, "reinstall even if up to date")
	dry := fs.Bool("dry-run", false, "patch but do not install")
	noDev := fs.Bool("no-device", false, "do not use adb at all (patch only)")
	var apps listFlag
	fs.Var(&apps, "app", "package to handle (repeatable, or comma separated)")
	_ = fs.Parse(args)
	a, err := setup()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := runner.Options{DeviceID: *device, Force: *force, DryRun: *dry, NoDevice: *noDev, Packages: apps}
	if len(apps) == 1 {
		o.Package = apps[0]
	}
	bad := false
	for _, r := range a.runner.Run(ctx, o) {
		status := "ok  "
		if !r.OK {
			status, bad = "FAIL", true
		}
		fmt.Printf("%s %s\n", status, r.Message)
	}
	if bad {
		return errors.New("some apps failed")
	}
	return nil
}

func cmdPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	device := fs.String("device", "", "device id (default: the only device)")
	port := fs.Int("pair-port", 0, "port shown in the TV's pairing dialog")
	code := fs.String("code", "", "6-digit pairing code")
	_ = fs.Parse(args)
	a, err := setup()
	if err != nil {
		return err
	}
	devs := a.st.Settings().Devices
	var host string
	for _, d := range devs {
		if *device == d.ID || (*device == "" && len(devs) == 1) {
			host = d.Host
		}
	}
	if host == "" {
		return errors.New("choose a device with --device (add it in the web page first)")
	}
	if *port == 0 || *code == "" {
		return errors.New("usage: pair --device ID --pair-port PORT --code 123456")
	}
	if err := a.runner.Pair(context.Background(), host, *port, *code); err != nil {
		return err
	}
	fmt.Println("paired with", host)
	return nil
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	pkg := fs.String("package", "", "package name")
	ver := fs.String("version", "", "version (default: newest the patches support)")
	sdk := fs.Int("sdk", 31, "device API level")
	_ = fs.Parse(args)
	if *pkg == "" {
		return errors.New("usage: fetch --package com.example.app [--version V]")
	}
	a, err := setup()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rel, err := patches.LatestRelease(ctx, a.hc, a.cfg.PatchesRepo, a.cfg.GitHubToken, a.cfg.Prerelease)
	if err != nil {
		return err
	}
	list, err := patches.FetchList(ctx, a.hc, a.cfg.PatchesRepo, rel.Tag, a.cfg.GitHubToken)
	if err != nil {
		return err
	}
	app, ok := list.App(*pkg)
	if !ok {
		return fmt.Errorf("%s is not supported by patches %s", *pkg, rel.Tag)
	}
	if *ver == "" {
		*ver = app.Newest
	}
	env := sources.DefaultEnv()
	env.InputDir, env.CacheDir, env.Store = a.cfg.Input(), a.cfg.Cache(), a.st
	env.FlareURL, env.DirectOK, env.APKPureOK = a.cfg.FlareSolverrURL, a.cfg.DirectOK, a.cfg.APKPureOK
	ov := a.st.Override(*pkg)
	f, err := sources.Fetch(ctx, &env, sources.App{Package: *pkg, Name: app.Name, Version: *ver, Sources: ov.Sources,
		SignerPin: ov.SignerSHA256, SlugOverride: ov.APKMirror}, *sdk)
	if err != nil {
		return err
	}
	facts, err := verify.Inspect(ctx, f.Path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"path": f.Path, "source": f.Detail, "package": f.Package, "version": f.Version,
		"leanback": facts.Leanback, "abis": facts.ABIs, "signer": f.Signer, "apksigner_ok": facts.SignerOK,
		"slug": a.st.App(*pkg).Slug, "signer_learned": f.LearnedSigner})
}

func cmdHealth() error {
	port := os.Getenv("WEB_PORT")
	if port == "" {
		port = "8093"
	}
	c := &http.Client{Timeout: 4 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:"+port+"/api/health", http.NoBody) //nolint:gosec // localhost only; the port comes from our own environment
	if err != nil {
		return err
	}
	resp, err := c.Do(req) //nolint:gosec // localhost only; the port comes from our own environment
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16))
	if resp.StatusCode != http.StatusOK || string(b) != "ok" {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}

// cmdResolve checks that every supported app can be found on APKMirror, without downloading.
func cmdResolve(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ExitOnError)
	pkg := fs.String("package", "", "only this package (default: every supported app)")
	_ = fs.Parse(args)
	a, err := setup()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rel, err := patches.LatestRelease(ctx, a.hc, a.cfg.PatchesRepo, a.cfg.GitHubToken, a.cfg.Prerelease)
	if err != nil {
		return err
	}
	list, err := patches.FetchList(ctx, a.hc, a.cfg.PatchesRepo, rel.Tag, a.cfg.GitHubToken)
	if err != nil {
		return err
	}
	bad := false
	for _, app := range list.Apps {
		if *pkg != "" && app.Package != *pkg {
			continue
		}
		res, _, err := apkmirror.Download(ctx, apkmirror.AppRequest{Package: app.Package, Name: app.Name, Version: app.Newest,
			Hint: apkmirror.Hints[app.Package], SDK: 31, DryRun: true}, apkmirror.Options{ProfileHint: a.st.LastProfile()})
		if err != nil {
			bad = true
			fmt.Printf("MISS %-40s %-28s %v\n", app.Name, app.Newest, err)
		} else {
			fmt.Printf("ok   %-40s %-28s %s\n", app.Name, app.Newest, res.Slug)
		}
		time.Sleep(8 * time.Second)
	}
	if bad {
		return errors.New("some apps could not be found")
	}
	return nil
}
