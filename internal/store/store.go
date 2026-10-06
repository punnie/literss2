// Package store persists users, passkeys, sessions, login links and the
// article/image cache in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	handle     BLOB NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS credentials (
	id           BLOB PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	data         TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS links (
	token_hash BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS articles (
	entry_id     INTEGER PRIMARY KEY,
	title        TEXT NOT NULL,
	url          TEXT NOT NULL,
	feed_title   TEXT NOT NULL,
	published_at INTEGER NOT NULL,
	content      TEXT NOT NULL,
	fetched_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS images (
	id           TEXT PRIMARY KEY,
	url          TEXT NOT NULL,
	feed_url     TEXT NOT NULL,
	content_type TEXT,
	fetched_at   INTEGER
);
`

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// NewToken returns a random URL-safe token and its hash for storage.
func NewToken() (string, []byte) {
	b := make([]byte, 32)
	rand.Read(b)
	t := base64.RawURLEncoding.EncodeToString(b)
	return t, HashToken(t)
}

func HashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// ---- users & credentials ----

type User struct {
	ID          int64
	Name        string
	Handle      []byte
	CreatedAt   time.Time
	Credentials []webauthn.Credential
}

func (u *User) WebAuthnID() []byte                         { return u.Handle }
func (u *User) WebAuthnName() string                       { return u.Name }
func (u *User) WebAuthnDisplayName() string                { return u.Name }
func (u *User) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

type Passkey struct {
	ID         []byte
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

func (s *Store) EnsureUser(ctx context.Context, name string) (*User, error) {
	handle := make([]byte, 64)
	rand.Read(handle)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (name, handle, created_at) VALUES (?, ?, ?) ON CONFLICT(name) DO NOTHING`,
		name, handle, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	return s.userBy(ctx, "name", name)
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return s.userBy(ctx, "id", id)
}

func (s *Store) UserByHandle(ctx context.Context, handle []byte) (*User, error) {
	return s.userBy(ctx, "handle", handle)
}

func (s *Store) UserByName(ctx context.Context, name string) (*User, error) {
	return s.userBy(ctx, "name", name)
}

func (s *Store) userBy(ctx context.Context, col string, v any) (*User, error) {
	u := &User{}
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, name, handle, created_at FROM users WHERE `+col+` = ?`, v).
		Scan(&u.ID, &u.Name, &u.Handle, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0)
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM credentials WHERE user_id = ? ORDER BY created_at`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return nil, err
		}
		u.Credentials = append(u.Credentials, c)
	}
	return u, rows.Err()
}

type UserSummary struct {
	Name      string
	CreatedAt time.Time
	Passkeys  int
	Sessions  int
}

func (s *Store) Users(ctx context.Context) ([]UserSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.name, u.created_at,
		       (SELECT count(*) FROM credentials c WHERE c.user_id = u.id),
		       (SELECT count(*) FROM sessions s WHERE s.user_id = u.id AND s.expires_at > ?)
		FROM users u ORDER BY u.name`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSummary
	for rows.Next() {
		var u UserSummary
		var created int64
		if err := rows.Scan(&u.Name, &created, &u.Passkeys, &u.Sessions); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(created, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUser(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddCredential(ctx context.Context, userID int64, name string, c *webauthn.Credential) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO credentials (id, user_id, name, data, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.ID, userID, name, string(data), time.Now().Unix())
	return err
}

// UpdateCredential stores the post-login state (sign count, flags).
func (s *Store) UpdateCredential(ctx context.Context, c *webauthn.Credential) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE credentials SET data = ?, last_used_at = ? WHERE id = ?`,
		string(data), time.Now().Unix(), c.ID)
	return err
}

func (s *Store) Passkeys(ctx context.Context, userID int64) ([]Passkey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_at, last_used_at FROM credentials WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Name, &created, &used); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(created, 0)
		if used.Valid {
			t := time.Unix(used.Int64, 0)
			p.LastUsedAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePasskey removes a passkey unless it is the user's last one.
func (s *Store) DeletePasskey(ctx context.Context, userID int64, id []byte) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM credentials WHERE user_id = ? AND id = ?
		AND (SELECT count(*) FROM credentials WHERE user_id = ?) > 1`, userID, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- login links ----

// CreateLink returns a single-use token that lets its holder register a
// passkey for the user.
func (s *Store) CreateLink(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	token, hash := NewToken()
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO links (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, userID, now.Unix(), now.Add(ttl).Unix())
	return token, err
}

// LinkUser returns the user a valid link belongs to.
func (s *Store) LinkUser(ctx context.Context, token string) (*User, error) {
	var userID int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM links WHERE token_hash = ? AND expires_at > ?`,
		HashToken(token), time.Now().Unix()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.UserByID(ctx, userID)
}

// ConsumeLink deletes a link, failing if it was already used or expired.
func (s *Store) ConsumeLink(ctx context.Context, token string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE token_hash = ? AND expires_at > ?`,
		HashToken(token), time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- sessions ----

func (s *Store) CreateSession(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	token, hash := NewToken()
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, userID, now.Unix(), now.Add(ttl).Unix())
	return token, err
}

// Session returns the user id and expiry of a valid session.
func (s *Store) Session(ctx context.Context, token string) (int64, time.Time, error) {
	var userID, exp int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		HashToken(token), time.Now().Unix()).Scan(&userID, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, ErrNotFound
	}
	return userID, time.Unix(exp, 0), err
}

func (s *Store) ExtendSession(ctx context.Context, token string, until time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, until.Unix(), HashToken(token))
	return err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(token))
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// Cleanup removes expired sessions and links.
func (s *Store) Cleanup(ctx context.Context) error {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE expires_at <= ?`, now)
	return err
}

// ---- article cache ----

type Article struct {
	EntryID     int64
	Title       string
	URL         string
	FeedTitle   string
	PublishedAt time.Time
	Content     string
	FetchedAt   time.Time
}

func (s *Store) Article(ctx context.Context, entryID int64) (*Article, error) {
	a := &Article{EntryID: entryID}
	var pub, fetched int64
	err := s.db.QueryRowContext(ctx,
		`SELECT title, url, feed_title, published_at, content, fetched_at FROM articles WHERE entry_id = ?`, entryID).
		Scan(&a.Title, &a.URL, &a.FeedTitle, &pub, &a.Content, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.PublishedAt, a.FetchedAt = time.Unix(pub, 0), time.Unix(fetched, 0)
	return a, nil
}

func (s *Store) HasArticle(ctx context.Context, entryID int64) bool {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM articles WHERE entry_id = ?`, entryID).Scan(&one) == nil
}

func (s *Store) SaveArticle(ctx context.Context, a *Article) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO articles (entry_id, title, url, feed_title, published_at, content, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(entry_id) DO UPDATE SET title = excluded.title, url = excluded.url,
			feed_title = excluded.feed_title, published_at = excluded.published_at,
			content = excluded.content, fetched_at = excluded.fetched_at`,
		a.EntryID, a.Title, a.URL, a.FeedTitle, a.PublishedAt.Unix(), a.Content, time.Now().Unix())
	return err
}

// ---- image cache ----

type Image struct {
	ID          string
	URL         string
	FeedURL     string
	ContentType string
	Fetched     bool
}

func (s *Store) AddImage(ctx context.Context, id, url, feedURL string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO images (id, url, feed_url) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		id, url, feedURL)
	return err
}

func (s *Store) Image(ctx context.Context, id string) (*Image, error) {
	im := &Image{ID: id}
	var ct sql.NullString
	var fetched sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT url, feed_url, content_type, fetched_at FROM images WHERE id = ?`, id).
		Scan(&im.URL, &im.FeedURL, &ct, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	im.ContentType, im.Fetched = ct.String, fetched.Valid
	return im, err
}

func (s *Store) MarkImageFetched(ctx context.Context, id, contentType string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE images SET content_type = ?, fetched_at = ? WHERE id = ?`,
		contentType, time.Now().Unix(), id)
	return err
}
