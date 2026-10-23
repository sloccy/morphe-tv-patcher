// Package abi decides which CPU builds of an app suit a device. A device lists the ABIs it can
// run in order of preference (arm64-v8a,armeabi-v7a,armeabi); a lower rank is a better match.
package abi

import (
	"regexp"
	"slices"
	"strings"
)

// Default is used when no device is known (the original 32-bit target).
var Default = []string{"armeabi-v7a", "armeabi"}

// Normalize returns the device list, or Default when it is empty.
func Normalize(device []string) []string {
	if len(device) == 0 {
		return slices.Clone(Default)
	}
	return device
}

// Parse splits a comma separated list such as the output of ro.product.cpu.abilist.
func Parse(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Is64 reports whether the device runs 64-bit native code.
func Is64(device []string) bool {
	return slices.Contains(device, "arm64-v8a") || slices.Contains(device, "x86_64")
}

// Rank is the index of the first device ABI the bundle has native code for. A bundle with no
// native code at all ranks 0. ok is false when it has native code but none the device can run.
func Rank(have map[string]bool, device []string) (rank int, ok bool) {
	if len(have) == 0 {
		return 0, true
	}
	for i, d := range Normalize(device) {
		if have[d] {
			return i, true
		}
	}
	return 0, false
}

// BestRank is the best rank any of the offered builds could reach on the device (builds are ABI
// names, "*" a universal build). ok is false when none of them runs on it or nothing is offered.
func BestRank(builds, device []string) (rank int, ok bool) {
	if len(builds) == 0 {
		return 0, false
	}
	have := map[string]bool{}
	for _, b := range builds {
		if b == "*" {
			return 0, true
		}
		have[b] = true
	}
	return Rank(have, device)
}

var token = regexp.MustCompile(`arm64-v8a|armeabi-v7a|armeabi|x86_64|x86|mips64|mips`)

// Row describes the architectures APKMirror lists for one variant row of text, such as
// "arm64-v8a + armeabi-v7a", "armeabi-v7a", "universal" or "noarch".
type Row struct {
	ABIs      []string
	Universal bool
}

// ParseRow reads the architecture tokens out of a variant row's text.
func ParseRow(text string) Row {
	t := strings.ToLower(text)
	r := Row{Universal: strings.Contains(t, "universal") || strings.Contains(t, "noarch")}
	for _, m := range token.FindAllString(t, -1) {
		if !slices.Contains(r.ABIs, m) {
			r.ABIs = append(r.ABIs, m)
		}
	}
	return r
}

// Score ranks a row for a device: smaller is better. Explicit matches score 2*rank, a universal
// build sits between rank 0 and rank 1 (1), and ok is false when nothing in the row runs on the
// device. Among equal scores, rows offering more ABIs are preferred by the caller.
func (r Row) Score(device []string) (score int, ok bool) {
	best, found := 0, false
	for i, d := range Normalize(device) {
		if slices.Contains(r.ABIs, d) {
			best, found = 2*i, true
			break
		}
	}
	if r.Universal {
		if !found || best > 1 {
			return 1, true
		}
	}
	return best, found
}

// Key is a stable identifier for a device's ABI list (devices with the same key share a fetch
// and a patched file).
func Key(device []string) string { return strings.Join(Normalize(device), ",") }
