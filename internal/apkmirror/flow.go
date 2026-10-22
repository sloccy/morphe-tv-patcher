package apkmirror

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/sloccy/morphe-tv-patcher/internal/abi"
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
	sleep(ctx, lo+time.Duration(rand.Int64N(int64(hi-lo)+1))) //nolint:gosec // jitter between requests, not security
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
	Package    string
	Name       string // display name, used to recognise search results
	Version    string
	KnownSlug  string // cached or overridden publisher/app path, tried first
	Hint       string // built-in guess, tried next
	SDK        int    // device API level; 0 = unknown
	DestDir    string
	ABIs       []string // the device's ABIs in order of preference; nil = the 32-bit default
	AllowPhone bool     // phone apps are acceptable (affects which search results come first)
	DryRun     bool     // find the page and variant but do not download (Result.Path is the release URL)
}

// Result is a downloaded bundle and the app page that produced it.
type Result struct {
	Path string
	Slug string
}

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
		hrefs := make([]string, 0, 32)
		for _, a := range find(app.Doc, isA) {
			h, _, _ := strings.Cut(attr(a, "href"), "#")
			if strings.Contains(h, "/"+slug+"/") {
				hrefs = append(hrefs, h)
			}
		}
		if h := pickRelease(hrefs, cores); h != "" {
			full := c.base() + h
			c.pause(ctx, 2*time.Second, 6*time.Second)
			rp, err := c.F.Page(ctx, full, c.base()+"/")
			return full, rp, err
		}
	}
	return "", nil, fmt.Errorf("%w: %s for %s", ErrNotFound, version, slug)
}

// pickRelease chooses the release link for a version from an app page's links. For each
// candidate version slug an exact match wins, then ones where APKMirror inserted "-apk", then
// a trailing "-2" style counter it adds to a re-uploaded version (paramount-...-16-19-0-2-release).
func pickRelease(hrefs, cores []string) string {
	for _, core := range cores {
		for _, tier := range []*regexp.Regexp{
			regexp.MustCompile(`-` + regexp.QuoteMeta(core) + `-release$`),
			regexp.MustCompile(`-` + regexp.QuoteMeta(core) + `-apk-release$`),
			regexp.MustCompile(`-` + regexp.QuoteMeta(core) + `(-apk)?-\d+-release$`),
		} {
			for _, h := range hrefs {
				if tier.MatchString(strings.TrimSuffix(h, "/")) {
					return h
				}
			}
		}
	}
	return ""
}

var variantType = regexp.MustCompile(`\b(bundle|apk)\b`)

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

// PickVariant chooses the variant page: a BUNDLE or APK row that runs on the device (its CPU
// list, in order of preference) and whose minimum Android fits. Rows naming the exact version
// win. Then the row with the best CPU match wins (a native 64-bit build for a 64-bit device),
// then the one offering more ABIs, then the first listed. A nil abis means the 32-bit default.
func PickVariant(rel *Page, version string, sdk int, abis []string) (string, error) {
	type cand struct {
		href  string
		score int
		nabis int
		exact bool
	}
	var cands []cand
	anyExact := false
	archSkipped, sdkSkipped := 0, 0
	for _, row := range find(rel.Doc, func(n *html.Node) bool { return n.Data == "div" && hasClass(n, "table-row", "headerFont") }) {
		t := strings.ToLower(text(row))
		if !variantType.MatchString(t) { // BUNDLE (split apks) or APK (single file)
			continue
		}
		link := first(row, func(n *html.Node) bool { return isA(n) && hasClass(n, "accent_color") })
		if link == nil {
			continue
		}
		r := abi.ParseRow(t)
		score, ok := r.Score(abis)
		if !ok {
			archSkipped++
			continue
		}
		if m := androidMin.FindStringSubmatch(t); m != nil && sdk > 0 {
			major, _ := strconv.Atoi(m[1])
			if need := APILevel(major, m[2] == "l"); need > sdk {
				sdkSkipped++
				continue
			}
		}
		exact := version != "" && strings.Contains(t, strings.ToLower(version))
		anyExact = anyExact || exact
		cands = append(cands, cand{attr(link, "href"), score, len(r.ABIs), exact})
	}
	best := -1
	for i, c := range cands {
		if anyExact && !c.exact {
			continue
		}
		if best < 0 || c.score < cands[best].score || (c.score == cands[best].score && c.nabis > cands[best].nabis) {
			best = i
		}
	}
	if best >= 0 {
		return cands[best].href, nil
	}
	title := rel.Title
	if len(title) > 80 {
		title = title[:80]
	}
	if archSkipped > 0 && sdkSkipped == 0 && len(cands) == 0 {
		return "", fmt.Errorf("%w: %w: the release has no build for %v (title %q)", ErrNoBuild, ErrNotFound, abi.Normalize(abis), title)
	}
	return "", fmt.Errorf("%w: no compatible variant (BUNDLE or APK for %v, Android %d) on the release page (title %q)", ErrNotFound, abi.Normalize(abis), sdk, title)
}

// hasVariantTable reports whether the page lists variants (even unusable ones).
func hasVariantTable(p *Page) bool {
	for _, row := range find(p.Doc, func(n *html.Node) bool { return n.Data == "div" && hasClass(n, "table-row", "headerFont") }) {
		if variantType.MatchString(strings.ToLower(text(row))) {
			return true
		}
	}
	return false
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
	variant := rel
	var btn string
	variantKey := "single" // which variant this is, so different builds never share a cache name
	vhref, err := PickVariant(rp, req.Version, req.SDK, req.ABIs)
	switch {
	case err == nil:
		variantKey = vhref
		variant = c.base() + vhref
		vp, err := c.F.Page(ctx, variant, rel)
		if err != nil {
			return "", err
		}
		if btn, err = downloadButton(vp); err != nil {
			return "", err
		}
	case errors.Is(err, ErrNotFound) && !hasVariantTable(rp):
		// a release with a single variant has no table: the download button is on this page
		if btn, err = downloadButton(rp); err != nil || !strings.Contains(btn, "/download/") {
			return "", fmt.Errorf("%w: no variants and no download button on the release page", ErrNotFound)
		}
	default:
		return "", err
	}
	if req.DryRun {
		return rel, nil
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
	h := fnv.New32a()
	_, _ = h.Write([]byte(variantKey))
	dest := filepath.Join(req.DestDir, fmt.Sprintf("%s-%s-%08x.apkm", slugFile(slug), patches.Slug(req.Version), h.Sum32()))
	if have := existing(dest); have != "" {
		return have, nil // the same release and variant is already cached
	}
	if err := c.F.File(ctx, c.base()+fl, dlURL, dest); err != nil {
		return "", err
	}
	return fixExtension(dest), nil
}

// existing returns dest (or its plain-APK twin) if it is already on disk.
func existing(dest string) string {
	for _, p := range []string{dest, strings.TrimSuffix(dest, filepath.Ext(dest)) + ".apk"} {
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			return p
		}
	}
	return ""
}

// fixExtension renames a plain APK (AndroidManifest.xml at the top) from .apkm to .apk, since
// Morphe merges split bundles by extension.
func fixExtension(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return path
	}
	plain := false
	for _, f := range zr.File {
		if f.Name == "AndroidManifest.xml" {
			plain = true
			break
		}
	}
	_ = zr.Close()
	if !plain {
		return path
	}
	apk := strings.TrimSuffix(path, filepath.Ext(path)) + ".apk"
	if os.Rename(path, apk) != nil {
		return path
	}
	return apk
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
	for seg := range strings.SplitSeq(pkg, ".") {
		if len(seg) >= 4 {
			add(seg)
		}
	}
	return out
}

// matches reports whether a publisher/app path contains one of the identifying tokens as a
// whole word (so "fox" does not match "firefox"). Longer tokens may also match across the
// dashes of the path ("formulaone" in "formula-one").
func matches(slug string, toks []string) bool {
	words := map[string]bool{}
	for _, w := range regexp.MustCompile(`[a-z0-9]+`).FindAllString(strings.ToLower(slug), -1) {
		words[w] = true
	}
	joined := strings.ReplaceAll(strings.ToLower(slug), "-", "")
	joined = strings.ReplaceAll(joined, "/", "")
	for _, t := range toks {
		if words[t] || (len(t) >= 7 && strings.Contains(joined, t)) {
			return true
		}
	}
	return false
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
		if len(m) == 0 || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		if !matches(m[1], toks) {
			continue // sidebar noise (trending apps) shows up in every search
		}
		if strings.Contains(m[1], "android-tv") {
			tv = append(tv, m[1])
		} else {
			other = append(other, m[1])
		}
	}
	out := slices.Concat(tv, other)
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
			wait = time.Duration(float64(wait) * (0.8 + 0.5*rand.Float64())) //nolint:gosec // jitter, not security
			slog.Warn("blocked; backing off", "profile", p.Name, "wait", wait.Round(time.Second), "err", err)
			sleep(ctx, wait)
		}
	}
	return Result{}, "", last
}
