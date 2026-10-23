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

// ParseListPatchDetails reads the same output per patch: its name, whether it is on by default and
// the versions it names for each package. Patches with no package listing do not appear.
func ParseListPatchDetails(out []byte) map[string][]PatchCompat {
	res := map[string][]PatchCompat{}
	name, enabled, cur := "", false, ""
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "Name: "):
			name, enabled, cur = strings.TrimSpace(strings.TrimPrefix(line, "Name: ")), false, ""
		case strings.HasPrefix(line, "Enabled: "):
			enabled = strings.TrimSpace(strings.TrimPrefix(line, "Enabled: ")) == "true"
		case strings.HasPrefix(line, "\tPackage name: "):
			cur = strings.TrimSpace(strings.TrimPrefix(line, "\tPackage name: "))
			res[cur] = append(res[cur], PatchCompat{Name: name, Enabled: enabled})
		case strings.HasPrefix(line, "\t\t") && cur != "":
			if v := strings.TrimSpace(line); v != "" {
				last := &res[cur][len(res[cur])-1]
				last.Versions = append(last.Versions, v)
			}
		case !strings.HasPrefix(line, "\t") && line != "":
			cur = ""
		}
	}
	return res
}

// FromCLI builds the app list of one release from the CLI's views of its bundle: stable holds
// the versions without -x, all the versions with -x (stable plus experimental). names comes from
// the repository's patches-list.json when it has one. The target is chosen by App.Pick; only when a
// package has no stable version are experimental ones considered (and flagged).
func FromCLI(tag string, stable, all map[string][]string, names map[string]string) *List {
	return FromCLIWithPatches(tag, stable, all, nil, names)
}

// FromCLIWithPatches is FromCLI plus each patch's own version list (ParseListPatchDetails of the
// -x output), which lets Pick prefer the version the most patches support.
func FromCLIWithPatches(tag string, stable, all map[string][]string, details map[string][]PatchCompat, names map[string]string) *List {
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
		l.Apps = append(l.Apps, buildApp(pkg, names[pkg], vs, stableSet, details[pkg]))
	}
	sort.Slice(l.Apps, func(i, j int) bool { return l.Apps[i].Name < l.Apps[j].Name })
	for i := range l.Apps {
		l.byPkg[l.Apps[i].Package] = &l.Apps[i]
	}
	return l
}

func buildApp(pkg, name string, versions []string, stable map[string]bool, ps []PatchCompat) App {
	a := App{Package: pkg, Name: name, Versions: SortVersions(versions), Patches: ps, stable: stable}
	if a.Name == "" {
		a.Name = PrettyName(pkg)
	}
	a.Target, a.Experimental = a.Pick(nil, nil)
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
