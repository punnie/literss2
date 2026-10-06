package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// LoadCookies reads either a Netscape cookies.txt file or a JSON array of
// cookies as exported by browser extensions (name, value, domain, path,
// secure, httpOnly, expirationDate).
func LoadCookies(path string) ([]*http.Cookie, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '[' {
		return parseJSONCookies(t)
	}
	return parseNetscapeCookies(b)
}

func parseJSONCookies(b []byte) ([]*http.Cookie, error) {
	var raw []struct {
		Name           string  `json:"name"`
		Value          string  `json:"value"`
		Domain         string  `json:"domain"`
		Path           string  `json:"path"`
		Secure         bool    `json:"secure"`
		HTTPOnly       bool    `json:"httpOnly"`
		ExpirationDate float64 `json:"expirationDate"`
		Expires        float64 `json:"expires"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parsing JSON cookies: %w", err)
	}
	out := make([]*http.Cookie, 0, len(raw))
	for _, r := range raw {
		c := &http.Cookie{Name: r.Name, Value: r.Value, Domain: r.Domain, Path: r.Path, Secure: r.Secure, HttpOnly: r.HTTPOnly}
		exp := r.ExpirationDate
		if exp == 0 {
			exp = r.Expires
		}
		if exp > 0 {
			sec, frac := math.Modf(exp)
			c.Expires = time.Unix(int64(sec), int64(frac*1e9))
		}
		if c.Path == "" {
			c.Path = "/"
		}
		out = append(out, c)
	}
	return out, nil
}

func parseNetscapeCookies(b []byte) ([]*http.Cookie, error) {
	var out []*http.Cookie
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		httpOnly := false
		if rest, ok := strings.CutPrefix(line, "#HttpOnly_"); ok {
			line, httpOnly = rest, true
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			return nil, fmt.Errorf("cookies.txt line %d: expected 7 tab-separated fields", n)
		}
		c := &http.Cookie{
			Domain:   f[0],
			Path:     f[2],
			Secure:   strings.EqualFold(f[3], "TRUE"),
			Name:     f[5],
			Value:    f[6],
			HttpOnly: httpOnly,
		}
		if exp, err := strconv.ParseInt(f[4], 10, 64); err == nil && exp > 0 {
			c.Expires = time.Unix(exp, 0)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// CookieHeader renders the feed's unexpired cookies as a Cookie header value,
// which is the form Miniflux takes them in.
func (f *Feed) CookieHeader() string {
	now := time.Now()
	parts := make([]string, 0, len(f.Cookies))
	for _, c := range f.Cookies {
		if !c.Expires.IsZero() && c.Expires.Before(now) {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}
