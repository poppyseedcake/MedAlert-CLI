package medicover

import (
	"net/http"
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
