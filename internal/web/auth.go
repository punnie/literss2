package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"literss/internal/store"
)

const (
	sessionCookie  = "literss_session"
	ceremonyCookie = "literss_ceremony"
	ceremonyTTL    = 5 * time.Minute
	// Sessions are only extended in the database once per refreshEvery, to
	// avoid a write on every request.
	refreshEvery = time.Hour
	// Device links generated from the account page.
	deviceLinkTTL = 15 * time.Minute
)

// ceremonies holds WebAuthn challenge state between begin and finish.
type ceremonies struct {
	mu sync.Mutex
	m  map[string]*ceremony
}

type ceremony struct {
	data    *webauthn.SessionData
	userID  int64
	link    string
	expires time.Time
}

func (c *ceremonies) put(cer *ceremony) string {
	b := make([]byte, 24)
	rand.Read(b)
	key := base64.RawURLEncoding.EncodeToString(b)
	cer.expires = time.Now().Add(ceremonyTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	c.m[key] = cer
	return key
}

// take returns and forgets a ceremony; each challenge is usable once.
func (c *ceremonies) take(key string) *ceremony {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer := c.m[key]
	delete(c.m, key)
	if cer == nil || time.Now().After(cer.expires) {
		return nil
	}
	return cer
}

func (s *Server) secureCookies() bool { return strings.HasPrefix(s.cfg.BaseURL, "https://") }

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies()})
}

func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, userID int64) error {
	token, err := s.store.CreateSession(ctx, userID, s.cfg.SessionTTL.Duration)
	if err != nil {
		return err
	}
	s.setCookie(w, sessionCookie, token, s.cfg.SessionTTL.Duration)
	return nil
}

type ctxKey struct{}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxKey{}).(*store.User)
	return u
}

// withUser resolves the session cookie, sliding its expiry forward.
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			userID, expires, err := s.store.Session(r.Context(), c.Value)
			if err == nil {
				if u, err := s.store.UserByID(r.Context(), userID); err == nil {
					ttl := s.cfg.SessionTTL.Duration
					if time.Until(expires) < ttl-refreshEvery {
						if err := s.store.ExtendSession(r.Context(), c.Value, time.Now().Add(ttl)); err == nil {
							s.setCookie(w, sessionCookie, c.Value, ttl)
						}
					}
					r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
				}
			} else if errors.Is(err, store.ErrNotFound) {
				s.clearCookie(w, sessionCookie)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Error(w, "not signed in", http.StatusUnauthorized)
			}
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---- login ----

func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request) {
	assertion, data, err := s.webauthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		slog.Error("begin login", "err", err)
		jsonError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	key := s.ceremonies.put(&ceremony{data: data})
	s.setCookie(w, ceremonyCookie, key, ceremonyTTL)
	writeJSON(w, http.StatusOK, assertion)
}

func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) {
	cer := s.takeCeremony(w, r)
	if cer == nil || cer.userID != 0 {
		jsonError(w, http.StatusBadRequest, "sign-in expired, please try again")
		return
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		return s.store.UserByHandle(r.Context(), userHandle)
	}
	user, cred, err := s.webauthn.FinishPasskeyLogin(handler, *cer.data, r)
	if err != nil {
		slog.Warn("passkey login failed", "err", err)
		jsonError(w, http.StatusUnauthorized, "that passkey was not accepted")
		return
	}
	u := user.(*store.User)
	if err := s.store.UpdateCredential(r.Context(), cred); err != nil {
		slog.Error("updating credential", "err", err)
	}
	if err := s.startSession(r.Context(), w, u.ID); err != nil {
		slog.Error("creating session", "err", err)
		jsonError(w, http.StatusInternalServerError, "could not sign in")
		return
	}
	slog.Info("user signed in", "user", u.Name)
	writeJSON(w, http.StatusOK, map[string]string{"redirect": "/"})
}

func (s *Server) takeCeremony(w http.ResponseWriter, r *http.Request) *ceremony {
	c, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return nil
	}
	s.clearCookie(w, ceremonyCookie)
	return s.ceremonies.take(c.Value)
}

// ---- registration through a link ----

func (s *Server) registerBegin(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	u, err := s.store.LinkUser(r.Context(), token)
	if err != nil {
		jsonError(w, http.StatusNotFound, "this link is invalid or has expired")
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range u.Credentials {
		exclude = append(exclude, c.Descriptor())
	}
	creation, data, err := s.webauthn.BeginRegistration(u,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclude),
	)
	if err != nil {
		slog.Error("begin registration", "err", err)
		jsonError(w, http.StatusInternalServerError, "could not start passkey creation")
		return
	}
	key := s.ceremonies.put(&ceremony{data: data, userID: u.ID, link: token})
	s.setCookie(w, ceremonyCookie, key, ceremonyTTL)
	writeJSON(w, http.StatusOK, creation)
}

func (s *Server) registerFinish(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	cer := s.takeCeremony(w, r)
	if cer == nil || cer.userID == 0 || cer.link != token {
		jsonError(w, http.StatusBadRequest, "passkey creation expired, please try again")
		return
	}
	u, err := s.store.UserByID(r.Context(), cer.userID)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "unknown user")
		return
	}
	cred, err := s.webauthn.FinishRegistration(u, *cer.data, r)
	if err != nil {
		slog.Warn("passkey registration failed", "user", u.Name, "err", err)
		jsonError(w, http.StatusBadRequest, "the passkey could not be verified")
		return
	}
	// Consuming the link is the point of no return: a link registers one passkey.
	if err := s.store.ConsumeLink(r.Context(), token); err != nil {
		jsonError(w, http.StatusGone, "this link has already been used or has expired")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if rs := []rune(name); len(rs) > 64 {
		name = string(rs[:64])
	}
	if name == "" {
		name = "Passkey added " + time.Now().In(s.cfg.Location).Format("2006-01-02")
	}
	if err := s.store.AddCredential(r.Context(), u.ID, name, cred); err != nil {
		slog.Error("saving credential", "err", err)
		jsonError(w, http.StatusInternalServerError, "could not save the passkey")
		return
	}
	if err := s.startSession(r.Context(), w, u.ID); err != nil {
		jsonError(w, http.StatusInternalServerError, "could not sign in")
		return
	}
	slog.Info("passkey registered", "user", u.Name, "name", name)
	writeJSON(w, http.StatusOK, map[string]string{"redirect": "/"})
}
