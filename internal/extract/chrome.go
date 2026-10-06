package extract

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"literss/internal/config"
)

const (
	// How long to wait for extensions to start after launching chromium.
	extensionStartTimeout = 20 * time.Second
	// Upper bound for a "scroll to bottom" step, for infinite-scroll pages.
	maxScrollTime = 20 * time.Second
)

// Chrome renders pages in a shared headless chromium, one tab per page.
type Chrome struct {
	cfg    config.Chrome
	extDir string
	sem    chan struct{}

	mu      sync.Mutex
	proc    *browserProcess
	browser context.Context
	cancel  context.CancelFunc
}

// NewChrome returns a renderer; extDir holds writable copies of extensions.
func NewChrome(cfg config.Chrome, extDir string) *Chrome {
	return &Chrome{cfg: cfg, extDir: extDir, sem: make(chan struct{}, cfg.MaxConcurrent)}
}

// browserCtx starts chromium on first use, and again if it has died.
func (c *Chrome) browserCtx() (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.browser != nil && c.browser.Err() == nil {
		return c.browser, nil
	}
	c.stop()
	proc, err := startBrowser(c.cfg, c.extDir)
	if err != nil {
		return nil, err
	}
	browser, cancel, err := proc.connect()
	if err != nil {
		proc.kill()
		return nil, err
	}
	if len(c.cfg.Extensions) > 0 {
		if err := waitForExtensions(browser, len(c.cfg.Extensions)); err != nil {
			cancel()
			proc.kill()
			return nil, err
		}
	}
	if c.cfg.UBlockMode != "" {
		found := false
		for _, ext := range proc.extensions {
			if !ext.isUBOLite() {
				continue
			}
			found = true
			if err := setUBlockMode(browser, ext, c.cfg.UBlockMode); err != nil {
				cancel()
				proc.kill()
				return nil, err
			}
		}
		if !found {
			slog.Warn("chrome.ublock_mode is set but uBlock Origin Lite is not among chrome.extensions")
		}
	}
	slog.Info("chromium started", "extensions", len(c.cfg.Extensions), "ublock_mode", c.cfg.UBlockMode)
	c.proc, c.browser, c.cancel = proc, browser, cancel
	return browser, nil
}

func (c *Chrome) stop() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.proc != nil {
		c.proc.kill()
	}
	c.proc, c.browser, c.cancel = nil, nil, nil
}

// waitForExtensions blocks until n extension service workers are running, so
// that content blocking is in place before the first page loads.
func waitForExtensions(browser context.Context, n int) error {
	deadline := time.Now().Add(extensionStartTimeout)
	for {
		targets, err := chromedp.Targets(browser)
		if err != nil {
			return fmt.Errorf("listing chromium targets: %w", err)
		}
		running := 0
		for _, t := range targets {
			if t.Type == "service_worker" && strings.HasPrefix(t.URL, "chrome-extension://") {
				running++
			}
		}
		if running >= n {
			// Give extensions a moment to register their rules and scripts.
			time.Sleep(2 * time.Second)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("only %d of %d chromium extensions started within %s", running, n, extensionStartTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (c *Chrome) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stop()
}

// Render loads pageURL, runs the feed's steps and returns the final URL and
// the rendered HTML.
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
	tab, cancelTab, err := newWindow(browser)
	if err != nil {
		return "", "", err
	}
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
	actions = append(actions, chromedp.Navigate(pageURL), chromedp.WaitReady("body", chromedp.ByQuery))
	for i, s := range feed.Chrome.Steps {
		actions = append(actions, stepAction(i, s))
	}
	if !feed.Chrome.KeepHidden {
		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			var removed int
			if err := chromedp.Evaluate(removeHiddenJS, &removed).Do(ctx); err != nil {
				return fmt.Errorf("removing hidden elements: %w", err)
			}
			slog.Debug("removed hidden elements", "url", pageURL, "count", removed)
			return nil
		}))
	}
	var finalURL, html string
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

// stepAction turns a configured step into a chromedp action. Errors name the
// step so a broken selector is easy to find in the config.
func stepAction(i int, s config.Step) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		switch {
		case s.Wait.Duration > 0:
			err = chromedp.Sleep(s.Wait.Duration).Do(ctx)
		case s.WaitFor != "":
			err = chromedp.WaitReady(s.WaitFor, chromedp.ByQuery).Do(ctx)
		case s.Scroll == "bottom":
			var res struct{ Rounds, MaxY, Height float64 }
			err = evalAsync(ctx, fmt.Sprintf(scrollToBottomJS, maxScrollTime.Milliseconds()), &res)
			slog.Debug("scrolled to bottom", "rounds", res.Rounds, "max_y", res.MaxY, "height", res.Height)
		case s.Click != "":
			var clicked bool
			err = chromedp.Evaluate("(() => { const el = document.querySelector("+strconv.Quote(s.Click)+"); if (el) el.click(); return !!el })()", &clicked).Do(ctx)
			slog.Debug("click step", "selector", s.Click, "found", clicked)
		case s.Remove != "":
			var removed int
			err = chromedp.Evaluate("(() => { const els = document.querySelectorAll("+strconv.Quote(s.Remove)+"); els.forEach(e => e.remove()); return els.length })()", &removed).Do(ctx)
			slog.Debug("remove step", "selector", s.Remove, "count", removed)
		}
		if err != nil {
			return fmt.Errorf("chrome.steps[%d] (%s): %w", i, s, err)
		}
		return nil
	})
}

func evalAsync(ctx context.Context, js string, res any) error {
	return chromedp.Evaluate(js, res, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}).Do(ctx)
}

// scrollToBottomJS scrolls a screen at a time, so lazy loaders watching for
// elements entering the viewport fire, until the bottom has been reached and
// the page has stopped growing, or the time limit (ms) passes.
const scrollToBottomJS = `(async (limit) => {
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  const deadline = Date.now() + limit;
  let lastHeight = -1, stable = 0, rounds = 0, maxY = 0;
  while (Date.now() < deadline) {
    window.scrollBy(0, Math.max(200, window.innerHeight * 0.8));
    rounds++;
    maxY = Math.max(maxY, window.scrollY);
    await sleep(150);
    const height = document.documentElement.scrollHeight;
    if (window.scrollY + window.innerHeight >= height - 2) {
      if (height === lastHeight) {
        if (++stable >= 3) break;
      } else {
        stable = 0;
      }
      lastHeight = height;
      await sleep(400);
    }
  }
  window.scrollTo(0, 0);
  return { rounds, maxY, height: document.documentElement.scrollHeight };
})(%d)`

// removeHiddenJS removes body elements that are not displayed, including
// anything a content blocker hid with cosmetic filters.
const removeHiddenJS = `(() => {
  let n = 0;
  for (const el of Array.from(document.body.querySelectorAll("*"))) {
    if (el.isConnected && getComputedStyle(el).display === "none") {
      el.remove();
      n++;
    }
  }
  return n;
})()`

// setUBlockMode sets uBO Lite's default filtering mode the way its settings
// page does: by messaging the extension from one of its own pages. The
// profile is temporary, so this runs on every launch.
func setUBlockMode(browser context.Context, ext loadedExtension, mode string) error {
	level := config.UBlockModes[mode]
	tab, cancel := chromedp.NewContext(browser)
	defer cancel()
	tab, cancelTimeout := context.WithTimeout(tab, 30*time.Second)
	defer cancelTimeout()
	var got int
	err := chromedp.Run(tab,
		chromedp.Navigate("chrome-extension://"+ext.id+"/dashboard.html"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			js := fmt.Sprintf(`chrome.runtime.sendMessage({ what: "setDefaultFilteringMode", level: %d })`, level)
			return evalAsync(ctx, js, &got)
		}),
	)
	if err != nil {
		return fmt.Errorf("setting uBlock Origin Lite mode: %w", err)
	}
	if got != level {
		return fmt.Errorf("uBlock Origin Lite refused filtering mode %q (got level %d)", mode, got)
	}
	return nil
}

// newWindow opens a page in its own window. Headless chromium only renders
// frames for the front tab of a window; in a background tab, lazy loaders
// (IntersectionObserver, requestAnimationFrame) never run.
func newWindow(browser context.Context) (context.Context, context.CancelFunc, error) {
	var id target.ID
	err := chromedp.Run(browser, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		id, err = target.CreateTarget("about:blank").WithNewWindow(true).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		return err
	}))
	if err != nil {
		return nil, nil, fmt.Errorf("opening window: %w", err)
	}
	tab, cancel := chromedp.NewContext(browser, chromedp.WithTargetID(id))
	return tab, func() {
		cancel()
		chromedp.Run(browser, chromedp.ActionFunc(func(ctx context.Context) error {
			return target.CloseTarget(id).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		}))
	}, nil
}
