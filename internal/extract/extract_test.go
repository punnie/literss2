package extract

import (
	"net/url"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	base, _ := url.Parse("https://example.com/news/story.html")
	var got []string
	rewrite := func(abs string) string {
		got = append(got, abs)
		return "/img/x"
	}
	in := `<p>Hello <a href="/other">link</a> <a href="#fn">fn</a></p>
<script>alert(1)</script>
<p onclick="evil()" style="color:red">styled</p>
<picture><source srcset="big.webp"><img src="data:image/gif;base64,AA" data-src="lazy.jpg" srcset="a.jpg 100w"></picture>
<img srcset="s.jpg 320w, m.jpg 800w, l.jpg 2000w">
<img src="pixel.gif" width="1" height="1">
<img src="avatar.jpg" width="36" height="36">
<ul><li><a href="/share"><svg></svg></a></li><li> </li></ul>
<iframe src="https://evil.example"></iframe>`
	out := Clean(in, base, rewrite)

	for _, want := range []string{
		`href="https://example.com/other"`,
		`href="#fn"`,
		`<img src="/img/x" loading="lazy"/>`,
		`styled`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"<script", "alert", "onclick", "style=", "iframe", "srcset", "data-src", "<ul", "<li", "pixel.gif", "avatar"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
	wantImgs := []string{"https://example.com/news/lazy.jpg", "https://example.com/news/m.jpg"}
	if strings.Join(got, ",") != strings.Join(wantImgs, ",") {
		t.Errorf("rewritten images = %v, want %v", got, wantImgs)
	}
}
