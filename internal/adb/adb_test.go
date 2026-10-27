package adb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

// fake adb: connect succeeds only for one port.
func fake(goodPort int, log *[]string) Runner {
	return func(ctx context.Context, args ...string) (string, error) {
		*log = append(*log, strings.Join(args, " "))
		switch args[0] {
		case "connect":
			if strings.HasSuffix(args[1], fmt.Sprintf(":%d", goodPort)) {
				return "connected to " + args[1], nil
			}
			return "failed to connect to " + args[1], nil
		case "mdns":
			return "List of discovered mdns services\nadb-ABC\t_adb-tls-connect._tcp\t10.0.0.6:41234\nadb-DEF\t_adb-tls-connect._tcp\t10.0.0.9:39999", nil
		}
		return "", nil
	}
}

func TestConnectTriesSavedThenMDNSThenScan(t *testing.T) {
	var log []string
	c := &Client{Run: fake(41234, &log), Dial: func(string, int) bool { return false }}
	// the saved port is stale; mDNS finds the right one for this host only
	port, err := c.Connect(context.Background(), "10.0.0.6", 37000)
	if err != nil || port != 41234 {
		t.Fatalf("port %d err %v", port, err)
	}
	if !strings.Contains(strings.Join(log, "\n"), "connect 10.0.0.6:37000") {
		t.Error("saved port should be tried first")
	}
	if strings.Contains(strings.Join(log, "\n"), "10.0.0.9") {
		t.Error("another device's mDNS entry must be ignored")
	}
}

func TestConnectScansWhenNothingElseWorks(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("not a TCP listener")
	}
	open := addr.Port
	var log []string
	c := &Client{
		Run: func(ctx context.Context, args ...string) (string, error) {
			if args[0] == "mdns" {
				return "", errors.New("no mdns")
			}
			return fake(open, &log)(ctx, args...)
		},
		Dial:   dial,
		ScanLo: open - 20, ScanHi: open + 20,
	}
	port, err := c.Connect(context.Background(), "127.0.0.1")
	if err != nil || port != open {
		t.Fatalf("port %d err %v", port, err)
	}
}

func TestConnectUnpairedAndUnreachable(t *testing.T) {
	c := &Client{Run: func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "connect" {
			return "failed to authenticate to 10.0.0.6:40000", nil
		}
		return "", nil
	}, Dial: func(string, int) bool { return false }}
	if _, err := c.Connect(context.Background(), "10.0.0.6", 40000); !errors.Is(err, ErrNotPaired) {
		t.Errorf("got %v", err)
	}
	var log []string
	c = &Client{Run: fake(0, &log), Dial: func(string, int) bool { return false }, ScanLo: 1, ScanHi: 5}
	if _, err := c.Connect(context.Background(), "10.0.0.6"); !errors.Is(err, ErrUnreachable) {
		t.Errorf("got %v", err)
	}
}

func TestPairInstallAndQueries(t *testing.T) {
	c := &Client{Run: func(ctx context.Context, args ...string) (string, error) {
		switch {
		case args[0] == "pair":
			if args[1] != "10.0.0.6:38000" || args[2] != "123456" {
				return "bad args", errors.New("x")
			}
			return "Successfully paired to 10.0.0.6:38000 [guid=adb-x]", nil
		case args[0] == "-s" && args[2] == "install":
			return "Performing Streamed Install\nSuccess", nil
		case args[2] == "shell" && strings.Contains(args[3], "abilist"):
			return "armeabi-v7a,armeabi\n", nil
		case args[2] == "shell" && strings.Contains(args[3], "version.sdk"):
			return "31", nil
		// the package name must arrive as its own argument, never concatenated into the command
		// string that the device's shell would re-parse
		case args[2] == "shell" && args[3] == "dumpsys" && args[4] == "package" && args[5] == "com.a":
			return "Packages:\n  versionCode=5\n  versionName=13.0.1 build 25028\n  flags=[ ]", nil
		case args[2] == "shell" && args[3] == "dumpsys" && args[4] == "package" && args[5] == "com.none":
			return "Unable to find package: com.none", nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	}}
	ctx := context.Background()
	if err := c.Pair(ctx, "10.0.0.6", 38000, "123456"); err != nil {
		t.Error(err)
	}
	if ok, _ := c.Install(ctx, "s", "/x.apk"); !ok {
		t.Error("install should succeed")
	}
	if abis, _ := c.ABIs(ctx, "s"); strings.Join(abis, ",") != "armeabi-v7a,armeabi" {
		t.Errorf("abis %v", abis)
	}
	if sdk, _ := c.SDK(ctx, "s"); sdk != 31 {
		t.Errorf("sdk %d", sdk)
	}
	if v, _ := c.InstalledVersion(ctx, "s", "com.a"); v != "13.0.1 build 25028" {
		t.Errorf("version %q", v)
	}
	if v, _ := c.InstalledVersion(ctx, "s", "com.none"); v != "" {
		t.Errorf("missing package should be empty, got %q", v)
	}
	c.Run = func(context.Context, ...string) (string, error) {
		return "Failed to pair: wrong code", errors.New("exit 1")
	}
	if err := c.Pair(ctx, "10.0.0.6", 38000, "000000"); err == nil || !strings.Contains(err.Error(), "wrong code") {
		t.Errorf("got %v", err)
	}
}

func TestReachableNeverOpensAnADBSession(t *testing.T) {
	var log []string
	open := map[int]bool{41234: true}
	c := &Client{Run: fake(41234, &log), Dial: func(_ string, p int) bool { return open[p] }}
	// the remembered port answers: no adb command at all
	if p, ok := c.Reachable(context.Background(), "10.0.0.6", 41234); !ok || p != 41234 || len(log) != 0 {
		t.Fatalf("%d %v %v", p, ok, log)
	}
	// stale port: the mDNS listing finds the new one, and only for this host
	if p, ok := c.Reachable(context.Background(), "10.0.0.6", 37000); !ok || p != 41234 {
		t.Fatalf("%d %v", p, ok)
	}
	for _, l := range log {
		if strings.HasPrefix(l, "connect") {
			t.Errorf("must never run adb connect: %v", log)
		}
	}
	if _, ok := c.Reachable(context.Background(), "10.0.0.9", 37000); ok {
		t.Error("that host's advertised port is closed")
	}
	open[41234] = false
	if _, ok := c.Reachable(context.Background(), "10.0.0.6", 41234); ok {
		t.Error("nothing listening")
	}
}

func TestPlayingFrom(t *testing.T) {
	playing := "  Sessions Stack - have 1 sessions:\n    com.netflix.ninja/x\n      state=PlaybackState {state=3, position=1000, buffered position=0}"
	paused := "      state=PlaybackState {state=2, position=1000}"
	cases := []struct {
		name, session, audio string
		want                 bool
	}{
		{"playing", playing, "", true},
		{"paused", paused, "players:\n  AudioPlaybackConfiguration piid:5 state:paused", false},
		{"idle", "Sessions Stack - have 0 sessions:", "", false},
		{"no session but audio running", "", "AudioPlaybackConfiguration piid:7 state:started", true},
		{"state 30 is not playing", "state=PlaybackState {state=30,", "", false},
	}
	for _, tc := range cases {
		if got := playingFrom(tc.session, tc.audio); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
