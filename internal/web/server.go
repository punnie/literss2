// Package web serves the river pages, articles, cached images and the passkey
// sign-in flow.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"rsc.io/qr"

	"literss/internal/config"
	"literss/internal/extract"
	"literss/internal/miniflux"
	"literss/internal/store"
)

//go:embed templates static
var assets embed.FS

type Server struct {
	cfg        *config.Config
	store      *store.Store
	mf         *miniflux.Client
	registry   *miniflux.Registry
	extract    *extract.Service
	webauthn   *webauthn.WebAuthn
	ceremonies *ceremonies
	pages      map[string]*template.Template
	static     http.Handler
	assetHash  string

	riverMu    sync.Mutex
	riverCache map[string]riverCacheEntry
}

type riverCacheEntry struct {
	list    *miniflux.EntryList
	expires time.Time
}

const riverCacheTTL = time.Minute

func New(cfg *config.Config, st *store.Store, mf *miniflux.Client, reg *miniflux.Registry, ex *extract.Service) (*Server, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID(),
		RPDisplayName: "literss",
		RPOrigins:     []string{cfg.Origin()},
	})
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, store: st, mf: mf, registry: reg, extract: ex,
		webauthn:   wa,
		ceremonies: &ceremonies{m: map[string]*ceremony{}},
		pages:      map[string]*template.Template{},
		riverCache: map[string]riverCacheEntry{},
	}
	staticFS, _ := fs.Sub(assets, "static")
	s.static = http.StripPrefix("/static/", http.FileServerFS(staticFS))
	h := sha256.New()
	fs.WalkDir(staticFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(staticFS, p)
			h.Write(b)
		}
		return nil
	})
	s.assetHash = hex.EncodeToString(h.Sum(nil))[:10]

	funcs := template.FuncMap{
		"when":  s.formatTime,
		"asset": func(name string) string { return "/static/" + name + "?v=" + s.assetHash },
		"b64":   base64.RawURLEncoding.EncodeToString,
	}
	for _, page := range []string{"river", "article", "login", "link", "account", "message"} {
		t, err := template.New("base.html").Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		s.pages[page] = t
	}
	return s, nil
}

func (s *Server) formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.In(s.cfg.Location)
	if t.Year() != time.Now().In(s.cfg.Location).Year() {
		return t.Format("Jan 2 2006, 15:04")
	}
	return t.Format("Jan 2, 15:04")
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		s.static.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /auth/login/begin", s.loginBegin)
	mux.HandleFunc("POST /auth/login/finish", s.loginFinish)
	mux.HandleFunc("GET /link/{token}", s.linkPage)
	mux.HandleFunc("POST /link/{token}/begin", s.registerBegin)
	mux.HandleFunc("POST /link/{token}/finish", s.registerFinish)

	mux.HandleFunc("GET /{$}", s.requireUser(s.riverPage))
	mux.HandleFunc("GET /r/{slug}", s.requireUser(s.riverPage))
	mux.HandleFunc("GET /a/{id}", s.requireUser(s.articlePage))
	mux.HandleFunc("GET /img/{id}", s.requireUser(s.image))
	mux.HandleFunc("GET /account", s.requireUser(s.accountPage))
	mux.HandleFunc("POST /account/link", s.requireUser(s.createDeviceLink))
	mux.HandleFunc("POST /account/passkeys/delete", s.requireUser(s.deletePasskey))
	mux.HandleFunc("POST /logout", s.requireUser(s.logout))
	mux.HandleFunc("POST /logout/all", s.requireUser(s.logoutAll))
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	return securityHeaders(http.NewCrossOriginProtection().Handler(s.withUser(mux)))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}

// ---- rendering ----

type navRiver struct {
	Slug, Title string
	Href        string
}

type pageData struct {
	Title   string
	Rivers  []navRiver
	Current string
	User    *store.User
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, title, current string, data any) {
	pd := pageData{Title: title, Current: current, User: currentUser(r), Data: data}
	if pd.User != nil {
		for i, rv := range s.cfg.Rivers {
			href := "/r/" + rv.Slug
			if i == 0 {
				href = "/"
			}
			pd.Rivers = append(pd.Rivers, navRiver{Slug: rv.Slug, Title: rv.Title, Href: href})
		}
	}
	var buf bytes.Buffer
	if err := s.pages[page].Execute(&buf, pd); err != nil {
		slog.Error("rendering template", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) message(w http.ResponseWriter, r *http.Request, status int, title, text string) {
	s.render(w, r, status, "message", title, "", text)
}

// ---- rivers ----

type riverItem struct {
	ID        int64
	Title     string
	Source    string
	Published time.Time
}

type riverData struct {
	River    *config.River
	Items    []riverItem
	Page     int
	PrevHref string
	NextHref string
	NotReady bool
}

func (s *Server) riverPage(w http.ResponseWriter, r *http.Request) {
	river := &s.cfg.Rivers[0]
	base := "/"
	if slug := r.PathValue("slug"); slug != "" {
		if river = s.cfg.River(slug); river == nil {
			s.message(w, r, http.StatusNotFound, "Not found", "There is no such river.")
			return
		}
		base = "/r/" + slug
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	data := &riverData{River: river, Page: page}

	catID, ok := s.registry.Category(river.Slug)
	if !ok {
		data.NotReady = true
		s.render(w, r, http.StatusServiceUnavailable, "river", river.Title, river.Slug, data)
		return
	}
	list, err := s.riverEntries(r.Context(), river.Slug, catID, page)
	if err != nil {
		slog.Error("listing entries", "river", river.Slug, "err", err)
		s.message(w, r, http.StatusBadGateway, "Unavailable", "Could not reach the feed backend. Try again in a moment.")
		return
	}
	for _, e := range list.Entries {
		feed := s.registry.Feed(e.FeedID)
		if feed == nil {
			continue
		}
		data.Items = append(data.Items, riverItem{ID: e.ID, Title: e.Title, Source: extract.FeedTitle(e, feed), Published: e.PublishedAt})
	}
	if page > 1 {
		data.PrevHref = base
		if page > 2 {
			data.PrevHref += "?page=" + strconv.Itoa(page-1)
		}
	}
	if page*s.cfg.ItemsPerPage < list.Total {
		data.NextHref = base + "?page=" + strconv.Itoa(page+1)
	}
	s.render(w, r, http.StatusOK, "river", river.Title, river.Slug, data)
}

func (s *Server) riverEntries(ctx context.Context, slug string, catID int64, page int) (*miniflux.EntryList, error) {
	key := slug + ":" + strconv.Itoa(page)
	s.riverMu.Lock()
	c, ok := s.riverCache[key]
	s.riverMu.Unlock()
	if ok && time.Now().Before(c.expires) {
		return c.list, nil
	}
	list, err := s.mf.CategoryEntries(ctx, catID, s.cfg.ItemsPerPage, (page-1)*s.cfg.ItemsPerPage)
	if err != nil {
		return nil, err
	}
	s.riverMu.Lock()
	s.riverCache[key] = riverCacheEntry{list: list, expires: time.Now().Add(riverCacheTTL)}
	s.riverMu.Unlock()
	return list, nil
}

// ---- articles ----

type articleData struct {
	Article  *store.Article
	Content  template.HTML
	Fallback bool
}

func (s *Server) articlePage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.message(w, r, http.StatusNotFound, "Not found", "There is no such article.")
		return
	}
	a, err := s.store.Article(r.Context(), id)
	fallback := false
	if errors.Is(err, store.ErrNotFound) {
		e, err := s.mf.Entry(r.Context(), id)
		if err != nil {
			if miniflux.IsNotFound(err) {
				s.message(w, r, http.StatusNotFound, "Not found", "There is no such article.")
			} else {
				slog.Error("fetching entry", "id", id, "err", err)
				s.message(w, r, http.StatusBadGateway, "Unavailable", "Could not reach the feed backend. Try again in a moment.")
			}
			return
		}
		feed := s.registry.Feed(e.FeedID)
		if feed == nil {
			s.message(w, r, http.StatusNotFound, "Not found", "There is no such article.")
			return
		}
		a, err = s.extract.Article(r.Context(), e, feed)
		if err != nil {
			slog.Warn("extraction failed, using feed content", "entry", id, "err", err)
			a, fallback = s.extract.Fallback(r.Context(), e, feed), true
		}
	} else if err != nil {
		slog.Error("loading article", "id", id, "err", err)
		s.message(w, r, http.StatusInternalServerError, "Error", "Something went wrong.")
		return
	}
	s.render(w, r, http.StatusOK, "article", a.Title, "", &articleData{
		Article:  a,
		Content:  template.HTML(a.Content), // sanitized by extract.Clean
		Fallback: fallback,
	})
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	path, ct, err := s.extract.Images.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("serving image", "id", r.PathValue("id"), "err", err)
		}
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	// Images (SVG in particular) must never run as documents on this origin.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// ---- auth pages ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Sign in", "", nil)
}

type linkData struct {
	Token string
	Name  string
}

func (s *Server) linkPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	u, err := s.store.LinkUser(r.Context(), token)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Link expired", "This link is invalid, has already been used, or has expired. Ask for a new one.")
		return
	}
	s.render(w, r, http.StatusOK, "link", "Create a passkey", "", &linkData{Token: token, Name: u.Name})
}

// ---- account ----

type accountData struct {
	Passkeys  []store.Passkey
	Link      string
	LinkQR    template.URL
	LinkUntil time.Time
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, &accountData{})
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, d *accountData) {
	pk, err := s.store.Passkeys(r.Context(), currentUser(r).ID)
	if err != nil {
		slog.Error("listing passkeys", "err", err)
	}
	d.Passkeys = pk
	s.render(w, r, http.StatusOK, "account", "Account", "", d)
}

func (s *Server) createDeviceLink(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	token, err := s.store.CreateLink(r.Context(), u.ID, deviceLinkTTL)
	if err != nil {
		slog.Error("creating link", "err", err)
		s.message(w, r, http.StatusInternalServerError, "Error", "Could not create a link.")
		return
	}
	link := s.cfg.BaseURL + "/link/" + token
	d := &accountData{Link: link, LinkUntil: time.Now().Add(deviceLinkTTL)}
	if code, err := qr.Encode(link, qr.M); err == nil {
		code.Scale = 6
		d.LinkQR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
	}
	s.renderAccount(w, r, d)
}

func (s *Server) deletePasskey(w http.ResponseWriter, r *http.Request) {
	id, err := base64.RawURLEncoding.DecodeString(r.PostFormValue("id"))
	if err == nil {
		err = s.store.DeletePasskey(r.Context(), currentUser(r).ID, id)
	}
	if err != nil {
		s.message(w, r, http.StatusBadRequest, "Not removed", "That passkey could not be removed. Your last passkey cannot be removed.")
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(r.Context(), c.Value)
	}
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) logoutAll(w http.ResponseWriter, r *http.Request) {
	s.store.DeleteUserSessions(r.Context(), currentUser(r).ID)
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
