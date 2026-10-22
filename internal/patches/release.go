package patches

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Release is a patches release that carries an .mpp bundle.
type Release struct {
	Tag      string
	AssetURL string
	Name     string
	Size     int64
}

// APIBase is overridden in tests.
var APIBase = "https://api.github.com"

func get(ctx context.Context, hc *http.Client, url, token string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "morphe-tv-patcher")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

type releaseJSON struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// LatestRelease returns the newest release (GitHub lists newest first) that has an .mpp asset.
func LatestRelease(ctx context.Context, hc *http.Client, repo, token string, prerelease bool) (*Release, error) {
	body, err := get(ctx, hc, fmt.Sprintf("%s/repos/%s/releases?per_page=10", APIBase, repo), token, 4<<20)
	if err != nil {
		return nil, err
	}
	var rels []releaseJSON
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, err
	}
	for _, r := range rels {
		if r.Draft || (r.Prerelease && !prerelease) {
			continue
		}
		for _, a := range r.Assets {
			if strings.HasSuffix(a.Name, ".mpp") {
				return &Release{Tag: r.TagName, AssetURL: a.URL, Name: a.Name, Size: a.Size}, nil
			}
		}
	}
	return nil, fmt.Errorf("no release with an .mpp asset in %s", repo)
}

const metaFile = "release.json"

type releaseMeta struct {
	Tag  string `json:"tag"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func writeMeta(dir string, rel *Release) {
	b, err := json.Marshal(releaseMeta{Tag: rel.Tag, Name: rel.Name, Size: rel.Size})
	if err == nil {
		_ = os.WriteFile(filepath.Join(dir, metaFile), b, 0o600)
	}
}

// Cached returns the last bundle downloaded into dir (its release and path), so a patch source
// that has gone away or cannot be reached still works with the last good bundle.
func Cached(dir string) (rel *Release, path string, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, metaFile)) //nolint:gosec // our own cache directory
	if err != nil {
		return nil, "", false
	}
	var m releaseMeta
	if json.Unmarshal(b, &m) != nil || m.Name == "" || m.Name != filepath.Base(m.Name) {
		return nil, "", false
	}
	p := filepath.Join(dir, m.Name)
	if st, err := os.Stat(p); err != nil || (m.Size > 0 && st.Size() != m.Size) {
		return nil, "", false
	}
	return &Release{Tag: m.Tag, Name: m.Name, Size: m.Size}, p, true
}

// MaxBundleBytes caps a patch bundle download.
const MaxBundleBytes = 100 << 20

// Download fetches the bundle into cacheDir (skipping it if already complete) and removes
// older .mpp files in that folder (give every patch source its own folder). It returns the path.
func Download(ctx context.Context, hc *http.Client, rel *Release, cacheDir string) (string, error) {
	if rel.Size > MaxBundleBytes {
		return "", fmt.Errorf("bundle %s is %d MB, over the %d MB limit", rel.Name, rel.Size>>20, MaxBundleBytes>>20)
	}
	if rel.Name != filepath.Base(rel.Name) || !strings.HasSuffix(rel.Name, ".mpp") {
		return "", fmt.Errorf("unexpected bundle name %q", rel.Name)
	}
	dest := filepath.Join(cacheDir, rel.Name)
	if st, err := os.Stat(dest); err == nil && st.Size() == rel.Size {
		writeMeta(cacheDir, rel)
		return dest, nil
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.AssetURL, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "morphe-tv-patcher")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", rel.Name, resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp) //nolint:gosec // inside our own cache directory
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, MaxBundleBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if rel.Size > 0 {
		if st, _ := os.Stat(tmp); st == nil || st.Size() != rel.Size {
			_ = os.Remove(tmp)
			return "", errors.New("download size mismatch")
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	writeMeta(cacheDir, rel)
	old, _ := filepath.Glob(filepath.Join(cacheDir, "*.mpp"))
	for _, o := range old {
		if o != dest {
			_ = os.Remove(o)
		}
	}
	return dest, nil
}
