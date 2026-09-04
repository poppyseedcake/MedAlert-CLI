package medicover

import (
	"errors"
	"net/http"
	"net/url"
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
	if len(s.cookies) == 0 {
		return ""
	}
	target, err := parseRequestURL(requestURL)
	if err != nil {
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
		if stored.Secure && target.scheme != "https" && !isLocalHost(target.host) {
			continue
		}
		if !domainMatches(target.host, stored.Domain) {
			continue
		}
		if !pathMatches(target.path, stored.Path) {
			continue
		}
		parts = append(parts, stored.Name+"="+stored.Value)
	}
	return strings.Join(parts, "; ")
}

type requestTarget struct {
	scheme string
	host   string
	path   string
}

func parseRequestURL(raw string) (requestTarget, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return requestTarget{}, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return requestTarget{}, errors.New("request URL needs scheme and host")
	}
	path := parsed.Path
	if path == "" {
		path = "/"
	}
	return requestTarget{scheme: parsed.Scheme, host: parsed.Hostname(), path: path}, nil
}

func domainMatches(requestHost, cookieDomain string) bool {
	trimmed := strings.TrimSpace(cookieDomain)
	if trimmed == "" {
		return true
	}
	trimmed = strings.TrimPrefix(strings.ToLower(trimmed), ".")
	hostOnly := strings.Trim(strings.ToLower(requestHost), "[]")
	if hostOnly == trimmed {
		return true
	}
	return strings.HasSuffix(hostOnly, "."+trimmed)
}

func pathMatches(requestPath, cookiePath string) bool {
	trimmed := strings.TrimSpace(cookiePath)
	if trimmed == "" {
		return true
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	if requestPath == "" {
		requestPath = "/"
	}
	if requestPath == trimmed {
		return true
	}
	if !strings.HasPrefix(requestPath, trimmed) {
		return false
	}
	return strings.HasSuffix(trimmed, "/") || strings.HasPrefix(requestPath[len(trimmed):], "/")
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
			// A past expiry deletes the cookie even when the directive
			// carries a non-empty value (for example
			// `Session=keep; Max-Age=0`).
			if isExpiredCookie(*parsed, s.now) {
				s.deleteByName(parsed.Name)
				continue
			}
			s.upsert(*parsed)
		}
	}
}

func (s *cookieStore) deleteByName(name string) {
	kept := s.cookies[:0]
	for _, existing := range s.cookies {
		if existing.Name != name {
			kept = append(kept, existing)
		}
	}
	s.cookies = kept
}

func (s *cookieStore) upsert(cookie StoredCookie) {
	// An empty value is a deletion directive (for example
	// `MFA-Pending=; Max-Age=0`). Remove the stored entry so stale MFA or
	// trusted-session state is never replayed or persisted. Match by name
	// alone: a deletion must clear the cookie even when the directive omits
	// the original domain or path attributes.
	if strings.TrimSpace(cookie.Value) == "" {
		s.deleteByName(cookie.Name)
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

// HasUsableSessionCookies reports whether a saved session contains a trusted
// Medicover cookie that is usable for the default authorization endpoint.
func HasUsableSessionCookies(state *SessionState, now time.Time) bool {
	return HasUsableSessionCookiesForIssuer(state, "", now)
}

// HasUsableSessionCookiesForIssuer reports whether a saved session contains a
// trusted cookie that matches the authorization endpoint for issuer. It checks
// the cookie name, expiry, security, domain, and path. A structurally valid
// session with only unrelated cookies still requires login.
func HasUsableSessionCookiesForIssuer(state *SessionState, issuer string, now time.Time) bool {
	if state == nil {
		return false
	}
	issuer = strings.TrimSuffix(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		issuer = defaultIssuer
	}
	authorizeURL := issuer + "/connect/authorize"
	for _, cookie := range state.Cookies {
		if cookie.Name != "MedicoverTrusted" {
			continue
		}
		cookies := &cookieStore{cookies: []StoredCookie{cookie}, now: now}
		if cookies.headerFor(authorizeURL) != "" {
			return true
		}
	}
	return false
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
				// A non-positive Max-Age deletes the cookie even when the
				// directive carries a value. Normalize to an empty value
				// so upsert removes the entry regardless of clock skew
				// between the server, time.Now, and the injected test
				// clock.
				if *duration <= 0 {
					cookie.Value = ""
				}
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
