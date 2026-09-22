// Package cookiebundle keeps allowlisted, origin-scoped cookies only in RAM.
// Credentials in scopes are one-way digests. Cookies are never serialized.
package cookiebundle

import (
	"crypto/sha256"
	"encoding/hex"
	"golang.org/x/net/publicsuffix"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

type Scope struct{ Credential, Route string }
type key struct {
	Scope
	Origin string
}
type bundle struct {
	jar                        *cookiejar.Jar
	issued, committed, version uint64
	touched                    time.Time
}
type Lease struct {
	key         key
	owner       *bundle
	sequence    uint64
	Version     uint64
	Cookies     []*http.Cookie
	Fingerprint string
}
type Store struct {
	mu         sync.Mutex
	entries    map[key]*bundle
	maxEntries int
	sessionTTL time.Duration
}

func New(maxEntries int, sessionTTL time.Duration) *Store {
	if maxEntries < 1 {
		maxEntries = 128
	}
	if sessionTTL <= 0 {
		sessionTTL = 180 * time.Second
	}
	return &Store{entries: make(map[key]*bundle), maxEntries: maxEntries, sessionTTL: sessionTTL}
}
func (s *Store) Begin(scope Scope, u *url.URL) Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{scope, strings.ToLower(u.Scheme + "://" + u.Host)}
	b := s.entries[k]
	if b == nil {
		if len(s.entries) >= s.maxEntries {
			var oldest key
			var at time.Time
			for k, v := range s.entries {
				if at.IsZero() || v.touched.Before(at) {
					oldest = k
					at = v.touched
				}
			}
			delete(s.entries, oldest)
		}
		jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
		b = &bundle{jar: jar}
		s.entries[k] = b
	}
	b.issued++
	b.touched = time.Now()
	cookies := b.jar.Cookies(u)
	var raw strings.Builder
	for _, c := range cookies {
		raw.WriteString(c.Name)
		raw.WriteByte('=')
		raw.WriteString(c.Value)
		raw.WriteByte(0)
	}
	fp := ""
	if raw.Len() > 0 {
		sum := sha256.Sum256([]byte(raw.String()))
		fp = hex.EncodeToString(sum[:8])
	}
	return Lease{key: k, owner: b, sequence: b.issued, Version: b.version, Cookies: cookies, Fingerprint: fp}
}

// Commit prevents an older response overwriting cookies already published by a
// newer request. Evicted scopes cannot be revived by a late response.
func (s *Store) Commit(l Lease, u *url.URL, cookies []*http.Cookie) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.entries[l.key]
	if b == nil || b != l.owner || l.sequence <= b.committed || len(cookies) == 0 {
		return false
	}
	if strings.ToLower(u.Scheme+"://"+u.Host) != l.key.Origin {
		return false
	}
	safe := make([]*http.Cookie, 0, 2)
	for _, c := range cookies {
		if c == nil || (c.Name != "__cflb" && c.Name != "__oailb") || len(c.Value) > 4096 {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if domain != "" && domain != strings.ToLower(u.Hostname()) {
			continue
		}
		cp := *c
		cp.Domain = "" // Never broaden to sibling hosts.
		if cp.Path != "" {
			p := path.Clean(cp.Path)
			if p != cp.Path || !strings.HasPrefix(p, "/") || !(u.Path == p || strings.HasPrefix(u.Path, strings.TrimRight(p, "/")+"/")) {
				continue
			}
		}
		if cp.Secure && u.Scheme != "https" {
			continue
		}
		if cp.MaxAge == 0 && cp.Expires.IsZero() {
			cp.Expires = time.Now().Add(s.sessionTTL)
		}
		if cp.Valid() != nil {
			continue
		}
		safe = append(safe, &cp)
		if len(safe) == 16 {
			break
		}
	}
	if len(safe) == 0 {
		return false
	}
	b.jar.SetCookies(u, safe)
	b.committed = l.sequence
	b.version++
	b.touched = time.Now()
	return true
}
func (s *Store) Size() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.entries) }
