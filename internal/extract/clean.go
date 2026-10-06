package extract

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowRelativeURLs(true)
	p.RequireNoReferrerOnLinks(true)
	p.AllowAttrs("loading").Matching(bluemonday.SpaceSeparatedTokens).OnElements("img")
	return p
}()

// Clean rewrites article HTML for local display: relative links are made
// absolute, lazy-loaded images are resolved, every image is routed through
// rewriteImage (the local cache), and the result is sanitized.
func Clean(content string, base *url.URL, rewriteImage func(abs string) string) string {
	nodes, err := html.ParseFragment(strings.NewReader(content), &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div})
	if err != nil {
		return policy.Sanitize(content)
	}
	root := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	walk(root, base, rewriteImage)
	var buf bytes.Buffer
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		html.Render(&buf, c)
	}
	return policy.Sanitize(buf.String())
}

func walk(n *html.Node, base *url.URL, rewriteImage func(string) string) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode {
			switch c.DataAtom {
			case atom.Source, atom.Script, atom.Style, atom.Noscript, atom.Iframe, atom.Svg, atom.Form:
				n.RemoveChild(c)
				c = next
				continue
			case atom.Img:
				if !fixImage(c, base, rewriteImage) {
					n.RemoveChild(c)
					c = next
					continue
				}
			case atom.A:
				if href := attr(c, "href"); href != "" && !strings.HasPrefix(href, "#") {
					setAttr(c, "href", resolve(base, href))
				}
			}
			walk(c, base, rewriteImage)
			if prunable[c.DataAtom] && isEmpty(c) {
				n.RemoveChild(c)
			}
		}
		c = next
	}
}

// prunable elements are removed when they end up with no text or media,
// e.g. share-button lists left behind by readability.
var prunable = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Span: true, atom.Li: true, atom.Ul: true, atom.Ol: true,
	atom.A: true, atom.Figure: true, atom.Section: true, atom.Picture: true, atom.Button: true,
}

func isEmpty(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			if strings.TrimSpace(c.Data) != "" {
				return false
			}
		case html.ElementNode:
			switch c.DataAtom {
			case atom.Img, atom.Video, atom.Audio, atom.Hr, atom.Table:
				return false
			}
			if !isEmpty(c) {
				return false
			}
		}
	}
	return true
}

// tiny reports whether declared dimensions mark an image as a tracking
// pixel, icon or avatar.
func tiny(n *html.Node) bool {
	w, werr := strconv.Atoi(attr(n, "width"))
	h, herr := strconv.Atoi(attr(n, "height"))
	switch {
	case werr == nil && herr == nil:
		return w <= 64 && h <= 64
	case werr == nil:
		return w <= 2
	case herr == nil:
		return h <= 2
	}
	return false
}

// fixImage resolves an image's real source and rewrites it. It returns false
// if the image should be dropped.
func fixImage(n *html.Node, base *url.URL, rewriteImage func(string) string) bool {
	src := ""
	for _, k := range []string{"data-src", "data-original", "data-lazy-src", "data-url"} {
		if v := attr(n, k); v != "" && !strings.HasPrefix(v, "data:") {
			src = v
			break
		}
	}
	if src == "" {
		if v := attr(n, "src"); !strings.HasPrefix(v, "data:") {
			src = v
		}
	}
	if s := pickSrcset(firstNonEmpty(attr(n, "data-srcset"), attr(n, "srcset"))); s != "" && src == "" {
		src = s
	}
	if src == "" || tiny(n) {
		return false
	}
	abs := resolve(base, src)
	u, err := url.Parse(abs)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	keep := n.Attr[:0]
	for _, a := range n.Attr {
		switch a.Key {
		case "alt", "title", "width", "height":
			keep = append(keep, a)
		}
	}
	n.Attr = keep
	setAttr(n, "src", rewriteImage(abs))
	setAttr(n, "loading", "lazy")
	return true
}

// pickSrcset chooses the widest candidate up to 1600w, or the first one.
func pickSrcset(srcset string) string {
	best, bestW := "", -1
	for _, cand := range strings.Split(srcset, ",") {
		f := strings.Fields(strings.TrimSpace(cand))
		if len(f) == 0 {
			continue
		}
		w := 0
		if len(f) > 1 && strings.HasSuffix(f[1], "w") {
			w, _ = strconv.Atoi(strings.TrimSuffix(f[1], "w"))
		}
		if best == "" || (w > bestW && w <= 1600) {
			best, bestW = f[0], w
		}
	}
	return best
}

func resolve(base *url.URL, ref string) string {
	if base == nil {
		return ref
	}
	u, err := base.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return u.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
