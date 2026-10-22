package apkmirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/sloccy/morphe-tv-patcher/internal/patches"
)

// BaseURL is the site root (tests point it at a local server).
const BaseURL = "https://www.apkmirror.com"

// Client drives the page chain: release page -> variant -> download page -> file.
type Client struct {
	F     Fetcher
	Base  string
	Sleep func(ctx context.Context, lo, hi time.Duration) // jittered pause between page loads
}

func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	return BaseURL
}

func (c *Client) pause(ctx context.Context, lo, hi time.Duration) {
	if c.Sleep != nil {
		c.Sleep(ctx, lo, hi)
		return
	}
	sleep(ctx, lo+time.Duration(rand.Int64N(int64(hi-lo)+1)))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// AppRequest says which app version to get.
type AppRequest struct {
	Package   string
	Name      string // display name, used to recognise search results
	Version   string
	KnownSlug string // cached or overridden publisher/app path, tried first
	Hint      string // built-in guess, tried next
	SDK       int    // device API level; 0 = unknown
	DestDir   string
}

// Result is a downloaded bundle and the app page that produced it.
type Result struct {
	Path string
	Slug string
}

var releaseHref = regexp.MustCompile(`^/apk/([^/]+/[^/]+)/[^/]+-release/?$`)

// FindRelease finds the release page for a version under an app page. The URL is guessed
// first; if that 404s the app's own page is searched (release URLs often differ from the
// app's slug, e.g. Prime Video).
func (c *Client) FindRelease(ctx context.Context, slug, version string) (string, *Page, error) {
	tail := slug[strings.LastIndex(slug, "/")+1:]
	vs := patches.Slug(version)
	guess := fmt.Sprintf("%s/apk/%s/%s-%s-release/", c.base(), slug, tail, vs)
	p, err := c.F.Page(ctx, guess, c.base()+"/")
	if err == nil {
		return guess, p, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", nil, err
	}
	// the numeric core (5.66.0 of 5.66.0-leanback) also matches, verify() checks the exact version
	cores := []string{vs}
	if m := regexp.MustCompile(`^\d+(?:\.\d+)*`).FindString(version); m != "" && patches.Slug(m) != vs {
		cores = append(cores, patches.Slug(m))
	}
	for page := 1; page <= 3; page++ {
		c.pause(ctx, 2*time.Second, 6*time.Second)
		u := fmt.Sprintf("%s/apk/%s/", c.base(), slug)
		if page > 1 {
			u += fmt.Sprintf("page/%d/", page)
		}
		app, err := c.F.Page(ctx, u, c.base()+"/")
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				break
			}
			return "", nil, err
		}
		for _, core := range cores {
			for _, a := range find(app.Doc, isA) {
				h := strings.SplitN(attr(a, "href"), "#", 2)[0]
				if !strings.Contains(h, "/"+slug+"/") {
					continue
				}
				t := strings.TrimSuffix(h, "/")
				if strings.HasSuffix(t, "-"+core+"-release") || strings.HasSuffix(t, "-"+core+"-apk-release") {
					full := c.base() + h
					c.pause(ctx, 2*time.Second, 6*time.Second)
					rp, err := c.F.Page(ctx, full, c.base()+"/")
					return full, rp, err
				}
			}
		}
	}
	return "", nil, fmt.Errorf("%w: %s for %s", ErrNotFound, version, slug)
}

var androidMin = regexp.MustCompile(`android\s+(\d+)(?:\.\d+)?(l?)\+`)

// APILevel maps the "Android 12L+" style minimum shown on APKMirror to an API level.
func APILevel(major int, l bool) int {
	m := map[int]int{4: 19, 5: 21, 6: 23, 7: 24, 8: 26, 9: 28, 10: 29, 11: 30, 12: 31, 13: 33, 14: 34, 15: 35, 16: 36}
	api := m[major]
	if major == 12 && l {
		api = 32
	}
	return api
}

// PickVariant chooses the variant page: a BUNDLE row whose architecture we can run and whose
// minimum Android fits the device. Rows naming the exact version win; then rows listing both
// arm64-v8a and armeabi-v7a (or universal) over armeabi-v7a only.
func PickVariant(rel *Page, version string, sdk int) (string, error) {
	type cand struct {
		href  string
		score int
		exact bool
	}
	var cands []cand
	anyExact := false
	for _, row := range find(rel.Doc, func(n *html.Node) bool { return n.Data == "div" && hasClass(n, "table-row", "headerFont") }) {
		t := strings.ToLower(text(row))
		if !strings.Contains(t, "bundle") {
			continue
		}
		link := first(row, func(n *html.Node) bool { return isA(n) && hasClass(n, "accent_color") })
		if link == nil {
			continue
		}
		score := 0
		switch {
		case strings.Contains(t, "armeabi-v7a") && strings.Contains(t, "arm64-v8a"),
			strings.Contains(t, "universal"), strings.Contains(t, "noarch"):
			score = 2
		case strings.Contains(t, "armeabi-v7a"):
			score = 1
		default:
			continue
		}
		if m := androidMin.FindStringSubmatch(t); m != nil && sdk > 0 {
			major, _ := strconv.Atoi(m[1])
			if need := APILevel(major, m[2] == "l"); need > sdk {
				continue
			}
		}
		exact := version != "" && strings.Contains(t, strings.ToLower(version))
		anyExact = anyExact || exact
		cands = append(cands, cand{attr(link, "href"), score, exact})
	}
	best := -1
	for i, c := range cands {
		if anyExact && !c.exact {
			continue
		}
		if best < 0 || c.score > cands[best].score {
			best = i
		}
	}
	if best < 0 {
		title := rel.Title
		if len(title) > 80 {
			title = title[:80]
		}
		return "", fmt.Errorf("%w: no compatible BUNDLE variant on the release page (title %q)", ErrNotFound, title)
	}
	return cands[best].href, nil
}

func downloadButton(p *Page) (string, error) {
	pred := func(n *html.Node) bool { return isA(n) && hasClass(n, "downloadButton") }
	for _, a := range find(p.Doc, pred) {
		if strings.Contains(attr(a, "href"), "/download/") {
			return attr(a, "href"), nil
		}
	}
	if a := first(p.Doc, pred); a != nil {
		return attr(a, "href"), nil
	}
	return "", fmt.Errorf("%w: download button missing", ErrNotFound)
}

func fileLink(p *Page) (string, error) {
	for _, a := range find(p.Doc, isA) {
		if strings.Contains(attr(a, "href"), "download.php") {
			return attr(a, "href"), nil
		}
	}
	for _, a := range find(p.Doc, isA) {
		if attr(a, "rel") == "nofollow" && strings.Contains(attr(a, "href"), "key=") {
			return attr(a, "href"), nil
		}
	}
	return "", fmt.Errorf("%w: final download link missing", ErrBlocked)
}

// fetchSlug runs the whole chain for one candidate app page.
func (c *Client) fetchSlug(ctx context.Context, req AppRequest, slug string) (string, error) {
	rel, rp, err := c.FindRelease(ctx, slug, req.Version)
	if err != nil {
		return "", err
	}
	if !strings.Contains(rp.Raw, req.Package) {
		return "", fmt.Errorf("%w: %s is a different app (page never mentions %s)", ErrNotFound, slug, req.Package)
	}
	slog.Info("apkmirror release page", "url", rel)
	c.pause(ctx, 2*time.Second, 6*time.Second)
	vhref, err := PickVariant(rp, req.Version, req.SDK)
	if err != nil {
		return "", err
	}
	variant := c.base() + vhref
	vp, err := c.F.Page(ctx, variant, rel)
	if err != nil {
		return "", err
	}
	btn, err := downloadButton(vp)
	if err != nil {
		return "", err
	}
	c.pause(ctx, 2*time.Second, 6*time.Second)
	dlURL := c.base() + btn
	dp, err := c.F.Page(ctx, dlURL, variant)
	if err != nil {
		return "", err
	}
	fl, err := fileLink(dp)
	if err != nil {
		return "", err
	}
	c.pause(ctx, 2*time.Second, 4*time.Second)
	dest := filepath.Join(req.DestDir, slugFile(slug)+"-"+patches.Slug(req.Version)+".apkm")
	if err := c.F.File(ctx, c.base()+fl, dlURL, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func slugFile(slug string) string { return strings.ReplaceAll(slug, "/", "_") }

// Fetch downloads the bundle: the known and hinted app pages first, then search results.
// ErrBlocked is returned as soon as it happens so the caller can rotate or back off.
func (c *Client) Fetch(ctx context.Context, req AppRequest) (Result, error) {
	c.pause(ctx, time.Second, 3*time.Second)
	if _, err := c.F.Page(ctx, c.base()+"/", ""); err != nil { // warm-up: cookies before deep pages
		return Result{}, err
	}
	tried := map[string]bool{}
	var lastNotFound error
	try := func(slug string) (Result, bool, error) {
		if slug == "" || tried[slug] {
			return Result{}, false, nil
		}
		tried[slug] = true
		c.pause(ctx, 2*time.Second, 6*time.Second)
		path, err := c.fetchSlug(ctx, req, slug)
		switch {
		case err == nil:
			return Result{Path: path, Slug: slug}, true, nil
		case errors.Is(err, ErrNotFound):
			slog.Info("apkmirror candidate rejected", "slug", slug, "err", err)
			lastNotFound = err
			return Result{}, false, nil
		}
		return Result{}, false, err
	}
	for _, slug := range []string{req.KnownSlug, req.Hint} {
		if r, ok, err := try(slug); ok || err != nil {
			return r, err
		}
	}
	cands, err := c.Candidates(ctx, req.Package, req.Name)
	if err != nil {
		return Result{}, err
	}
	for _, slug := range cands {
		if r, ok, err := try(slug); ok || err != nil {
			return r, err
		}
	}
	if lastNotFound != nil {
		return Result{}, lastNotFound
	}
	return Result{}, fmt.Errorf("%w: no APKMirror page found for %s", ErrNotFound, req.Package)
}

var genericWords = map[string]bool{"android": true, "tv": true, "app": true, "com": true, "org": true, "net": true,
	"the": true, "www": true, "mobile": true, "player": true, "production": true}

// tokens returns lowercase words that identify an app, for matching search results.
func tokens(pkg, name string) []string {
	var out []string
	add := func(w string) {
		w = strings.ToLower(w)
		if len(w) >= 2 && !genericWords[w] {
			out = append(out, w)
		}
	}
	for _, w := range regexp.MustCompile(`[A-Za-z0-9]+`).FindAllString(name, -1) {
		add(w)
	}
	for _, seg := range strings.Split(pkg, ".") {
		if len(seg) >= 4 {
			add(seg)
		}
	}
	return out
}

// Candidates searches APKMirror by package id and returns up to four publisher/app paths
// that look like this app, Android TV pages first. Results are only guesses: the release
// page must also name the package, and the downloaded file is verified.
func (c *Client) Candidates(ctx context.Context, pkg, name string) ([]string, error) {
	c.pause(ctx, 2*time.Second, 6*time.Second)
	q := url.Values{"post_type": {"app_release"}, "searchtype": {"apk"}, "s": {pkg}}
	p, err := c.F.Page(ctx, c.base()+"/?"+q.Encode(), c.base()+"/")
	if err != nil {
		return nil, err
	}
	toks := tokens(pkg, name)
	seen := map[string]bool{}
	var tv, other []string
	for _, a := range find(p.Doc, isA) {
		m := regexp.MustCompile(`^/apk/([^/]+/[^/]+)/`).FindStringSubmatch(attr(a, "href"))
		if m == nil || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		match := false
		for _, t := range toks {
			if strings.Contains(strings.ReplaceAll(m[1], "-", ""), t) || strings.Contains(m[1], t) {
				match = true
			}
		}
		if !match {
			continue // sidebar noise (trending apps) shows up in every search
		}
		if strings.Contains(m[1], "android-tv") {
			tv = append(tv, m[1])
		} else {
			other = append(other, m[1])
		}
	}
	out := append(tv, other...)
	if len(out) > 4 {
		out = out[:4]
	}
	return out, nil
}

// Options tune Download.
type Options struct {
	FlareURL    string          // use a FlareSolverr sidecar instead of direct requests
	ProfileHint string          // browser profile that last worked
	Backoff     []time.Duration // waits between blocked attempts (default 30s, 90s)
	Base        string
	Sleep       func(ctx context.Context, lo, hi time.Duration)
}

// Download gets the bundle. Direct mode rotates through browser profiles when blocked; with
// FlareURL set it makes one attempt through the sidecar. It returns what worked ("chrome",
// "flaresolverr", ...) for the caller to remember.
func Download(ctx context.Context, req AppRequest, o Options) (Result, string, error) {
	if o.FlareURL != "" {
		c := &Client{F: NewFlare(o.FlareURL), Base: o.Base, Sleep: o.Sleep}
		r, err := c.Fetch(ctx, req)
		return r, "flaresolverr", err
	}
	backoff := o.Backoff
	if backoff == nil {
		backoff = []time.Duration{30 * time.Second, 90 * time.Second}
	}
	order := append([]Profile{}, Profiles...)
	for i, p := range order {
		if p.Name == o.ProfileHint && i > 0 {
			order = append([]Profile{p}, append(order[:i:i], order[i+1:]...)...)
			break
		}
	}
	var last error
	for i, p := range order {
		c := &Client{F: NewDirect(p), Base: o.Base, Sleep: o.Sleep}
		r, err := c.Fetch(ctx, req)
		if err == nil {
			return r, p.Name, nil
		}
		last = err
		if !errors.Is(err, ErrBlocked) {
			return Result{}, p.Name, err
		}
		if i < len(order)-1 {
			wait := backoff[min(i, len(backoff)-1)]
			wait = time.Duration(float64(wait) * (0.8 + 0.5*rand.Float64()))
			slog.Warn("blocked; backing off", "profile", p.Name, "wait", wait.Round(time.Second), "err", err)
			sleep(ctx, wait)
		}
	}
	return Result{}, "", last
}
