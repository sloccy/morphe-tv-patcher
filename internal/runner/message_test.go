package runner

import (
	"strings"
	"testing"
)

func TestTelegramTextGroupsUpdatesByTVAndListsProblemsBriefly(t *testing.T) {
	res := []Result{
		{Device: "Living <Room>", Name: "YouTube", OK: true, Changed: true, Alert: true, From: "4.10.1", To: "4.12.0", Patches: "v1.5.0"},
		{Device: "Living <Room>", Name: "Netflix", OK: true, Changed: true, Alert: true, From: "none", To: "9.1.0", Patches: "v1.5.0"},
		{Device: "Bedroom", Name: "YouTube", OK: true, Changed: true, Alert: true, From: "4.10.1", To: "4.12.0", Patches: "v1.5.0"},
		{Device: "Bedroom", OK: false, Alert: true, Short: "Bedroom: not reachable", Message: "long text. more"},
		{Device: "Bedroom", OK: false, Message: "Hulu: patching failed. details"},
		{Device: "Bedroom", OK: false, Alert: true, Short: "Bedroom: not reachable"},
	}
	want := "<b>Living &lt;Room&gt;</b>\nYouTube: 4.10.1 → 4.12.0 (patches v1.5.0)\nNetflix: none → 9.1.0 (patches v1.5.0)\n\n" +
		"<b>Bedroom</b>\nYouTube: 4.10.1 → 4.12.0 (patches v1.5.0)\n\n" +
		"<b>Problems</b>\nBedroom: not reachable\nHulu: patching failed"
	if got := telegramText(res); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTelegramTextIsEmptyWithoutAnAlert(t *testing.T) {
	res := []Result{
		{OK: true, Message: "up to date"},
		{OK: false, Message: "still under the alert threshold"},
		{OK: true, Changed: true, From: "1", To: "2", Patches: "v1"}, // e.g. a dry run: not an alert
	}
	if got := telegramText(res); got != "" {
		t.Errorf("nothing to say: %q", got)
	}
}

func TestBriefKeepsTheFirstSentenceOnOneLine(t *testing.T) {
	if got := brief("first part. second part"); got != "first part" {
		t.Errorf("%q", got)
	}
	if got := brief(strings.Repeat("x", 300)); len([]rune(got)) != 120 {
		t.Errorf("%d", len([]rune(got)))
	}
}
