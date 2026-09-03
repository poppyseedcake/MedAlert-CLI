package medicover

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type authorizeOutcome struct {
	code   string
	state  string
	issuer string
}

type pageResult struct {
	body             string
	url              string
	isHTML           bool
	redirectedToCode bool
	code             string
	codeState        string
	codeIssuer       string
}

type formResponse struct {
	location string
	body     string
	isHTML   bool
	status   int
}

// followAuthorize performs one authorize request without following redirects
// automatically. A code in the redirect chain is returned; otherwise the
// caller continues with the login form.
func (c *Client) followAuthorize(ctx context.Context, authorizeURL string, cookies *cookieStore) (authorizeOutcome, error) {
	current := authorizeURL
	for redirect := 0; redirect < maxRedirects; redirect++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return authorizeOutcome{}, temporary("cannot build authorization request")
		}
		request.Header.Set("Accept", "text/html,application/xhtml+xml")
		if header := cookies.headerFor(current); header != "" {
			request.Header.Set("Cookie", header)
		}
		response, err := c.cfg.HTTPClient.Do(request)
		if err != nil {
			return authorizeOutcome{}, temporary("authorization endpoint is temporarily unavailable")
		}
		cookies.addFromResponse(response)
		location := response.Header.Get("Location")
		status := response.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		response.Body.Close()

		if status == http.StatusTooManyRequests {
			return authorizeOutcome{}, rateLimitedError(response)
		}
		if status >= 500 || status == http.StatusRequestTimeout {
			return authorizeOutcome{}, temporary("authorization endpoint is temporarily unavailable")
		}
		if isRedirect(status) && strings.TrimSpace(location) != "" {
			next := resolveURL(current, location)
			if code, state, issuer, ok := codeFromLocation(next, c.cfg.RedirectURI); ok {
				return authorizeOutcome{code: code, state: state, issuer: issuer}, nil
			}
			if err := c.checkRedirectAllowed(current, next); err != nil {
				return authorizeOutcome{}, err
			}
			current = next
			continue
		}
		// No redirect: the trusted session did not produce a code.
		return authorizeOutcome{}, nil
	}
	return authorizeOutcome{}, protocolChanged("too many authorization redirects")
}

func (c *Client) getPage(ctx context.Context, pageURL string, cookies *cookieStore) (*pageResult, error) {
	current := pageURL
	for redirect := 0; redirect < maxRedirects; redirect++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, temporary("cannot build page request")
		}
		request.Header.Set("Accept", "text/html,application/xhtml+xml")
		if header := cookies.headerFor(current); header != "" {
			request.Header.Set("Cookie", header)
		}
		response, err := c.cfg.HTTPClient.Do(request)
		if err != nil {
			return nil, temporary("Medicover page is temporarily unavailable")
		}
		cookies.addFromResponse(response)
		location := strings.TrimSpace(response.Header.Get("Location"))
		status := response.StatusCode
		if isRedirect(status) && location != "" {
			next := resolveURL(current, location)
			if code, state, issuer, ok := codeFromLocation(next, c.cfg.RedirectURI); ok {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
				response.Body.Close()
				return &pageResult{url: current, redirectedToCode: true, code: code, codeState: state, codeIssuer: issuer}, nil
			}
			if err := c.checkRedirectAllowed(current, next); err != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
				response.Body.Close()
				return nil, err
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
			response.Body.Close()
			current = next
			continue
		}
		if status == http.StatusTooManyRequests {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
			response.Body.Close()
			return nil, rateLimitedError(response)
		}
		if status < 200 || status >= 300 {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
			response.Body.Close()
			if status >= 500 || status == http.StatusRequestTimeout {
				return nil, temporary("Medicover page is temporarily unavailable")
			}
			return nil, protocolChanged("unexpected page response")
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 512*1024))
		response.Body.Close()
		if err != nil {
			return nil, temporary("cannot read Medicover page")
		}
		contentType := strings.ToLower(response.Header.Get("Content-Type"))
		isHTML := strings.Contains(contentType, "text/html") || looksLikeHTML(string(body))
		return &pageResult{body: string(body), url: current, isHTML: isHTML}, nil
	}
	return nil, protocolChanged("too many page redirects")
}

func (c *Client) postForm(ctx context.Context, action string, values url.Values, cookies *cookieStore, referer string) (*formResponse, error) {
	if strings.TrimSpace(action) == "" {
		return nil, protocolChanged("missing form action")
	}
	// Resolve relative actions against the page that served the form, then
	// require the target host to be an approved origin. This prevents a
	// server-supplied action from sending credentials to an untrusted host.
	resolvedAction := action
	if strings.TrimSpace(referer) != "" {
		resolvedAction = resolveURL(referer, action)
	}
	parsed, err := url.Parse(resolvedAction)
	if err != nil {
		return nil, protocolChanged("invalid form action")
	}
	if parsed.Scheme != "" && parsed.Scheme != "https" && !isLocalHost(parsed.Host) {
		return nil, protocolChanged("insecure form action")
	}
	baseForActionCheck := referer
	if strings.TrimSpace(baseForActionCheck) == "" {
		baseForActionCheck = c.cfg.Issuer
	}
	if err := c.checkRedirectAllowed(baseForActionCheck, resolvedAction); err != nil {
		return nil, err
	}
	action = resolvedAction
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, temporary("cannot build form request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	if strings.TrimSpace(referer) != "" {
		request.Header.Set("Referer", referer)
	}
	if header := cookies.headerFor(action); header != "" {
		request.Header.Set("Cookie", header)
	}
	response, err := c.cfg.HTTPClient.Do(request)
	if err != nil {
		return nil, temporary("Medicover form is temporarily unavailable")
	}
	defer response.Body.Close()
	cookies.addFromResponse(response)
	location := strings.TrimSpace(response.Header.Get("Location"))
	status := response.StatusCode
	if isRedirect(status) {
		if location == "" {
			return nil, protocolChanged("missing redirect location")
		}
		next := resolveURL(action, location)
		// Validate every redirect target before acting on it. The MFA
		// check alone is not sufficient: an untrusted URL containing
		// "/mfa" must never receive cookies or the MFA code.
		if err := c.checkRedirectAllowed(action, next); err != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
			return nil, err
		}
		// The callback URL carries the code; MFA and login redirects are
		// validated by the caller.
		if _, _, _, ok := codeFromLocation(next, c.cfg.RedirectURI); ok {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
			return &formResponse{location: next, status: status}, nil
		}
		if isMFARedirect(next) {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
			return &formResponse{location: next, status: status}, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
		return &formResponse{location: next, status: status}, nil
	}
	if status == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
		return nil, rateLimitedError(response)
	}
	// Never expose form response bodies from login or MFA endpoints in
	// errors; the caller inspects only the presence of known fields.
	body, err := io.ReadAll(io.LimitReader(response.Body, 512*1024))
	if err != nil {
		return nil, temporary("cannot read form response")
	}
	if status >= 500 || status == http.StatusRequestTimeout {
		return nil, temporary("Medicover form is temporarily unavailable")
	}
	if status < 200 || status >= 300 {
		return nil, protocolChanged("unexpected form response")
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	isHTML := strings.Contains(contentType, "text/html") || looksLikeHTML(string(body))
	return &formResponse{body: string(body), isHTML: isHTML, status: status}, nil
}

func (c *Client) checkRedirectAllowed(current, next string) error {
	currentURL, err := url.Parse(current)
	if err != nil {
		return protocolChanged("invalid redirect")
	}
	nextURL, err := url.Parse(next)
	if err != nil {
		return protocolChanged("invalid redirect")
	}
	if nextURL.Scheme != "https" && !isLocalHost(nextURL.Host) {
		return protocolChanged("insecure redirect")
	}
	callbackURL, _ := url.Parse(c.cfg.RedirectURI)
	issuerURL, _ := url.Parse(c.cfg.Issuer)
	allowed := map[string]bool{}
	if callbackURL != nil && callbackURL.Host != "" {
		allowed[strings.ToLower(callbackURL.Host)] = true
	}
	if issuerURL != nil && issuerURL.Host != "" {
		allowed[strings.ToLower(issuerURL.Host)] = true
	}
	if currentURL.Host != "" {
		allowed[strings.ToLower(currentURL.Host)] = true
	}
	// Local test hosts are always allowed to redirect to each other.
	if isLocalHost(nextURL.Host) {
		return nil
	}
	if allowed[strings.ToLower(nextURL.Host)] {
		return nil
	}
	return protocolChanged(fmt.Sprintf("unexpected redirect host"))
}

func isRedirect(status int) bool {
	return status == http.StatusMovedPermanently ||
		status == http.StatusFound ||
		status == http.StatusSeeOther ||
		status == http.StatusTemporaryRedirect ||
		status == http.StatusPermanentRedirect
}

func looksLikeHTML(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "<html") || strings.Contains(lowered, "<form")
}

// codeFromLocation extracts an OIDC code when location points at the
// registered callback. Query and fragment are both accepted because the
// server declares query response mode but tests may use either. The scheme,
// host (including port), and path must match the registered callback
// exactly; a matching path on another host is not sufficient.
func codeFromLocation(location, redirectURI string) (code, state, issuer string, ok bool) {
	if strings.TrimSpace(location) == "" {
		return "", "", "", false
	}
	locationURL, err := url.Parse(location)
	if err != nil {
		return "", "", "", false
	}
	callbackURL, err := url.Parse(redirectURI)
	if err != nil {
		return "", "", "", false
	}
	if !strings.EqualFold(locationURL.Scheme, callbackURL.Scheme) ||
		!strings.EqualFold(locationURL.Host, callbackURL.Host) ||
		locationURL.Path != callbackURL.Path {
		return "", "", "", false
	}
	values := locationURL.Query()
	if values.Get("code") == "" && locationURL.Fragment != "" {
		if fragmentValues, err := url.ParseQuery(locationURL.Fragment); err == nil {
			values = fragmentValues
		}
	}
	code = values.Get("code")
	if code == "" {
		return "", "", "", false
	}
	return code, values.Get("state"), values.Get("iss"), true
}

func isMFARedirect(location string) bool {
	if strings.TrimSpace(location) == "" {
		return false
	}
	return strings.Contains(location, "/Mfa") || strings.Contains(strings.ToLower(location), "/mfa")
}
