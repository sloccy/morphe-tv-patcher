package patches

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// App is one app the patch bundle supports.
type App struct {
	Package      string   `json:"package"`
	Name         string   `json:"name"`
	Versions     []string `json:"versions"` // oldest first
	Newest       string   `json:"newest"`
	Experimental bool     `json:"experimental"` // the newest version is marked experimental
}

// List is the parsed patches-list.json.
type List struct {
	Version string
	Apps    []App
	byPkg   map[string]*App
}

type listFile struct {
	Version  string            `json:"version"`
	AppNames map[string]string `json:"appNames"`
	Patches  []struct {
		CompatiblePackages map[string][]string `json:"compatiblePackages"`
		Compatibility      []struct {
			PackageName string `json:"packageName"`
			Targets     []struct {
				Version      string `json:"version"`
				Experimental bool   `json:"experimental"`
			} `json:"targets"`
		} `json:"compatibility"`
	} `json:"patches"`
}

// ParseList decodes patches-list.json. Versions come from both "compatibility" and
// "compatiblePackages" so an older or newer file format still works.
func ParseList(data []byte) (*List, error) {
	var f listFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("patches-list.json: %w", err)
	}
	versions := map[string][]string{}
	experimental := map[string]map[string]bool{}
	for _, p := range f.Patches {
		for pkg, vs := range p.CompatiblePackages {
			versions[pkg] = append(versions[pkg], vs...)
		}
		for _, c := range p.Compatibility {
			for _, t := range c.Targets {
				versions[c.PackageName] = append(versions[c.PackageName], t.Version)
				if t.Experimental {
					if experimental[c.PackageName] == nil {
						experimental[c.PackageName] = map[string]bool{}
					}
					experimental[c.PackageName][t.Version] = true
				}
			}
		}
	}
	l := &List{Version: f.Version, byPkg: map[string]*App{}}
	for pkg, vs := range versions {
		stable := map[string]bool{}
		for _, v := range vs {
			if !experimental[pkg][v] {
				stable[v] = true
			}
		}
		a := buildApp(pkg, f.AppNames[pkg], vs, stable)
		if f.AppNames[pkg] == "" {
			a.Name = pkg
		}
		l.Apps = append(l.Apps, a)
	}
	sort.Slice(l.Apps, func(i, j int) bool { return l.Apps[i].Name < l.Apps[j].Name })
	for i := range l.Apps {
		l.byPkg[l.Apps[i].Package] = &l.Apps[i]
	}
	return l, nil
}

// App returns the app for a package, if the bundle supports it.
func (l *List) App(pkg string) (App, bool) {
	a, ok := l.byPkg[pkg]
	if !ok {
		return App{}, false
	}
	return *a, true
}

// FetchList downloads patches-list.json as of a release tag.
func FetchList(ctx context.Context, hc *http.Client, repo, tag, token string) (*List, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/patches-list.json", repo, tag)
	body, err := get(ctx, hc, url, token, 8<<20)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return nil, ErrNoList
		}
		return nil, err
	}
	return ParseList(body)
}
