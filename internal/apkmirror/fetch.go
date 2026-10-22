// Package apkmirror downloads stock Android TV app bundles from APKMirror. The site has no
// API and sits behind Cloudflare, so requests look like a browser, back off when limited,
// and can fall back to a FlareSolverr sidecar.
package apkmirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/sloccy/morphe-tv-patcher/internal/flaresolverr"
)

var (
	// ErrBlocked: Cloudflare challenge or rate limit. Temporary; try another route or wait.
	ErrBlocked = errors.New("blocked by Cloudflare or rate limit")
	// ErrNotFound: APKMirror does not have this page/version (yet).
	ErrNotFound = errors.New("not found on APKMirror")
)

// Page is a fetched, parsed HTML page.
type Page struct {
	URL   string
	Raw   string
	Doc   *html.Node
	Title string
}

func parsePage(url, raw string) (*Page, error) {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	p := &Page{URL: url, Raw: raw, Doc: doc}
	if t := first(doc, func(n *html.Node) bool { return n.Data == "title" }); t != nil {
		p.Title = text(t)
	}
	return p, nil
}

// classify turns a status and body into ErrBlocked / ErrNotFound / nil.
func classify(url string, status int, raw string) error {
	head := raw
	if len(head) > 3000 {
		head = head[:3000]
	}
	switch {
	case status == 403 || status == 429 || status == 503 || strings.Contains(head, "Just a moment..."):
		return fmt.Errorf("%w: %s (HTTP %d)", ErrBlocked, url, status)
	case status == 404:
		return fmt.Errorf("%w: %s", ErrNotFound, url)
	case status != 200:
		return fmt.Errorf("%s: HTTP %d", url, status)
	}
	return nil
}

// Fetcher loads pages and downloads the final file.
type Fetcher interface {
	Page(ctx context.Context, url, referer string) (*Page, error)
	File(ctx context.Context, url, referer, dest string) error
}

// Profile is a set of browser-like request headers.
type Profile struct {
	Name    string
	Headers map[string]string
}

// Profiles are tried in order; the one that last worked goes first.
var Profiles = []Profile{
	{"chrome", map[string]string{
		"User-Agent":         "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
		"Accept":             "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"Accept-Language":    "en-US,en;q=0.9",
		"sec-ch-ua":          `"Chromium";v="130", "Google Chrome";v="130", "Not?A_Brand";v="99"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"macOS"`,
		"Sec-Fetch-Dest":     "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-User": "?1",
		"Upgrade-Insecure-Requests": "1",
	}},
	{"firefox", map[string]string{
		"User-Agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:132.0) Gecko/20100101 Firefox/132.0",
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
		"Sec-Fetch-Dest":  "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-User": "?1",
		"Upgrade-Insecure-Requests": "1",
	}},
	{"safari", map[string]string{
		"User-Agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15",
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.9",
	}},
}

// Direct talks to APKMirror itself.
type Direct struct {
	hc      *http.Client
	profile Profile
}

// NewDirect makes a fetcher with its own cookie jar.
func NewDirect(p Profile) *Direct {
	jar, _ := cookiejar.New(nil)
	return &Direct{hc: &http.Client{Jar: jar, Timeout: 30 * time.Minute}, profile: p}
}

func (d *Direct) request(ctx context.Context, url, referer string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range d.profile.Headers {
		req.Header.Set(k, v)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	return req, nil
}

// Page implements Fetcher.
func (d *Direct) Page(ctx context.Context, url, referer string) (*Page, error) {
	req, err := d.request(ctx, url, referer)
	if err != nil {
		return nil, err
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if err := classify(url, resp.StatusCode, string(b)); err != nil {
		return nil, err
	}
	return finishPage(url, string(b))
}

func finishPage(url, raw string) (*Page, error) {
	p, err := parsePage(url, raw)
	if err != nil {
		return nil, err
	}
	// through a real browser the 404 page can arrive as a 200
	if strings.Contains(p.Title, "Page Not Found") {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, url)
	}
	return p, nil
}

// File implements Fetcher: stream to dest, refusing anything that is not a zip.
func (d *Direct) File(ctx context.Context, url, referer, dest string) error {
	req, err := d.request(ctx, url, referer)
	if err != nil {
		return err
	}
	return d.save(req, dest)
}

func (d *Direct) save(req *http.Request, dest string) error {
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode == 503 {
		return fmt.Errorf("%w: download HTTP %d", ErrBlocked, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if !isZip(tmp) {
		os.Remove(tmp)
		return fmt.Errorf("%w: download was not a zip (interstitial page?)", ErrBlocked)
	}
	return os.Rename(tmp, dest)
}

func isZip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [2]byte
	_, err = io.ReadFull(f, b[:])
	return err == nil && b[0] == 'P' && b[1] == 'K'
}

// Flare loads pages in a FlareSolverr browser. For the file it first lets the browser visit
// the download URL (which can have its own Cloudflare challenge), then downloads with the
// cookies and user agent it ended up with.
type Flare struct {
	URL     string
	hc      *http.Client
	cookies map[string]string
	ua      string
	direct  *Direct
}

// NewFlare makes a fetcher using the sidecar at url.
func NewFlare(url string) *Flare {
	return &Flare{URL: url, hc: &http.Client{}, cookies: map[string]string{}, direct: NewDirect(Profiles[0])}
}

func (f *Flare) absorb(r *flaresolverr.Result) {
	for k, v := range r.Cookies {
		f.cookies[k] = v
	}
	if r.UserAgent != "" {
		f.ua = r.UserAgent
	}
}

// Page implements Fetcher.
func (f *Flare) Page(ctx context.Context, url, _ string) (*Page, error) {
	r, err := flaresolverr.Solve(ctx, f.hc, f.URL, url)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBlocked, err)
	}
	f.absorb(r)
	if err := classify(url, r.Status, r.HTML); err != nil {
		return nil, err
	}
	return finishPage(url, r.HTML)
}

// File implements Fetcher.
func (f *Flare) File(ctx context.Context, url, referer, dest string) error {
	if r, err := flaresolverr.Solve(ctx, f.hc, f.URL, url); err == nil {
		f.absorb(r)
	}
	req, err := f.direct.request(ctx, url, referer)
	if err != nil {
		return err
	}
	if f.ua != "" {
		req.Header.Set("User-Agent", f.ua)
	}
	for k, v := range f.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	return f.direct.save(req, dest)
}
