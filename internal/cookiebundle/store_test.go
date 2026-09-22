package cookiebundle

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIsolationDeletionAndLateResponse(t *testing.T) {
	u, _ := url.Parse("https://example.com/backend-api/codex/responses")
	s := New(8, time.Minute)
	scope := Scope{"credential", "route"}
	a := s.Begin(scope, u)
	b := s.Begin(scope, u)
	if !s.Commit(b, u, []*http.Cookie{{Name: "__cflb", Value: "new", Path: "/", Secure: true}, {Name: "__oailb", Value: "paired", Path: "/"}}) {
		t.Fatal("not saved")
	}
	if s.Commit(a, u, []*http.Cookie{{Name: "__cflb", Value: "old"}}) {
		t.Fatal("late overwrite")
	}
	current := s.Begin(scope, u)
	if len(current.Cookies) != 2 || current.Version != 1 || current.Fingerprint == "" {
		t.Fatal("missing cookies")
	}
	for _, other := range []Scope{{"other", "route"}, {"credential", "other"}} {
		if len(s.Begin(other, u).Cookies) != 0 {
			t.Fatal("scope leak")
		}
	}
	other, _ := url.Parse("https://other.example.com/backend-api/codex/responses")
	if len(s.Begin(scope, other).Cookies) != 0 {
		t.Fatal("origin leak")
	}
	s.Commit(current, u, []*http.Cookie{{Name: "__cflb", Path: "/", MaxAge: -1}})
	cookies := s.Begin(scope, u).Cookies
	if len(cookies) != 1 || cookies[0].Name != "__oailb" {
		t.Fatal("deletion lost or removed bundle")
	}
}
func TestAllowlistAndExpiry(t *testing.T) {
	u, _ := url.Parse("https://example.com/responses")
	s := New(2, time.Minute)
	scope := Scope{"a", "r"}
	l := s.Begin(scope, u)
	if s.Commit(l, u, []*http.Cookie{{Name: "session", Value: "secret"}, {Name: "__cflb", Value: "bad", Domain: "evil.com"}, {Name: "__oailb", Value: strings.Repeat("x", 4097)}}) {
		t.Fatal("untrusted cookie accepted")
	}
	s.Commit(l, u, []*http.Cookie{{Name: "__cflb", Value: "expired", Expires: time.Now().Add(-time.Second)}})
	if len(s.Begin(scope, u).Cookies) != 0 {
		t.Fatal("expired cookie sent")
	}
	s.Begin(Scope{"b", "r"}, u)
	s.Begin(Scope{"c", "r"}, u)
	if s.Size() != 2 {
		t.Fatal("unbounded store")
	}
}
