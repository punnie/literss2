package miniflux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"literss/internal/config"
)

// Registry maps configured rivers and feeds onto Miniflux IDs. Each river is a
// Miniflux category with the river's title; each feed lives in its river's
// category.
type Registry struct {
	cfg    *config.Config
	client *Client

	mu         sync.RWMutex
	categories map[string]int64       // river slug -> category id
	feeds      map[int64]*config.Feed // miniflux feed id -> configured feed
	ready      chan struct{}
	readyOnce  sync.Once
}

func NewRegistry(cfg *config.Config, client *Client) *Registry {
	return &Registry{cfg: cfg, client: client, ready: make(chan struct{})}
}

// Ready is closed after the first successful sync.
func (r *Registry) Ready() <-chan struct{} { return r.ready }

func (r *Registry) Category(slug string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.categories[slug]
	return id, ok
}

// Feed returns the configuration of a Miniflux feed, or nil if the feed is not
// part of any river (and therefore must not be shown).
func (r *Registry) Feed(id int64) *config.Feed {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.feeds[id]
}

// Run syncs until it succeeds once, then again every sync_interval.
func (r *Registry) Run(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		err := r.Sync(ctx)
		wait := r.cfg.SyncInterval.Duration
		if err != nil {
			slog.Error("miniflux sync failed", "err", err)
			select {
			case <-r.ready:
			default:
				wait = backoff
				backoff = min(backoff*2, 5*time.Minute)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func normURL(u string) string { return strings.TrimRight(strings.ToLower(strings.TrimSpace(u)), "/") }

// Sync makes sure every configured river and feed exists in Miniflux with the
// configured settings. Feeds that are not in the configuration are left alone.
func (r *Registry) Sync(ctx context.Context) error {
	cats, err := r.client.Categories(ctx)
	if err != nil {
		return fmt.Errorf("listing categories: %w", err)
	}
	catByTitle := map[string]int64{}
	for _, c := range cats {
		catByTitle[strings.ToLower(c.Title)] = c.ID
	}
	existing, err := r.client.Feeds(ctx)
	if err != nil {
		return fmt.Errorf("listing feeds: %w", err)
	}
	feedByURL := map[string]int64{}
	for _, f := range existing {
		feedByURL[normURL(f.FeedURL)] = f.ID
	}

	categories := map[string]int64{}
	feeds := map[int64]*config.Feed{}
	var errs []error
	for ri := range r.cfg.Rivers {
		river := &r.cfg.Rivers[ri]
		catID, ok := catByTitle[strings.ToLower(river.Title)]
		if !ok {
			c, err := r.client.CreateCategory(ctx, river.Title)
			if err != nil {
				errs = append(errs, fmt.Errorf("river %s: creating category: %w", river.Slug, err))
				continue
			}
			slog.Info("created miniflux category", "river", river.Slug, "id", c.ID)
			catID = c.ID
		}
		categories[river.Slug] = catID

		for fi := range river.Feeds {
			f := &river.Feeds[fi]
			s := FeedSettings{
				FeedURL:        f.URL,
				CategoryID:     catID,
				Title:          f.Title,
				UserAgent:      f.UserAgent,
				Cookie:         f.CookieHeader(),
				Username:       f.Username,
				Password:       f.Password,
				Crawler:        f.Crawler,
				ScraperRules:   f.ScraperRules,
				RewriteRules:   f.RewriteRules,
				BlocklistRules: f.BlocklistRules,
				KeeplistRules:  f.KeeplistRules,
			}
			id, ok := feedByURL[normURL(f.URL)]
			if ok {
				err = r.client.UpdateFeed(ctx, id, s)
			} else {
				id, err = r.client.CreateFeed(ctx, s)
				if err == nil {
					slog.Info("created miniflux feed", "river", river.Slug, "url", f.URL, "id", id)
					if f.Title != "" {
						// Creation ignores the title; set it afterwards.
						err = r.client.UpdateFeed(ctx, id, s)
					}
				}
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("feed %s: %w", f.URL, err))
			}
			if id != 0 {
				// Even if updating failed, the feed exists and belongs here.
				feeds[id] = f
			}
		}
	}

	r.mu.Lock()
	r.categories, r.feeds = categories, feeds
	r.mu.Unlock()
	if len(categories) > 0 {
		r.readyOnce.Do(func() { close(r.ready) })
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	slog.Info("miniflux sync complete", "rivers", len(categories), "feeds", len(feeds))
	return nil
}
