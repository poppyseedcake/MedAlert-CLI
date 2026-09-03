package medicover

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCookieDeletionRemovesStoredEntry(t *testing.T) {
	now := time.Now()
	store := &cookieStore{
		cookies: []StoredCookie{{Name: "MFA-Pending", Value: "1", Path: "/"}},
		now:     now,
	}
	response := &http.Response{Header: http.Header{}}
	response.Header.Add("Set-Cookie", "MFA-Pending=; Path=/; Max-Age=0")
	store.addFromResponse(response)
	if header := store.headerFor("http://example"); header != "" {
		t.Fatalf("deleted cookie was replayed: %q", header)
	}
	if persisted := store.persist(); len(persisted) != 0 {
		t.Fatalf("deleted cookie was persisted: %+v", persisted)
	}
}

func TestMaxAgeZeroDeletesCookieWithValue(t *testing.T) {
	now := time.Now()
	store := &cookieStore{
		cookies: []StoredCookie{{Name: "Session", Value: "keep", Path: "/"}},
		now:     now,
	}
	response := &http.Response{Header: http.Header{}}
	response.Header.Add("Set-Cookie", "Session=keep; Path=/; Max-Age=0")
	store.addFromResponse(response)
	if header := store.headerFor("https://login.example/"); header != "" {
		t.Fatalf("expired cookie was replayed: %q", header)
	}
}

func TestCookieDomainPathAndSecureMatching(t *testing.T) {
	now := time.Now()
	store := &cookieStore{
		cookies: []StoredCookie{
			{Name: "A", Value: "1", Domain: "login.example", Path: "/Account"},
			{Name: "B", Value: "2", Secure: true},
			{Name: "C", Value: "3", Path: "/"},
		},
		now: now,
	}
	if header := store.headerFor("https://login.example/Account/Login"); header == "" {
		t.Fatal("expected cookies for matching host/path")
	}
	header := store.headerFor("https://other.example/Account/Login")
	if containsCookie(header, "A") {
		t.Fatalf("cookie leaked to wrong host: %q", header)
	}
	header = store.headerFor("https://login.example/Other")
	if containsCookie(header, "A") {
		t.Fatalf("cookie leaked to wrong path: %q", header)
	}
	header = store.headerFor("http://login.example/")
	if containsCookie(header, "B") {
		t.Fatalf("secure cookie sent over http: %q", header)
	}
}

func containsCookie(header, name string) bool {
	for _, part := range splitCookieHeader(header) {
		if part == name {
			return true
		}
	}
	return false
}

func splitCookieHeader(header string) []string {
	var names []string
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, _, _ := strings.Cut(part, "=")
		names = append(names, strings.TrimSpace(name))
	}
	return names
}
