package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	token := write(t, "token", "secret\n")
	cfg, err := Load(write(t, "c.toml", `
base_url = "https://literss.ethereal.io/"
prefetch_interval = "15m"

[miniflux]
url = "http://mf/"
token_file = "`+token+`"

[[rivers]]
slug = "news"
title = "News"

  [[rivers.feeds]]
  url = "https://a.example/rss"

  [[rivers.feeds]]
  url = "https://b.example/rss"
  extractor = "chrome"
  chrome = { steps = [{ wait_for = "article" }, { scroll = "bottom" }, { wait = "2s" }] }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Miniflux.Token != "secret" || cfg.Miniflux.URL != "http://mf" {
		t.Errorf("miniflux = %+v", cfg.Miniflux)
	}
	if cfg.RPID() != "literss.ethereal.io" || cfg.Origin() != "https://literss.ethereal.io" {
		t.Errorf("rpid=%q origin=%q", cfg.RPID(), cfg.Origin())
	}
	if cfg.SessionTTL.Duration != 7*24*time.Hour || cfg.PrefetchInterval.Duration != 15*time.Minute {
		t.Errorf("session ttl = %s, prefetch = %s", cfg.SessionTTL, cfg.PrefetchInterval)
	}
	feeds := cfg.Rivers[0].Feeds
	if len(feeds) != 2 || feeds[0].Extractor != ExtractorMiniflux || feeds[1].Extractor != ExtractorChrome ||
		len(feeds[1].Chrome.Steps) != 3 || feeds[1].Chrome.Steps[0].WaitFor != "article" ||
		feeds[1].Chrome.Steps[1].Scroll != "bottom" || feeds[1].Chrome.Steps[2].Wait.Duration != 2*time.Second {
		t.Errorf("feeds = %+v", feeds)
	}
}

func TestValidate(t *testing.T) {
	_, err := Load(write(t, "c.toml", `
base_url = "literss.ethereal.io"
miniflux = { url = "http://mf", token = "x" }

[[rivers]]
slug = "News"
title = "News"
feeds = [{ url = "https://a.example/rss", extractor = "lynx" }]

[[rivers]]
slug = "two"
title = "news"
feeds = [{ url = "https://a.example/rss" }]
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"base_url", "slug must match", "extractor must be", "duplicate title", "only belong to one river"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestUnknownKey(t *testing.T) {
	_, err := Load(write(t, "c.toml", `
base_url = "https://x"
miniflux = { url = "http://mf", token = "x" }

[[river]]
slug = "typo"
`))
	if err == nil || !strings.Contains(err.Error(), "unknown keys: river") {
		t.Errorf("expected unknown key error, got %v", err)
	}
}

func TestBadDuration(t *testing.T) {
	_, err := Load(write(t, "c.toml", `
base_url = "https://x"
session_ttl = "a week"
miniflux = { url = "http://mf", token = "x" }
`))
	if err == nil || !strings.Contains(err.Error(), "duration") {
		t.Errorf("expected duration error, got %v", err)
	}
}

func TestStepValidation(t *testing.T) {
	_, err := Load(write(t, "c.toml", `
base_url = "https://x"
miniflux = { url = "http://mf", token = "x" }
chrome = { extensions = ["/nonexistent"], ublock_mode = "max" }

[[rivers]]
slug = "r"
title = "R"

  [[rivers.feeds]]
  url = "https://a.example/rss"
  extractor = "chrome"
  chrome = { steps = [{ scroll = "top" }, { click = "a", remove = "b" }, {}] }
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		`chrome.steps[0]: scroll must be "bottom"`,
		"chrome.steps[1]: each step must set exactly one",
		"chrome.steps[2]: each step must set exactly one",
		"/nonexistent is not an unpacked extension",
		`chrome.ublock_mode must be "basic", "optimal" or "complete", not "max"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
