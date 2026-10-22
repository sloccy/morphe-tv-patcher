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
	if c.Interval != 6*time.Hour || c.WebPort != 8093 || c.APKPureOK || !c.DirectOK || c.FailuresBeforeAlert != 3 ||
		c.BytecodeMode != "STRIP_SAFE" || c.DataDir != "/data" {
		t.Errorf("%+v", c)
	}
	if !strings.HasSuffix(c.Keystore(), "keystore/morphe.keystore") || !strings.HasSuffix(c.Cache(), "work/cache") {
		t.Errorf("paths %s %s", c.Keystore(), c.Cache())
	}
}

func TestOverridesAndValidation(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"INTERVAL": "90m", "WEB_PORT": "9000", "APKPURE_ACKNOWLEDGE_DANGERS": "true",
		"APKMIRROR_DIRECT_DISABLED": "1", "FLARESOLVERR_URL": "http://127.0.0.1:8191/"}))
	if err != nil || c.Interval != 90*time.Minute || c.WebPort != 9000 || !c.APKPureOK || c.DirectOK || c.FlareSolverrURL != "http://127.0.0.1:8191" {
		t.Errorf("%+v %v", c, err)
	}
	for _, bad := range []map[string]string{{"INTERVAL": "soon"}, {"INTERVAL": "10s"}, {"WEB_PORT": "0"}, {"WEB_PORT": "x"}, {"FETCH_FAILURES_BEFORE_ALERT": "0"}} {
		if _, err := FromEnv(env(bad)); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
}
