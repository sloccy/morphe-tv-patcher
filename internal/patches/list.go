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
	Versions     []string `json:"versions"`     // oldest first
	Target       string   `json:"target"`       // the version to build by default (see Pick)
	Experimental bool     `json:"experimental"` // the target version is marked experimental

	// Patches says which versions each patch supports, so Pick can weigh them.
	Patches []PatchCompat `json:"-"`
	stable  map[string]bool
}

// PatchCompat is one patch's view of an app: whether it is on by default and which versions it names.
// A patch naming no versions is universal and does not influence the choice.
type PatchCompat struct {
	Name     string
	Enabled  bool
	Versions []string
}

// Pick chooses the version to build: the stable version that the most applied patches support, the
// newest of those on a tie. Applied means on by default, minus disable, plus enable (patch names, any
// case). When no version is stable, all of them are considered and the result is flagged experimental.
// Without patch data every score is zero, so it is simply the newest candidate.
func (a App) Pick(enable, disable []string) (version string, experimental bool) {
	var cands []string
	for _, v := range a.Versions {
		if a.stable[v] {
			cands = append(cands, v)
		}
	}
	if len(cands) == 0 {
		cands, experimental = a.Versions, len(a.Versions) > 0
	}
	has := func(list []string, name string) bool {
		for _, x := range list {
			if strings.EqualFold(x, name) {
				return true
			}
		}
		return false
	}
	score := map[string]int{}
	for _, p := range a.Patches {
		on := p.Enabled
		if has(disable, p.Name) {
			on = false
		}
		if has(enable, p.Name) {
			on = true
		}
		if !on {
			continue
		}
		for _, v := range p.Versions {
			score[v]++
		}
	}
	best := -1
	for _, v := range cands { // oldest first, so >= leaves the newest of equals
		if score[v] >= best {
			version, best = v, score[v]
		}
	}
	return version, experimental
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
		Name               string              `json:"name"`
		Use                bool                `json:"use"`
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
	details := map[string][]PatchCompat{}
	mark := func(pkg, v string) {
		if stableMark[pkg] == nil {
			stableMark[pkg] = map[string]bool{}
		}
		stableMark[pkg][v] = true
	}
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
			details[pkg] = append(details[pkg], PatchCompat{Name: p.Name, Enabled: p.Use, Versions: vs})
		}
	}
	l := &List{Version: f.Version, byPkg: map[string]*App{}}
	for pkg, vs := range versions {
		a := buildApp(pkg, f.AppNames[pkg], vs, stableMark[pkg], details[pkg])
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
