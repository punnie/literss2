// Package config loads the literss TOML configuration file.
//
// Everything a user could otherwise customise (rivers, feeds, extraction
// method, cookies) lives here; the web UI exposes none of it.
package config

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	ExtractorMiniflux = "miniflux"
	ExtractorChrome   = "chrome"
)

// Duration is a time.Duration that unmarshals from strings like "15m".
// An empty string means zero.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("duration must be a string like \"15m\": %w", err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

type Config struct {
	// Address to listen on, e.g. "127.0.0.1:8090".
	Listen string `toml:"listen"`
	// Public URL of the application, e.g. "https://literss.ethereal.io".
	// Its host is used as the WebAuthn relying party ID.
	BaseURL string `toml:"base_url"`
	// Directory holding the SQLite database and the image cache.
	DataDir string `toml:"data_dir"`
	// IANA time zone used to render entry times.
	TimeZone string `toml:"time_zone"`
	// Number of titles per river page.
	ItemsPerPage int `toml:"items_per_page"`
	// Sliding session lifetime.
	SessionTTL Duration `toml:"session_ttl"`
	// How often recent entries are extracted ahead of time. Zero disables it.
	PrefetchInterval Duration `toml:"prefetch_interval"`
	// How often feed configuration is reconciled with Miniflux.
	SyncInterval Duration `toml:"sync_interval"`

	Miniflux Miniflux `toml:"miniflux"`
	Chrome   Chrome   `toml:"chrome"`
	Rivers   []River  `toml:"rivers"`

	Location *time.Location `toml:"-"`
}

type Miniflux struct {
	URL       string `toml:"url"`
	Token     string `toml:"token"`
	TokenFile string `toml:"token_file"`
}

type Chrome struct {
	// Path to the chromium binary. Empty means look it up in $PATH.
	Path string `toml:"path"`
	// Disable the chromium sandbox (needed in some containers).
	NoSandbox bool `toml:"no_sandbox"`
	// Per-page timeout.
	Timeout Duration `toml:"timeout"`
	// Maximum number of pages rendered at the same time.
	MaxConcurrent int `toml:"max_concurrent"`
	// Default user agent for rendered pages.
	UserAgent string `toml:"user_agent"`
}

type River struct {
	Slug  string `toml:"slug"`
	Title string `toml:"title"`
	Feeds []Feed `toml:"feeds"`
}

type Feed struct {
	// URL of the RSS/Atom feed itself.
	URL string `toml:"url"`
	// Optional title override; otherwise Miniflux's title is used.
	Title string `toml:"title"`
	// "miniflux" (fetch-content endpoint) or "chrome" (headless + readability).
	Extractor string `toml:"extractor"`

	// Netscape cookies.txt or a JSON array of cookies (browser export
	// format). They are sent by Miniflux when polling the feed and fetching
	// articles, injected into chromium, and used when caching images.
	CookiesFile string         `toml:"cookies_file"`
	Cookies     []*http.Cookie `toml:"-"`

	// Settings pushed to the Miniflux feed. They affect how Miniflux polls the
	// feed and, for the miniflux extractor, how it fetches the article.
	UserAgent      string `toml:"user_agent"`
	Username       string `toml:"username"`
	Password       string `toml:"password"`
	PasswordFile   string `toml:"password_file"`
	Crawler        bool   `toml:"crawler"`
	ScraperRules   string `toml:"scraper_rules"`
	RewriteRules   string `toml:"rewrite_rules"`
	BlocklistRules string `toml:"blocklist_rules"`
	KeeplistRules  string `toml:"keeplist_rules"`

	Chrome ChromeFeed `toml:"chrome"`
}

type ChromeFeed struct {
	// CSS selector to wait for before extracting.
	WaitSelector string `toml:"wait_selector"`
	// Extra fixed delay after load, for pages that render late.
	Wait Duration `toml:"wait"`
	// Overrides chrome.user_agent for this feed.
	UserAgent string `toml:"user_agent"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads, defaults, validates and resolves secret files.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	md, err := toml.Decode(string(raw), &c)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	// Reject unknown keys so typos don't silently fall back to defaults.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("parsing %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	c.setDefaults()
	if err := c.resolveSecrets(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) setDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8090"
	}
	if c.DataDir == "" {
		c.DataDir = "."
	}
	if c.TimeZone == "" {
		c.TimeZone = "UTC"
	}
	if c.ItemsPerPage <= 0 {
		c.ItemsPerPage = 100
	}
	if c.SessionTTL.Duration == 0 {
		c.SessionTTL.Duration = 7 * 24 * time.Hour
	}
	if c.SyncInterval.Duration == 0 {
		c.SyncInterval.Duration = time.Hour
	}
	if c.Chrome.Timeout.Duration == 0 {
		c.Chrome.Timeout.Duration = 45 * time.Second
	}
	if c.Chrome.MaxConcurrent <= 0 {
		c.Chrome.MaxConcurrent = 2
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	c.Miniflux.URL = strings.TrimRight(c.Miniflux.URL, "/")
	for i := range c.Rivers {
		for j := range c.Rivers[i].Feeds {
			f := &c.Rivers[i].Feeds[j]
			if f.Extractor == "" {
				f.Extractor = ExtractorMiniflux
			}
		}
	}
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *Config) resolveSecrets() error {
	if c.Miniflux.TokenFile != "" {
		t, err := readSecret(c.Miniflux.TokenFile)
		if err != nil {
			return fmt.Errorf("miniflux.token_file: %w", err)
		}
		c.Miniflux.Token = t
	}
	for i := range c.Rivers {
		for j := range c.Rivers[i].Feeds {
			f := &c.Rivers[i].Feeds[j]
			if f.CookiesFile != "" {
				cs, err := LoadCookies(f.CookiesFile)
				if err != nil {
					return fmt.Errorf("feed %s: cookies_file: %w", f.URL, err)
				}
				f.Cookies = cs
			}
			if f.PasswordFile != "" {
				v, err := readSecret(f.PasswordFile)
				if err != nil {
					return fmt.Errorf("feed %s: password_file: %w", f.URL, err)
				}
				f.Password = v
			}
		}
	}
	return nil
}

func (c *Config) validate() error {
	var errs []error
	u, err := url.Parse(c.BaseURL)
	if c.BaseURL == "" || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		errs = append(errs, errors.New("base_url must be an absolute http(s) URL"))
	}
	if c.Miniflux.URL == "" {
		errs = append(errs, errors.New("miniflux.url is required"))
	}
	if c.Miniflux.Token == "" {
		errs = append(errs, errors.New("miniflux.token or miniflux.token_file is required"))
	}
	loc, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		errs = append(errs, fmt.Errorf("time_zone: %w", err))
	}
	c.Location = loc
	if len(c.Rivers) == 0 {
		errs = append(errs, errors.New("at least one river is required"))
	}
	slugs := map[string]bool{}
	titles := map[string]bool{}
	feeds := map[string]string{}
	for _, r := range c.Rivers {
		if !slugRe.MatchString(r.Slug) {
			errs = append(errs, fmt.Errorf("river %q: slug must match %s", r.Slug, slugRe))
		}
		if r.Title == "" {
			errs = append(errs, fmt.Errorf("river %q: title is required", r.Slug))
		}
		if slugs[r.Slug] {
			errs = append(errs, fmt.Errorf("river %q: duplicate slug", r.Slug))
		}
		// Rivers map onto Miniflux categories, whose titles are unique.
		if titles[strings.ToLower(r.Title)] {
			errs = append(errs, fmt.Errorf("river %q: duplicate title", r.Slug))
		}
		slugs[r.Slug], titles[strings.ToLower(r.Title)] = true, true
		for _, f := range r.Feeds {
			fu, err := url.Parse(f.URL)
			if err != nil || fu.Host == "" {
				errs = append(errs, fmt.Errorf("river %q: invalid feed url %q", r.Slug, f.URL))
			}
			// A Miniflux feed belongs to exactly one category.
			if other, ok := feeds[f.URL]; ok {
				errs = append(errs, fmt.Errorf("feed %s appears in rivers %q and %q; a feed can only belong to one river", f.URL, other, r.Slug))
			}
			feeds[f.URL] = r.Slug
			switch f.Extractor {
			case ExtractorMiniflux, ExtractorChrome:
			default:
				errs = append(errs, fmt.Errorf("feed %s: extractor must be %q or %q", f.URL, ExtractorMiniflux, ExtractorChrome))
			}
		}
	}
	return errors.Join(errs...)
}

// RPID is the WebAuthn relying party ID (the host of base_url).
func (c *Config) RPID() string {
	u, _ := url.Parse(c.BaseURL)
	return u.Hostname()
}

// Origin is base_url's scheme and host.
func (c *Config) Origin() string {
	u, _ := url.Parse(c.BaseURL)
	return u.Scheme + "://" + u.Host
}

func (c *Config) River(slug string) *River {
	for i := range c.Rivers {
		if c.Rivers[i].Slug == slug {
			return &c.Rivers[i]
		}
	}
	return nil
}
