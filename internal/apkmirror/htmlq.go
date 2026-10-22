package apkmirror

import (
	"strings"

	"golang.org/x/net/html"
)

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, classes ...string) bool {
	have := map[string]bool{}
	for _, c := range strings.Fields(attr(n, "class")) {
		have[c] = true
	}
	for _, c := range classes {
		if !have[c] {
			return false
		}
	}
	return true
}

// find returns every element under n (document order) accepted by match.
func find(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode && match(x) {
			out = append(out, x)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func first(n *html.Node, match func(*html.Node) bool) *html.Node {
	if r := find(n, match); len(r) > 0 {
		return r[0]
	}
	return nil
}

func text(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
			b.WriteByte(' ')
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func isA(n *html.Node) bool { return n.Data == "a" }
