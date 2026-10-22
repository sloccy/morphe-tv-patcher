package abi

import "testing"

var (
	armv7  = []string{"armeabi-v7a", "armeabi"}
	arm64  = []string{"arm64-v8a", "armeabi-v7a", "armeabi"}
	only64 = []string{"arm64-v8a"}
)

func TestRank(t *testing.T) {
	cases := []struct {
		name   string
		have   map[string]bool
		device []string
		rank   int
		ok     bool
	}{
		{"32-bit device, 32-bit build", map[string]bool{"armeabi-v7a": true}, armv7, 0, true},
		{"64-bit device prefers its own", map[string]bool{"arm64-v8a": true, "armeabi-v7a": true}, arm64, 0, true},
		{"64-bit device with only a 32-bit build", map[string]bool{"armeabi-v7a": true}, arm64, 1, true},
		{"arm64-only device with a 32-bit build", map[string]bool{"armeabi-v7a": true}, only64, 0, false},
		{"32-bit device with a 64-bit build", map[string]bool{"arm64-v8a": true}, armv7, 0, false},
		{"no native code runs anywhere", nil, only64, 0, true},
		{"unknown device uses the default", map[string]bool{"armeabi-v7a": true}, nil, 0, true},
	}
	for _, c := range cases {
		r, ok := Rank(c.have, c.device)
		if r != c.rank || ok != c.ok {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", c.name, r, ok, c.rank, c.ok)
		}
	}
}

func TestParseRowRealLabels(t *testing.T) {
	cases := map[string]Row{
		"7.8.100 BUNDLE 26 S 230708100 August 12, 2026 armeabi-v7a Android 12L+ 320dpi":                   {ABIs: []string{"armeabi-v7a"}},
		"26.16.0 BUNDLE 26 S 1 September 11, 2026 arm64-v8a + armeabi-v7a Android 6.0+ 320dpi":            {ABIs: []string{"arm64-v8a", "armeabi-v7a"}},
		"7.9.0.61 BUNDLE 26 S 1 August 10, 2026 arm64-v8a Android 10+ 320dpi":                             {ABIs: []string{"arm64-v8a"}},
		"(arm) (nodpi) (Android 5.0+) Latest: 4.0.0 armeabi Android 5.0+ nodpi":                           {ABIs: []string{"armeabi"}},
		"(arm64-v8a + x86) (320dpi) (Android 5.0+) Latest: 7.47.0.35 arm64-v8a + x86 Android 5.0+ 320dpi": {ABIs: []string{"arm64-v8a", "x86"}},
		"(noarch) (320dpi) (Android 5.0+) Latest: 7.40.0.28 noarch Android 5.0+ 320dpi":                   {Universal: true},
		"(universal) (nodpi) Latest: 7.71.1.2 universal Android 5.0+ nodpi":                               {Universal: true},
		"x86_64 build": {ABIs: []string{"x86_64"}},
	}
	for text, want := range cases {
		got := ParseRow(text)
		if got.Universal != want.Universal || len(got.ABIs) != len(want.ABIs) {
			t.Errorf("%q: got %+v, want %+v", text, got, want)
			continue
		}
		for i := range want.ABIs {
			if got.ABIs[i] != want.ABIs[i] {
				t.Errorf("%q: got %+v, want %+v", text, got, want)
			}
		}
	}
}

func TestScore(t *testing.T) {
	row := ParseRow
	cases := []struct {
		name   string
		row    Row
		device []string
		score  int
		ok     bool
	}{
		{"32-bit row on 32-bit device", row("armeabi-v7a"), armv7, 0, true},
		{"combined row on 32-bit device", row("arm64-v8a + armeabi-v7a"), armv7, 0, true},
		{"64-bit-only row on 32-bit device", row("arm64-v8a"), armv7, 0, false},
		{"64-bit row on 64-bit device", row("arm64-v8a"), arm64, 0, true},
		{"32-bit row on 64-bit device is second best", row("armeabi-v7a"), arm64, 2, true},
		{"universal sits between", row("universal"), arm64, 1, true},
		{"universal on a device nothing else fits", row("universal"), only64, 1, true},
		{"x86 row on arm device", row("x86"), arm64, 0, false},
		{"legacy armeabi row on 32-bit device", row("armeabi"), armv7, 2, true},
	}
	for _, c := range cases {
		s, ok := c.row.Score(c.device)
		if s != c.score || ok != c.ok {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", c.name, s, ok, c.score, c.ok)
		}
	}
}

func TestHelpers(t *testing.T) {
	if !Is64(arm64) || Is64(armv7) || !Is64([]string{"x86_64", "x86"}) {
		t.Error("Is64 wrong")
	}
	if Key(nil) != "armeabi-v7a,armeabi" || Key(arm64) != "arm64-v8a,armeabi-v7a,armeabi" {
		t.Error("Key wrong")
	}
	if got := Parse(" arm64-v8a, armeabi-v7a ,,armeabi"); len(got) != 3 || got[1] != "armeabi-v7a" {
		t.Errorf("Parse = %v", got)
	}
	got := Normalize(nil)
	got[0] = "mutated"
	if Default[0] != "armeabi-v7a" {
		t.Error("Normalize must not hand out the shared default slice")
	}
}
