package extract

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"literss/internal/config"
)

// Chrome renders pages in a shared headless chromium, one tab per page.
type Chrome struct {
	cfg config.Chrome
	sem chan struct{}

	mu      sync.Mutex
	browser context.Context
	cancel  context.CancelFunc
}

func NewChrome(cfg config.Chrome) *Chrome {
	return &Chrome{cfg: cfg, sem: make(chan struct{}, cfg.MaxConcurrent)}
}

// browserCtx starts chromium on first use, and again if it has died.
func (c *Chrome) browserCtx() (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.browser != nil && c.browser.Err() == nil {
		return c.browser, nil
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.Flag("blink-settings", "imagesEnabled=false"),
		chromedp.Flag("mute-audio", true),
	)
	if c.cfg.Path != "" {
		opts = append(opts, chromedp.ExecPath(c.cfg.Path))
	}
	if c.cfg.NoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	if c.cfg.UserAgent != "" {
		opts = append(opts, chromedp.UserAgent(c.cfg.UserAgent))
	}
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	browser, cancelBrowser := chromedp.NewContext(alloc)
	if err := chromedp.Run(browser); err != nil {
		cancelBrowser()
		cancelAlloc()
		return nil, fmt.Errorf("starting chromium: %w", err)
	}
	slog.Info("chromium started")
	c.browser = browser
	c.cancel = func() { cancelBrowser(); cancelAlloc() }
	return browser, nil
}

func (c *Chrome) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
}

// Render loads pageURL and returns the final URL and the rendered HTML.
func (c *Chrome) Render(ctx context.Context, pageURL string, feed *config.Feed, cookies []*http.Cookie) (string, string, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	browser, err := c.browserCtx()
	if err != nil {
		return "", "", err
	}
	tab, cancelTab := chromedp.NewContext(browser)
	defer cancelTab()
	tab, cancelTimeout := context.WithTimeout(tab, c.cfg.Timeout.Duration)
	defer cancelTimeout()
	// Abandon the tab when the caller gives up.
	stop := context.AfterFunc(ctx, cancelTab)
	defer stop()

	var actions []chromedp.Action
	actions = append(actions, network.Enable())
	if ua := feed.Chrome.UserAgent; ua != "" {
		actions = append(actions, emulation.SetUserAgentOverride(ua))
	}
	if len(cookies) > 0 {
		params := make([]*network.CookieParam, 0, len(cookies))
		for _, ck := range cookies {
			p := &network.CookieParam{
				Name: ck.Name, Value: ck.Value, Domain: ck.Domain, Path: ck.Path,
				Secure: ck.Secure, HTTPOnly: ck.HttpOnly,
			}
			if !ck.Expires.IsZero() {
				t := cdp.TimeSinceEpoch(ck.Expires)
				p.Expires = &t
			}
			params = append(params, p)
		}
		actions = append(actions, network.SetCookies(params))
	}
	var finalURL, html string
	actions = append(actions, chromedp.Navigate(pageURL), chromedp.WaitReady("body", chromedp.ByQuery))
	if sel := feed.Chrome.WaitSelector; sel != "" {
		actions = append(actions, chromedp.WaitReady(sel, chromedp.ByQuery))
	}
	if d := feed.Chrome.Wait.Duration; d > 0 {
		actions = append(actions, chromedp.Sleep(d))
	}
	actions = append(actions,
		chromedp.Location(&finalURL),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)
	start := time.Now()
	if err := chromedp.Run(tab, actions...); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s rendering page", c.cfg.Timeout.Duration)
		}
		return "", "", err
	}
	slog.Debug("rendered page", "url", pageURL, "took", time.Since(start))
	return finalURL, html, nil
}
