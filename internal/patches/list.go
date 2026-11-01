package patches

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Patch is one patch in a bundle: what it does and whether it is applied unless asked otherwise.
type Patch struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Default     bool   `json:"default"` // applied when the app's patch choice is left alone
}

// App is one app the patch bundle supports.
type App struct {
	Package      string   `json:"package"`
	Name         string   `json:"name"`
	Versions     []string `json:"versions"`     // oldest first
	Target       string   `json:"target"`       // the version to build by default (see Pick)
	Experimental bool     `json:"experimental"` // the target version is marked experimental
	Patches      []Patch  `json:"patches"`      // the patches written for this app
	stable       map[string]bool
}

// Pick returns the version to build: the newest stable one. Only when no version is stable is the
// newest of all of them used, and flagged experimental.
func (a App) Pick() (version string, experimental bool) {
	for _, v := range a.Versions { // oldest first
		if a.stable[v] {
			version = v
		}
	}
	if version == "" && len(a.Versions) > 0 {
		return a.Versions[len(a.Versions)-1], true
	}
	return version, false
}

// List is the parsed patches-list.json.
type List struct {
	Version   string
	Apps      []App
	Universal []Patch // patches that name no app and can go on any of them
	byPkg     map[string]*App
}

// SetPatches attaches what each app's patches are, and the patches that fit any app.
func (l *List) SetPatches(perPkg map[string][]Patch, universal []Patch) {
	l.Universal = universal
	for i := range l.Apps {
		l.Apps[i].Patches = perPkg[l.Apps[i].Package]
	}
}

type listFile struct {
	Version  string            `json:"version"`
	AppNames map[string]string `json:"appNames"`
	Patches  []struct {
		Name               string              `json:"name"`
		Description        string              `json:"description"`
		Use                *bool               `json:"use"`
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
	stableMark := map[string]map[string]bool{} // a version is stable if any patch lists it as stable
	mark := func(pkg, v string) {
		if stableMark[pkg] == nil {
			stableMark[pkg] = map[string]bool{}
		}
		stableMark[pkg][v] = true
	}
	perPkgPatches := map[string][]Patch{}
	var universal []Patch
	for _, p := range f.Patches {
		perPkg := map[string][]string{}
		inCompat := map[string]bool{}
		for _, c := range p.Compatibility {
			inCompat[c.PackageName] = true
			for _, t := range c.Targets {
				perPkg[c.PackageName] = append(perPkg[c.PackageName], t.Version)
				if !t.Experimental {
					mark(c.PackageName, t.Version)
				}
			}
		}
		for pkg, vs := range p.CompatiblePackages {
			if inCompat[pkg] {
				continue // the newer "compatibility" form says more (experimental flags)
			}
			perPkg[pkg] = append(perPkg[pkg], vs...)
			for _, v := range vs {
				mark(pkg, v)
			}
		}
		for pkg, vs := range perPkg {
			versions[pkg] = append(versions[pkg], vs...)
		}
		if p.Name != "" {
			info := Patch{Name: p.Name, Description: p.Description, Default: p.Use == nil || *p.Use}
			if len(perPkg) == 0 {
				universal = appendPatch(universal, info)
			}
			for pkg := range perPkg {
				perPkgPatches[pkg] = appendPatch(perPkgPatches[pkg], info)
			}
		}
	}
	l := &List{Version: f.Version, Universal: universal, byPkg: map[string]*App{}}
	for pkg, vs := range versions {
		a := buildApp(pkg, f.AppNames[pkg], vs, stableMark[pkg])
		a.Patches = perPkgPatches[pkg]
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

// appendPatch adds a patch unless one with that name is already listed.
func appendPatch(list []Patch, p Patch) []Patch {
	for _, x := range list {
		if x.Name == p.Name {
			return list
		}
	}
	return append(list, p)
}
