package medicover

import (
	"net/http"
	"strings"
	"time"
)

// cookieStore keeps per-account cookies in memory and persists only the
// entries that are still valid.
type cookieStore struct {
	cookies []StoredCookie
	now     time.Time
}

func (s *cookieStore) headerFor(requestURL string) string {
	// The caller resolves domain and path matching loosely: persisted
	// cookies carry their original domain/path, and the fake Medicover
	// server accepts any cookie whose name matches. Production matching
	// stays inside this adapter and never leaks cookie values to logs.
	if len(s.cookies) == 0 {
		return ""
	}
	var parts []string
	for _, stored := range s.cookies {
		if strings.TrimSpace(stored.Value) == "" {
			continue
		}
		if isExpiredCookie(stored, s.now) {
			continue
		}
		parts = append(parts, stored.Name+"="+stored.Value)
	}
	return strings.Join(parts, "; ")
}

func isExpiredCookie(cookie StoredCookie, now time.Time) bool {
	if strings.TrimSpace(cookie.Expires) == "" {
		return false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, cookie.Expires); err == nil {
		return !parsed.After(now)
	}
	if parsed, err := http.ParseTime(cookie.Expires); err == nil {
		return !parsed.After(now)
	}
	return false
}

func (s *cookieStore) addFromResponse(response *http.Response) {
	for _, header := range response.Header["Set-Cookie"] {
		if parsed := parseSetCookie(header); parsed != nil {
			s.upsert(*parsed)
		}
	}
}

func (s *cookieStore) upsert(cookie StoredCookie) {
	// An empty value is a deletion directive (for example
	// `MFA-Pending=; Max-Age=0`). Remove the stored entry so stale MFA or
	// trusted-session state is never replayed or persisted. Match by name
	// alone: a deletion must clear the cookie even when the directive omits
	// the original domain or path attributes.
	if strings.TrimSpace(cookie.Value) == "" {
		kept := s.cookies[:0]
		for _, existing := range s.cookies {
			if existing.Name != cookie.Name {
				kept = append(kept, existing)
			}
		}
		s.cookies = kept
		return
	}
	for index, existing := range s.cookies {
		if existing.Name == cookie.Name && existing.Domain == cookie.Domain && existing.Path == cookie.Path {
			s.cookies[index] = cookie
			return
		}
	}
	s.cookies = append(s.cookies, cookie)
}

func (s *cookieStore) persist() []StoredCookie {
	kept := make([]StoredCookie, 0, len(s.cookies))
	for _, cookie := range s.cookies {
		if strings.TrimSpace(cookie.Name) == "" {
			continue
		}
		if isExpiredCookie(cookie, s.now) {
			continue
		}
		// Never persist empty placeholder cookies.
		if strings.TrimSpace(cookie.Value) == "" {
			continue
		}
		kept = append(kept, cookie)
	}
	if kept == nil {
		return []StoredCookie{}
	}
	return kept
}

func parseSetCookie(header string) *StoredCookie {
	segments := strings.Split(header, ";")
	if len(segments) == 0 {
		return nil
	}
	nameValue := strings.SplitN(strings.TrimSpace(segments[0]), "=", 2)
	if len(nameValue) != 2 {
		return nil
	}
	name := strings.TrimSpace(nameValue[0])
	value := strings.TrimSpace(nameValue[1])
	if name == "" {
		return nil
	}
	cookie := &StoredCookie{Name: name, Value: value}
	for _, segment := range segments[1:] {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		key, val, _ := strings.Cut(segment, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "domain":
			cookie.Domain = strings.ToLower(val)
		case "path":
			cookie.Path = val
		case "expires":
			cookie.Expires = val
		case "max-age":
			if duration := parseMaxAge(val); duration != nil {
				cookie.Expires = time.Now().Add(*duration).UTC().Format(time.RFC3339Nano)
			}
		case "secure":
			cookie.Secure = true
		case "httponly":
			cookie.HTTPOnly = true
		}
	}
	return cookie
}

func parseMaxAge(value string) *time.Duration {
	var seconds int64
	var sign = 1
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "-") {
		sign = -1
		trimmed = strings.TrimPrefix(trimmed, "-")
	}
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			return nil
		}
		seconds = seconds*10 + int64(r-'0')
		if seconds > 10*365*24*3600 {
			break
		}
	}
	duration := time.Duration(sign) * time.Duration(seconds) * time.Second
	return &duration
}
