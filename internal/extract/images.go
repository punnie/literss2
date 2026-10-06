package extract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"literss/internal/store"
)

const (
	maxImageSize     = 25 << 20
	defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

// Images is a disk cache for article images, so pages never hotlink.
type Images struct {
	dir   string
	store *store.Store

	clients map[string]*http.Client // feed url -> client with that feed's cookies
	plain   *http.Client

	mu       sync.Mutex
	inflight map[string]chan struct{}
	queue    chan string
}

func NewImages(dir string, st *store.Store, feedCookies map[string][]*http.Cookie) (*Images, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	im := &Images{
		dir: dir, store: st,
		clients:  map[string]*http.Client{},
		plain:    newImageClient(nil),
		inflight: map[string]chan struct{}{},
		queue:    make(chan string, 4096),
	}
	for feedURL, cookies := range feedCookies {
		jar, _ := cookiejar.New(nil)
		for _, c := range cookies {
			host := strings.TrimPrefix(c.Domain, ".")
			if host == "" {
				continue
			}
			jar.SetCookies(&url.URL{Scheme: "https", Host: host, Path: "/"}, []*http.Cookie{c})
		}
		im.clients[feedURL] = newImageClient(jar)
	}
	return im, nil
}

// publicOnly refuses connections to loopback, private and link-local
// addresses: image URLs come from third-party content.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("refusing to fetch image from non-public address %s", host)
	}
	return nil
}

func newImageClient(jar http.CookieJar) *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, Control: publicOnly}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
	return &http.Client{Transport: tr, Jar: jar, Timeout: 2 * time.Minute}
}

func imageID(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:16])
}

func (im *Images) path(id string) string { return filepath.Join(im.dir, id[:2], id) }

// Register records an image URL and returns its local path.
func (im *Images) Register(ctx context.Context, src, feedURL string) (string, error) {
	id := imageID(src)
	if err := im.store.AddImage(ctx, id, src, feedURL); err != nil {
		return "", err
	}
	return id, nil
}

// Enqueue schedules images for background download.
func (im *Images) Enqueue(ids ...string) {
	for _, id := range ids {
		select {
		case im.queue <- id:
		default: // Queue full; it will be fetched on first view.
		}
	}
}

// Run downloads queued images until ctx is done.
func (im *Images) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-im.queue:
			if _, _, err := im.Get(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
				slog.Warn("caching image failed", "id", id, "err", err)
			}
		}
	}
}

// Get returns the cached file path and content type of an image, downloading
// it first if needed.
func (im *Images) Get(ctx context.Context, id string) (string, string, error) {
	if len(id) != 32 {
		return "", "", store.ErrNotFound
	}
	for {
		rec, err := im.store.Image(ctx, id)
		if err != nil {
			return "", "", err
		}
		if rec.Fetched {
			return im.path(id), rec.ContentType, nil
		}
		im.mu.Lock()
		wait, busy := im.inflight[id]
		if !busy {
			done := make(chan struct{})
			im.inflight[id] = done
			im.mu.Unlock()
			err := im.download(ctx, rec)
			im.mu.Lock()
			delete(im.inflight, id)
			im.mu.Unlock()
			close(done)
			if err != nil {
				return "", "", err
			}
			continue
		}
		im.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
		// Re-check; if the other download failed we try ourselves.
	}
}

func (im *Images) download(ctx context.Context, rec *store.Image) error {
	client := im.clients[rec.FeedURL]
	if client == nil {
		client = im.plain
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rec.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/svg+xml,image/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: HTTP %d", rec.URL, resp.StatusCode)
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(resp.Body, head)
	head = head[:n]
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(ct, "image/") {
		ct = http.DetectContentType(head)
	}
	if !strings.HasPrefix(ct, "image/") {
		return fmt.Errorf("fetching %s: not an image (%s)", rec.URL, ct)
	}
	dst := im.path(rec.ID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(head); err != nil {
		tmp.Close()
		return err
	}
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, maxImageSize))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if written+int64(len(head)) >= maxImageSize {
		return fmt.Errorf("fetching %s: image too large", rec.URL)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	return im.store.MarkImageFetched(ctx, rec.ID, ct)
}
