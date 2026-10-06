package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCookies(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "cookies.txt")
	os.WriteFile(txt, []byte("# Netscape HTTP Cookie File\n"+
		".example.com\tTRUE\t/\tTRUE\t1999999999\tsession\tabc\n"+
		"#HttpOnly_www.example.com\tFALSE\t/app\tFALSE\t0\ttoken\txyz\n"), 0o600)
	cs, err := LoadCookies(txt)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Name != "session" || !cs[0].Secure || cs[0].Expires.Unix() != 1999999999 ||
		cs[1].Domain != "www.example.com" || !cs[1].HttpOnly || cs[1].Path != "/app" || !cs[1].Expires.IsZero() {
		t.Errorf("unexpected netscape cookies: %+v", cs)
	}

	js := filepath.Join(dir, "cookies.json")
	os.WriteFile(js, []byte(`[{"name":"a","value":"1","domain":".example.org","path":"/","secure":true,"httpOnly":true,"expirationDate":1999999999.5}]`), 0o600)
	cs, err = LoadCookies(js)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "a" || cs[0].Domain != ".example.org" || !cs[0].HttpOnly || cs[0].Expires.Unix() != 1999999999 {
		t.Errorf("unexpected json cookies: %+v", cs)
	}
}

func TestFeedCookies(t *testing.T) {
	cookies := write(t, "cookies.txt", "# Netscape HTTP Cookie File\n"+
		".example.com\tTRUE\t/\tTRUE\t1999999999\tsession\tabc\n"+
		".example.com\tTRUE\t/\tTRUE\t1000000000\texpired\told\n"+
		"www.example.com\tFALSE\t/\tFALSE\t0\ttoken\txyz\n")
	cfg, err := Load(write(t, "c.toml", `
base_url = "https://x"
miniflux = { url = "http://mf", token = "x" }

[[rivers]]
slug = "private"
title = "Private"
feeds = [{ url = "https://example.com/rss", extractor = "chrome", cookies_file = "`+cookies+`" }]
`))
	if err != nil {
		t.Fatal(err)
	}
	f := &cfg.Rivers[0].Feeds[0]
	if len(f.Cookies) != 3 {
		t.Fatalf("cookies = %+v", f.Cookies)
	}
	if got := f.CookieHeader(); got != "session=abc; token=xyz" {
		t.Errorf("CookieHeader() = %q", got)
	}

	_, err = Load(write(t, "c.toml", `
base_url = "https://x"
miniflux = { url = "http://mf", token = "x" }

[[rivers]]
slug = "private"
title = "Private"
feeds = [{ url = "https://example.com/rss", cookies_file = "/nonexistent" }]
`))
	if err == nil || !strings.Contains(err.Error(), "cookies_file") {
		t.Errorf("expected cookies_file error, got %v", err)
	}
}
