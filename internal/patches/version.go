// Package patches talks to the patches repository: finding the latest release, downloading
// the .mpp bundle, and reading which apps and versions it supports.
package patches

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type part struct {
	num   int
	text  string
	isNum bool
}

func split(v string) []part {
	var out []part
	rs := []rune(v)
	for i := 0; i < len(rs); {
		j := i
		if unicode.IsDigit(rs[i]) {
			for j < len(rs) && unicode.IsDigit(rs[j]) {
				j++
			}
			n, _ := strconv.Atoi(string(rs[i:j]))
			out = append(out, part{num: n, isNum: true})
		} else {
			for j < len(rs) && !unicode.IsDigit(rs[j]) {
				j++
			}
			out = append(out, part{text: string(rs[i:j])})
		}
		i = j
	}
	return out
}

// VersionLess orders version strings so numeric runs compare as numbers:
// 7.10.102 is newer than 7.8.100, and 26.16.0+rc2-… newer than 26.12.1+rc1-….
func VersionLess(a, b string) bool {
	pa, pb := split(a), split(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, y := pa[i], pb[i]
		switch {
		case x.isNum && y.isNum:
			if x.num != y.num {
				return x.num < y.num
			}
		case x.isNum != y.isNum:
			return !x.isNum // text sorts before numbers, as in the Python tool
		default:
			if x.text != y.text {
				return x.text < y.text
			}
		}
	}
	return len(pa) < len(pb)
}

// SortVersions returns a de-duplicated copy, oldest first.
func SortVersions(vs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vs {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return VersionLess(out[i], out[j]) })
	return out
}

// Newest returns the newest version, or "" for an empty list.
func Newest(vs []string) string {
	s := SortVersions(vs)
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// Slug turns a version into APKMirror's URL form: lowercase, "+" dropped, other runs of
// non-alphanumerics become "-" (6.23.23+v15.5.0.70-armv7a -> 6-23-23v15-5-0-70-armv7a).
func Slug(v string) string {
	v = strings.ReplaceAll(strings.ToLower(v), "+", "")
	var b strings.Builder
	dash := false
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
