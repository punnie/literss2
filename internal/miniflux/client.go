// Package miniflux is a small client for the parts of the Miniflux API that
// literss needs: https://miniflux.app/docs/api.html
package miniflux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{base: baseURL, token: token, http: &http.Client{Timeout: 90 * time.Second}}
}

type Category struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

type Feed struct {
	ID       int64    `json:"id"`
	FeedURL  string   `json:"feed_url"`
	SiteURL  string   `json:"site_url"`
	Title    string   `json:"title"`
	Category Category `json:"category"`
}

type Entry struct {
	ID          int64     `json:"id"`
	FeedID      int64     `json:"feed_id"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Content     string    `json:"content"`
	Author      string    `json:"author"`
	PublishedAt time.Time `json:"published_at"`
	Feed        Feed      `json:"feed"`
}

type EntryList struct {
	Total   int      `json:"total"`
	Entries []*Entry `json:"entries"`
}

// FeedSettings holds the fields literss manages on a feed. It is used both to
// create and to update feeds.
type FeedSettings struct {
	FeedURL        string `json:"feed_url,omitempty"`
	CategoryID     int64  `json:"category_id"`
	Title          string `json:"title,omitempty"`
	UserAgent      string `json:"user_agent"`
	Cookie         string `json:"cookie"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	Crawler        bool   `json:"crawler"`
	ScraperRules   string `json:"scraper_rules"`
	RewriteRules   string `json:"rewrite_rules"`
	BlocklistRules string `json:"blocklist_rules"`
	KeeplistRules  string `json:"keeplist_rules"`
}

type apiError struct {
	Status  int
	Message string `json:"error_message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("miniflux: HTTP %d: %s", e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("miniflux: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		e := &apiError{Status: resp.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, e) != nil || e.Message == "" {
			e.Message = string(b)
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// IsNotFound reports whether err is a 404 from Miniflux.
func IsNotFound(err error) bool {
	e, ok := err.(*apiError)
	return ok && e.Status == http.StatusNotFound
}

func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	var out []Category
	return out, c.do(ctx, http.MethodGet, "/v1/categories", nil, &out)
}

func (c *Client) CreateCategory(ctx context.Context, title string) (*Category, error) {
	var out Category
	return &out, c.do(ctx, http.MethodPost, "/v1/categories", map[string]string{"title": title}, &out)
}

func (c *Client) Feeds(ctx context.Context) ([]Feed, error) {
	var out []Feed
	return out, c.do(ctx, http.MethodGet, "/v1/feeds", nil, &out)
}

func (c *Client) CreateFeed(ctx context.Context, s FeedSettings) (int64, error) {
	var out struct {
		FeedID int64 `json:"feed_id"`
	}
	return out.FeedID, c.do(ctx, http.MethodPost, "/v1/feeds", s, &out)
}

func (c *Client) UpdateFeed(ctx context.Context, id int64, s FeedSettings) error {
	s.FeedURL = ""
	return c.do(ctx, http.MethodPut, "/v1/feeds/"+strconv.FormatInt(id, 10), s, nil)
}

// CategoryEntries lists entries of a category, newest first.
func (c *Client) CategoryEntries(ctx context.Context, categoryID int64, limit, offset int) (*EntryList, error) {
	q := url.Values{}
	q.Set("category_id", strconv.FormatInt(categoryID, 10))
	q.Set("order", "published_at")
	q.Set("direction", "desc")
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	var out EntryList
	return &out, c.do(ctx, http.MethodGet, "/v1/entries?"+q.Encode(), nil, &out)
}

func (c *Client) Entry(ctx context.Context, id int64) (*Entry, error) {
	var out Entry
	return &out, c.do(ctx, http.MethodGet, "/v1/entries/"+strconv.FormatInt(id, 10), nil, &out)
}

// FetchContent asks Miniflux to download and scrape the original article.
func (c *Client) FetchContent(ctx context.Context, id int64) (string, error) {
	var out struct {
		Content string `json:"content"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/entries/"+strconv.FormatInt(id, 10)+"/fetch-content?update_content=false", nil, &out)
	return out.Content, err
}
