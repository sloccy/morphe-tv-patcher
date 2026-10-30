package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 6*time.Hour || c.WebPort != 8093 || !c.DirectOK || c.FailuresBeforeAlert != 3 ||
		c.BytecodeMode != "STRIP_SAFE" || c.DataDir != "/data" || c.VerifyEvery != 168*time.Hour || c.TVFixedADBPort != 0 || c.ScanLo != 30000 || c.ScanHi != 61000 {
		t.Errorf("%+v", c)
	}
	if !strings.HasSuffix(c.Keystore(), "keystore/morphe.keystore") || !strings.HasSuffix(c.Cache(), "work/cache") {
		t.Errorf("paths %s %s", c.Keystore(), c.Cache())
	}
}

func TestOverridesAndValidation(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"INTERVAL": "90m", "WEB_PORT": "9000",
		"APKMIRROR_DIRECT_DISABLED": "1", "FLARESOLVERR_URL": "http://127.0.0.1:8191/", "TV_FIXED_ADB_PORT": "5555", "ADB_SCAN_RANGE": "40000-41000", "VERIFY_EVERY": "48h"}))
	if err != nil || c.TVFixedADBPort != 5555 || c.ScanLo != 40000 || c.ScanHi != 41000 || c.VerifyEvery != 48*time.Hour || c.Interval != 90*time.Minute || c.WebPort != 9000 || c.DirectOK || c.FlareSolverrURL != "http://127.0.0.1:8191" {
		t.Errorf("%+v %v", c, err)
	}
	for _, bad := range []map[string]string{{"INTERVAL": "soon"}, {"INTERVAL": "10s"}, {"WEB_PORT": "0"}, {"WEB_PORT": "x"}, {"FETCH_FAILURES_BEFORE_ALERT": "0"}, {"VERIFY_EVERY": "5m"}, {"TV_FIXED_ADB_PORT": "-1"}, {"TV_FIXED_ADB_PORT": "80"}, {"TV_FIXED_ADB_PORT": "x"},
		{"ADB_SCAN_RANGE": "x"}, {"ADB_SCAN_RANGE": "41000-40000"}, {"ADB_SCAN_RANGE": "0-70000"}, {"ADB_SCAN_RANGE": "40000"}} {
		if _, err := FromEnv(env(bad)); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
}

func TestPatchesRepoIsAFirstRunSeedList(t *testing.T) {
	c, _ := FromEnv(env(nil))
	if len(c.PatchesRepos) != 1 || c.PatchesRepos[0] != "ajstrick81/morphe-androidtv-patches" {
		t.Errorf("the built-in seed is the Android TV patches: %v", c.PatchesRepos)
	}
	c, _ = FromEnv(env(map[string]string{"PATCHES_REPO": "a/b, c/d e/f\ng/h", "DATA_DIR": "/d"}))
	if len(c.PatchesRepos) != 4 || c.PatchesRepos[1] != "c/d" || c.PatchesRepos[3] != "g/h" {
		t.Errorf("%v", c.PatchesRepos)
	}
	if c.PatchBundles() != "/d/patches" {
		t.Errorf("bundles live in the backed-up state volume, not the cache: %s", c.PatchBundles())
	}
}
