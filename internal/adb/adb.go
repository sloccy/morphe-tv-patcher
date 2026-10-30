// Package adb drives the Android Debug Bridge for Google TV wireless debugging: pairing,
// finding the (random, changing) connection port, and installing apps.
package adb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotPaired: the device is reachable but has not authorised this machine.
var ErrNotPaired = errors.New("device not paired (use Pair, then approve on the TV)")

// ErrUnreachable: no working wireless-debugging port was found.
var ErrUnreachable = errors.New("device unreachable (is Wireless debugging on?)")

// Runner runs `adb <args>` and returns combined output.
type Runner func(ctx context.Context, args ...string) (string, error)

// Client wraps an adb binary.
type Client struct {
	Run  Runner
	Dial func(host string, port int) bool
	// MDNS asks the LAN which port a service is on. Ubuntu's android-tools-adb is built without
	// a mDNS backend, so "adb mdns services" finds nothing there and the port would have to be
	// scanned for; this asks for it directly. Nil means do not ask (the tests' default).
	MDNS func(ctx context.Context, service string) (map[string]int, error)
	// ScanLo, ScanHi bound the last-resort port scan. adbd takes its port from the kernel's
	// ephemeral range, which on Linux is 32768-60999.
	ScanLo int
	ScanHi int
}

// New returns a Client using the adb on PATH.
func New() *Client {
	return &Client{
		Run:  execRun,
		Dial: dial,
		MDNS: func(ctx context.Context, service string) (map[string]int, error) {
			return Lookup(ctx, service, mdnsWait)
		},
		ScanLo: 30000, ScanHi: 61000,
	}
}

const mdnsWait = 1500 * time.Millisecond

func execRun(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "adb", args...) //nolint:gosec // fixed binary; arguments are built by us from validated host/port values
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func dial(host string, port int) bool {
	d := net.Dialer{Timeout: 400 * time.Millisecond}
	c, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Pair registers this machine with the device (one time). pairPort and code come from the
// "Pair device with pairing code" dialog on the TV.
func (c *Client) Pair(ctx context.Context, host string, pairPort int, code string) error {
	out, err := c.Run(ctx, "pair", net.JoinHostPort(host, strconv.Itoa(pairPort)), code)
	if err != nil || !strings.Contains(out, "Successfully paired") {
		return fmt.Errorf("pairing failed: %s", firstLine(out, err))
	}
	return nil
}

var mdnsLine = regexp.MustCompile(`(_adb-tls-\w+)\S*\s+(\S+):(\d+)`)

// mdnsPort returns the port host advertises for a service, or 0. It asks the LAN itself and then
// falls back to the adb server's own listing, which only works on an adb built with mDNS support.
func (c *Client) mdnsPort(ctx context.Context, host, service string) int {
	if c.MDNS != nil {
		if found, err := c.MDNS(ctx, service); err == nil && found[host] > 0 {
			return found[host]
		}
	}
	out, err := c.Run(ctx, "mdns", "services")
	if err != nil {
		return 0
	}
	for _, m := range mdnsLine.FindAllStringSubmatch(out, -1) {
		if m[1]+"._tcp.local" == service && m[2] == host {
			p, _ := strconv.Atoi(m[3])
			return p
		}
	}
	return 0
}

// PairingPort asks the LAN which port the TV's "Pair device with pairing code" dialog is on.
// The dialog shows it on screen, but it is a different random port every time it is opened.
func (c *Client) PairingPort(ctx context.Context, host string) int {
	return c.mdnsPort(ctx, host, ServicePairing)
}

// an "adb devices" line: an address and what the adb server makes of it
var deviceLine = regexp.MustCompile(`(?m)^(\S+):(\d+)\s+(device|unauthorized|offline)\b`)

// ConnectedPort asks the adb server which port it is already talking to host on. Pairing leaves
// the server knowing the TV's current port, so this learns a changed port without any discovery.
func (c *Client) ConnectedPort(ctx context.Context, host string) int {
	out, err := c.Run(ctx, "devices")
	if err != nil {
		return 0
	}
	best := 0
	for _, m := range deviceLine.FindAllStringSubmatch(out, -1) {
		if m[1] != host || m[3] == "offline" {
			continue
		}
		p, _ := strconv.Atoi(m[2])
		if p > 0 && (best == 0 || m[3] == "device") {
			best = p
		}
	}
	return best
}

// Disconnect drops the adb server's entry for an address, so a port the TV has stopped using is
// not kept around as an offline device that later "-s host:port" calls could pick up.
func (c *Client) Disconnect(ctx context.Context, host string, port int) error {
	_, err := c.Run(ctx, "disconnect", net.JoinHostPort(host, strconv.Itoa(port)))
	return err
}

// TCPIP restarts adbd on a fixed TCP port. It drops the connection it is asked over.
func (c *Client) TCPIP(ctx context.Context, serial string, port int) error {
	out, err := c.Run(ctx, "-s", serial, "tcpip", strconv.Itoa(port))
	if err != nil || !strings.Contains(out, "restarting in TCP mode") {
		return fmt.Errorf("adb tcpip %d: %s", port, firstLine(out, err))
	}
	return nil
}

// Connect finds a working port for host: the ports given first (last good, configured),
// then mDNS, then a scan of the usual wireless-debugging range. It returns the port.
func (c *Client) Connect(ctx context.Context, host string, known ...int) (int, error) {
	tried := map[int]bool{}
	attempt := func(p int) (bool, error) {
		if p <= 0 || tried[p] {
			return false, nil
		}
		tried[p] = true
		out, _ := c.Run(ctx, "connect", net.JoinHostPort(host, strconv.Itoa(p)))
		switch {
		case strings.Contains(out, "unauthorized"), strings.Contains(out, "failed to authenticate"):
			return false, ErrNotPaired
		case strings.Contains(out, "connected to"):
			return true, nil
		}
		return false, nil
	}
	for _, p := range known {
		if ok, err := attempt(p); ok || err != nil {
			return p, err
		}
		// adb keeps an offline entry for an address it could not reach, and wireless debugging
		// picks a new port every time Android switches it back on: drop the old one so "adb
		// devices" stays truthful and nothing later talks to a serial that cannot work.
		_ = c.Disconnect(ctx, host, p)
	}
	if p := c.mdnsPort(ctx, host, ServiceConnect); p > 0 {
		if ok, err := attempt(p); ok || err != nil {
			return p, err
		}
	}
	slog.Info("scanning for the wireless debugging port", "host", host)
	for _, p := range c.scan(ctx, host) {
		if ok, err := attempt(p); ok || err != nil {
			return p, err
		}
	}
	return 0, ErrUnreachable
}

// Reachable looks for the wireless debugging port without opening an ADB session, so the TV
// shows no "debugging connected" notice: a plain TCP connection to the remembered ports, then
// the mDNS listing. It cannot tell whether this machine is still paired.
func (c *Client) Reachable(ctx context.Context, host string, known ...int) (port int, ok bool) {
	dialFn := c.Dial
	if dialFn == nil {
		dialFn = dial
	}
	for _, p := range known {
		if p > 0 && dialFn(host, p) {
			return p, true
		}
	}
	if p := c.mdnsPort(ctx, host, ServiceConnect); p > 0 && dialFn(host, p) {
		return p, true
	}
	// A scan is plain TCP, so it still opens no ADB session and the TV still shows no notice.
	// It cannot tell adbd from any other open port, which is why it comes last.
	for _, p := range c.scan(ctx, host) {
		return p, true
	}
	return 0, false
}

func (c *Client) scan(ctx context.Context, host string) []int {
	dialFn := c.Dial
	if dialFn == nil {
		dialFn = dial
	}
	var (
		mu   sync.Mutex
		open []int
		wg   sync.WaitGroup
		jobs = make(chan int)
	)
	for range scanWorkers {
		wg.Go(func() {
			for p := range jobs {
				if dialFn(host, p) {
					mu.Lock()
					open = append(open, p)
					mu.Unlock()
				}
			}
		})
	}
feed:
	for p := c.ScanLo; p <= c.ScanHi; p++ {
		select {
		case jobs <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	slices.Sort(open)
	return open
}

func (c *Client) shell(ctx context.Context, serial, cmd string) (string, error) {
	out, err := c.Run(ctx, "-s", serial, "shell", cmd)
	if err != nil {
		return "", fmt.Errorf("adb shell %q: %s", cmd, firstLine(out, err))
	}
	return out, nil
}

// Shell runs a command on the device and returns its output.
func (c *Client) Shell(ctx context.Context, serial, cmd string) (string, error) {
	return c.shell(ctx, serial, cmd)
}

// ABIs returns ro.product.cpu.abilist.
func (c *Client) ABIs(ctx context.Context, serial string) ([]string, error) {
	out, err := c.shell(ctx, serial, "getprop ro.product.cpu.abilist")
	if err != nil {
		return nil, err
	}
	var abis []string
	for a := range strings.SplitSeq(out, ",") {
		if a = strings.TrimSpace(a); a != "" {
			abis = append(abis, a)
		}
	}
	return abis, nil
}

// SDK returns the device's API level.
func (c *Client) SDK(ctx context.Context, serial string) (int, error) {
	out, err := c.shell(ctx, serial, "getprop ro.build.version.sdk")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

var versionName = regexp.MustCompile(`versionName=(\S.*)`)

// InstalledVersion returns the installed versionName, or "" if the package is not installed.
func (c *Client) InstalledVersion(ctx context.Context, serial, pkg string) (string, error) {
	// Separate arguments, not one string: adb hands a single string to the device's shell, and the
	// package name can come from a patched APK's manifest.
	out, err := c.Run(ctx, "-s", serial, "shell", "dumpsys", "package", pkg)
	if err != nil {
		return "", fmt.Errorf("adb shell dumpsys package %q: %s", pkg, firstLine(out, err))
	}
	if m := versionName.FindStringSubmatch(out); m != nil {
		return strings.TrimSpace(m[1]), nil
	}
	return "", nil
}

var (
	playingSession = regexp.MustCompile(`PlaybackState \{state=3\b`) // 3 = PlaybackState.STATE_PLAYING
	startedPlayer  = regexp.MustCompile(`state:started\b`)           // an audio player that is running
)

// Playing says whether the TV is playing something right now: a media session in the playing state,
// or (for apps that publish no session) an audio player that has started. Paused, idle and the
// screensaver do not count. An error means the media service could not be asked.
func (c *Client) Playing(ctx context.Context, serial string) (bool, error) {
	sessions, err := c.shell(ctx, serial, "dumpsys media_session")
	if err != nil {
		return false, err
	}
	audio, _ := c.shell(ctx, serial, "dumpsys audio") // only a second opinion, so its failure is not one
	return playingFrom(sessions, audio), nil
}

func playingFrom(mediaSession, audio string) bool {
	return playingSession.MatchString(mediaSession) || startedPlayer.MatchString(audio)
}

// Pull copies a file off the device. The boot helper APK is around 12 KB, which is the only
// thing this is used for.
func (c *Client) Pull(ctx context.Context, serial, devicePath, localPath string) error {
	out, err := c.Run(ctx, "-s", serial, "pull", devicePath, localPath)
	if err != nil {
		return fmt.Errorf("adb pull %s: %s", devicePath, firstLine(out, err))
	}
	if _, err := os.Stat(localPath); err != nil {
		return fmt.Errorf("adb pull %s: %s", devicePath, firstLine(out, err))
	}
	return nil
}

// Install runs `adb install -r`, so it updates an app that is already there. ok is false with the
// adb output on failure, which is what Android answers with when the update cannot be done over
// the top: a different signing certificate, or a versionCode lower than the installed one.
func (c *Client) Install(ctx context.Context, serial, apkPath string) (ok bool, output string) {
	out, err := c.Run(ctx, "-s", serial, "install", "-r", apkPath)
	return err == nil && strings.Contains(out, "Success"), out
}

// Uninstall removes a package.
func (c *Client) Uninstall(ctx context.Context, serial, pkg string) error {
	out, err := c.Run(ctx, "-s", serial, "uninstall", pkg)
	if err != nil {
		return fmt.Errorf("uninstall %s: %s", pkg, firstLine(out, err))
	}
	return nil
}

func firstLine(out string, err error) string {
	if out != "" {
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[:i]
		}
		return out
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// scanWorkers is how many ports the scan dials at once: the whole ephemeral range at a 400ms
// dial timeout would take minutes one at a time.
const scanWorkers = 800
