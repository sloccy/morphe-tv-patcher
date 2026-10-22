package patches

import (
	"errors"
	"sort"
	"strings"
)

// ErrNoList: the repository does not publish a patches-list.json (only some do).
var ErrNoList = errors.New("repository has no patches-list.json")

// ParseListPatches reads the output of `list-patches -p -v`: for every package, the versions
// any patch declares compatible. A package whose patches name no versions appears with none.
func ParseListPatches(out []byte) map[string][]string {
	res := map[string][]string{}
	seen := map[string]map[string]bool{}
	cur := ""
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "\tPackage name: "):
			cur = strings.TrimSpace(strings.TrimPrefix(line, "\tPackage name: "))
			if _, ok := res[cur]; !ok {
				res[cur] = nil
				seen[cur] = map[string]bool{}
			}
		case strings.HasPrefix(line, "\t\t") && cur != "":
			v := strings.TrimSpace(line)
			if v != "" && !seen[cur][v] {
				seen[cur][v] = true
				res[cur] = append(res[cur], v)
			}
		case !strings.HasPrefix(line, "\t") && line != "":
			cur = "" // a new patch ("Index:", "Name:", ...) ends the package block
		}
	}
	return res
}

// FromCLI builds the app list of one release from the CLI's views of its bundle: stable holds
// the versions without -x, all the versions with -x (stable plus experimental). names comes from
// the repository's patches-list.json when it has one. The newest stable version is the target;
// only when a package has no stable version is the newest experimental one used (and flagged).
func FromCLI(tag string, stable, all map[string][]string, names map[string]string) *List {
	l := &List{Version: tag, byPkg: map[string]*App{}}
	for pkg, vs := range all {
		stableSet := map[string]bool{}
		for _, v := range stable[pkg] {
			stableSet[v] = true
		}
		for _, v := range vs {
			if len(stable) == 0 { // a caller with no separate stable view: treat everything as stable
				stableSet[v] = true
			}
		}
		l.Apps = append(l.Apps, buildApp(pkg, names[pkg], vs, stableSet))
	}
	sort.Slice(l.Apps, func(i, j int) bool { return l.Apps[i].Name < l.Apps[j].Name })
	for i := range l.Apps {
		l.byPkg[l.Apps[i].Package] = &l.Apps[i]
	}
	return l
}

func buildApp(pkg, name string, versions []string, stable map[string]bool) App {
	sorted := SortVersions(versions)
	var stableVersions []string
	for _, v := range sorted {
		if stable[v] {
			stableVersions = append(stableVersions, v)
		}
	}
	a := App{Package: pkg, Name: name, Versions: sorted}
	if a.Name == "" {
		a.Name = PrettyName(pkg)
	}
	if len(stableVersions) > 0 {
		a.Newest = Newest(stableVersions)
	} else {
		a.Newest = Newest(sorted)
		a.Experimental = a.Newest != ""
	}
	return a
}

// Names returns package -> display name.
func (l *List) Names() map[string]string {
	m := make(map[string]string, len(l.Apps))
	for _, a := range l.Apps {
		m[a.Package] = a.Name
	}
	return m
}

var genericParts = map[string]bool{"com": true, "org": true, "net": true, "io": true, "tv": true, "android": true,
	"app": true, "apps": true, "google": true, "mobile": true, "frontpage": false}

// PrettyName guesses a display name from a package when nothing better is known:
// com.google.android.apps.youtube.music -> "Youtube Music".
func PrettyName(pkg string) string {
	var words []string
	for p := range strings.SplitSeq(pkg, ".") {
		if p != "" && !genericParts[p] {
			words = append(words, strings.ToUpper(p[:1])+p[1:])
		}
	}
	if len(words) == 0 {
		return pkg
	}
	return strings.Join(words, " ")
}
