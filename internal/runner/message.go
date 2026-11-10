package runner

import (
	"fmt"
	"html"
	"strings"
)

// telegramText is the Telegram message for a run, in Telegram's HTML: each TV that got updates with
// one line per app (old version, new version, patches release), then one short line per problem.
// It is empty when nothing in the run was worth a message.
func telegramText(results []Result) string {
	worth := false
	var order []string
	byDevice := map[string][]string{}
	var problems []string
	seen := map[string]bool{}
	for _, x := range results {
		switch {
		case x.OK && x.Changed && x.To != "":
			worth = worth || x.Alert
			if _, ok := byDevice[x.Device]; !ok {
				order = append(order, x.Device)
			}
			byDevice[x.Device] = append(byDevice[x.Device], fmt.Sprintf("%s: %s → %s (patches %s)",
				html.EscapeString(x.Name), html.EscapeString(x.From), html.EscapeString(x.To), html.EscapeString(x.Patches)))
		case !x.OK:
			worth = worth || x.Alert
			line := x.Short
			if line == "" {
				line = brief(x.Message)
			}
			if line = html.EscapeString(line); line != "" && !seen[line] {
				seen[line] = true
				problems = append(problems, line)
			}
		}
	}
	if !worth {
		return ""
	}
	var parts []string
	for _, dev := range order {
		parts = append(parts, "<b>"+html.EscapeString(dev)+"</b>\n"+strings.Join(byDevice[dev], "\n"))
	}
	if len(problems) > 0 {
		parts = append(parts, "<b>Problems</b>\n"+strings.Join(problems, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// brief is the first sentence of s, cut to a length that fits on one line of a chat.
func brief(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:119]) + "…"
	}
	return s
}
