// Package extract turns Miniflux entries into readable, self-contained
// article HTML, either through Miniflux's fetch-content endpoint or through a
// headless chromium plus readability.
package extract

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"

	"literss/internal/config"
	"literss/internal/miniflux"
	"literss/internal/store"
)

type Service struct {
	cfg      *config.Config
	store    *store.Store
	mf       *miniflux.Client
	registry *miniflux.Registry
	chrome   *Chrome
	Images   *Images

	mu       sync.Mutex
	inflight map[int64]*call
}

type call struct {
	done chan struct{}
	a    *store.Article
	err  error
}

func New(cfg *config.Config, st *store.Store, mf *miniflux.Client, reg *miniflux.Registry, imageDir string) (*Service, error) {
	cookies := map[string][]*http.Cookie{}
	for _, r := range cfg.Rivers {
		for _, f := range r.Feeds {
			if len(f.Cookies) > 0 {
				cookies[f.URL] = f.Cookies
			}
		}
	}
	images, err := NewImages(imageDir, st, cookies)
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg: cfg, store: st, mf: mf, registry: reg,
		chrome:   NewChrome(cfg.Chrome, filepath.Join(cfg.DataDir, "chrome-extensions")),
		Images:   images,
		inflight: map[int64]*call{},
	}, nil
}

func (s *Service) Close() { s.chrome.Close() }

// Article returns the cached article for an entry, extracting it first if
// needed. Concurrent requests for the same entry share one extraction.
func (s *Service) Article(ctx context.Context, e *miniflux.Entry, feed *config.Feed) (*store.Article, error) {
	if a, err := s.store.Article(ctx, e.ID); err == nil {
		return a, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	s.mu.Lock()
	if c, ok := s.inflight[e.ID]; ok {
		s.mu.Unlock()
		select {
		case <-c.done:
			return c.a, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	s.inflight[e.ID] = c
	s.mu.Unlock()

	// Extraction outlives an impatient client so the result still gets cached.
	xctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	c.a, c.err = s.extract(xctx, e, feed)
	cancel()

	s.mu.Lock()
	delete(s.inflight, e.ID)
	s.mu.Unlock()
	close(c.done)
	return c.a, c.err
}

func (s *Service) extract(ctx context.Context, e *miniflux.Entry, feed *config.Feed) (*store.Article, error) {
	start := time.Now()
	base, _ := url.Parse(e.URL)
	var content string
	switch feed.Extractor {
	case config.ExtractorChrome:
		c, u, err := s.renderReadable(ctx, e.URL, feed)
		if err != nil {
			return nil, err
		}
		content, base = c, u
	default:
		c, err := s.mf.FetchContent(ctx, e.ID)
		if err != nil {
			return nil, fmt.Errorf("miniflux fetch-content %d: %w", e.ID, err)
		}
		content = c
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("no readable content found at %s", e.URL)
	}
	a, err := s.save(ctx, e, feed, content, base)
	if err != nil {
		return nil, err
	}
	slog.Info("extracted article", "entry", e.ID, "extractor", feed.Extractor, "took", time.Since(start).Round(time.Millisecond))
	return a, nil
}

func (s *Service) save(ctx context.Context, e *miniflux.Entry, feed *config.Feed, content string, base *url.URL) (*store.Article, error) {
	var ids []string
	var regErr error
	cleaned := Clean(content, base, func(abs string) string {
		id, err := s.Images.Register(ctx, abs, feed.URL)
		if err != nil {
			regErr = err
			return ""
		}
		ids = append(ids, id)
		return "/img/" + id
	})
	if regErr != nil {
		return nil, regErr
	}
	a := &store.Article{
		EntryID:     e.ID,
		Title:       e.Title,
		URL:         e.URL,
		FeedTitle:   FeedTitle(e, feed),
		PublishedAt: e.PublishedAt,
		Content:     cleaned,
	}
	if err := s.store.SaveArticle(ctx, a); err != nil {
		return nil, err
	}
	s.Images.Enqueue(ids...)
	return a, nil
}

// Fallback renders the feed's own entry content, for when extraction fails.
// It is not cached so the next view retries extraction.
func (s *Service) Fallback(ctx context.Context, e *miniflux.Entry, feed *config.Feed) *store.Article {
	base, _ := url.Parse(e.URL)
	var ids []string
	content := Clean(e.Content, base, func(abs string) string {
		id, err := s.Images.Register(ctx, abs, feed.URL)
		if err != nil {
			return ""
		}
		ids = append(ids, id)
		return "/img/" + id
	})
	s.Images.Enqueue(ids...)
	return &store.Article{
		EntryID: e.ID, Title: e.Title, URL: e.URL, FeedTitle: FeedTitle(e, feed),
		PublishedAt: e.PublishedAt, Content: content,
	}
}

func FeedTitle(e *miniflux.Entry, feed *config.Feed) string {
	if feed != nil && feed.Title != "" {
		return feed.Title
	}
	return e.Feed.Title
}

// Prefetch periodically extracts the latest entries of every river so that
// articles open instantly and survive the original going away.
func (s *Service) Prefetch(ctx context.Context) {
	interval := s.cfg.PrefetchInterval.Duration
	if interval <= 0 {
		return
	}
	select {
	case <-s.registry.Ready():
	case <-ctx.Done():
		return
	}
	// Entries whose extraction failed are retried after prefetchRetry rather
	// than on every pass.
	const prefetchRetry = 6 * time.Hour
	failed := map[int64]time.Time{}
	for {
		for id, t := range failed {
			if time.Since(t) > 2*prefetchRetry {
				delete(failed, id)
			}
		}
		for _, river := range s.cfg.Rivers {
			catID, ok := s.registry.Category(river.Slug)
			if !ok {
				continue
			}
			list, err := s.mf.CategoryEntries(ctx, catID, s.cfg.ItemsPerPage, 0)
			if err != nil {
				slog.Warn("prefetch: listing entries failed", "river", river.Slug, "err", err)
				continue
			}
			for _, e := range list.Entries {
				if ctx.Err() != nil {
					return
				}
				feed := s.registry.Feed(e.FeedID)
				if feed == nil || s.store.HasArticle(ctx, e.ID) {
					continue
				}
				if t, ok := failed[e.ID]; ok && time.Since(t) < prefetchRetry {
					continue
				}
				if _, err := s.Article(ctx, e, feed); err != nil {
					slog.Warn("prefetch: extraction failed", "entry", e.ID, "url", e.URL, "err", err)
					failed[e.ID] = time.Now()
				} else {
					delete(failed, e.ID)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// renderReadable renders a page in chromium and runs readability on it,
// returning the article HTML and the page's final URL.
func (s *Service) renderReadable(ctx context.Context, pageURL string, feed *config.Feed) (string, *url.URL, error) {
	finalURL, page, err := s.chrome.Render(ctx, pageURL, feed, feed.Cookies)
	if err != nil {
		return "", nil, fmt.Errorf("rendering %s: %w", pageURL, err)
	}
	base, err := url.Parse(finalURL)
	if err != nil || base.Host == "" {
		base, _ = url.Parse(pageURL)
	}
	art, err := readability.FromReader(strings.NewReader(page), base)
	if err != nil {
		return "", nil, fmt.Errorf("readability %s: %w", pageURL, err)
	}
	var b strings.Builder
	if err := art.RenderHTML(&b); err != nil {
		return "", nil, err
	}
	return b.String(), base, nil
}

// Preview extracts a page with a chrome feed's settings without caching
// anything, for tuning a feed's configuration.
func (s *Service) Preview(ctx context.Context, feed *config.Feed, pageURL string) (string, error) {
	if feed.Extractor != config.ExtractorChrome {
		return "", fmt.Errorf("feed %s uses the %s extractor; previews only work for chrome feeds", feed.URL, feed.Extractor)
	}
	content, base, err := s.renderReadable(ctx, pageURL, feed)
	if err != nil {
		return "", err
	}
	return Clean(content, base, func(abs string) string { return abs }), nil
}
