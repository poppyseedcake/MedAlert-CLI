// Package medicover owns Medicover OIDC, PKCE, password, MFA, trusted-device
// state, cookies, session renewal, and safe typed errors.
//
// Callers never see URLs, HTML field names, cookies, redirect paths, bearer
// headers, transport JSON, or application-version parameters. All
// Medicover-specific HTTP behavior stays inside this module.
package medicover

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// Production endpoints. Tests override them through Config.
	defaultIssuer      = "https://login-online24.medicover.pl"
	defaultClientID    = "web"
	defaultRedirectURI = "https://online24.medicover.pl/signin-oidc"
	defaultScope       = "openid offline_access profile"
	defaultAPIBaseURL  = "https://api-gateway-online24.medicover.pl"
	// currentAppVersion is the Medicover web application version observed in
	// research. It lives only inside this adapter and is never part of the
	// domain model.
	currentAppVersion = "3.37.0-beta.1.6"

	maxRedirects      = 10
	requestTimeout    = 15 * time.Second
	renewBeforeExpiry = 30 * time.Second
)

// Error codes returned across the seam. HTTP, SQL, and secret details never
// cross the seam.
const (
	CodeAuthRequired       = "authentication_required"
	CodeProtocolChanged    = "protocol_changed"
	CodeInvalidCredentials = "invalid_credentials"
	CodeTemporary          = "temporary_failure"
	CodeRateLimited        = "rate_limited"
	CodeMFARequired        = "mfa_required"
	CodeCancelled          = "cancelled"
	CodePartial            = "partial_result"
	CodeConflicting        = "conflicting_result"
	CodeStale              = "stale_result"
)

// Error is the only error type that crosses the Medicover seam. Message is
// always sanitized: it never contains passwords, MFA codes, tokens, cookies,
// authorization headers, login form values, or sensitive query parameters.
type Error struct {
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func IsAuthRequired(err error) bool {
	return errorCode(err) == CodeAuthRequired || errorCode(err) == CodeMFARequired
}

func IsProtocolChanged(err error) bool { return errorCode(err) == CodeProtocolChanged }

func IsInvalidCredentials(err error) bool { return errorCode(err) == CodeInvalidCredentials }

func IsTemporary(err error) bool {
	code := errorCode(err)
	return code == CodeTemporary || code == CodeRateLimited
}

func errorCode(err error) string {
	var medicoverErr *Error
	if errors.As(err, &medicoverErr) {
		return medicoverErr.Code
	}
	return ""
}

func authRequired(msg string) *Error {
	return &Error{Code: CodeAuthRequired, Message: msg}
}

func protocolChanged(msg string) *Error {
	return &Error{Code: CodeProtocolChanged, Message: msg}
}

func invalidCredentials(msg string) *Error {
	return &Error{Code: CodeInvalidCredentials, Message: msg}
}

func temporary(msg string) *Error {
	return &Error{Code: CodeTemporary, Message: msg}
}

// Secret hides a value in normal formatting. Use Expose only at the exact
// transport boundary that needs the raw value.
type Secret string

func (s Secret) String() string   { return "[REDACTED]" }
func (s Secret) GoString() string { return "[REDACTED]" }
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}

// Expose returns the raw secret value. Callers must never log or return it.
func (s Secret) Expose() string { return string(s) }

// StoredCookie is one persisted cookie. Expiry is respected: expired cookies
// are never sent and are dropped on save.
type StoredCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Expires  string `json:"expires,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"http_only,omitempty"`
}

// SessionState is the persisted per-account session. It contains cookies, a
// refresh token, and trusted-device data. It never contains the short-lived
// access token.
type SessionState struct {
	DeviceID     string         `json:"device_id"`
	Cookies      []StoredCookie `json:"cookies,omitempty"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	UpdatedAt    string         `json:"updated_at,omitempty"`
}

// Tokens is an in-memory authentication result. The access token is never
// persisted.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// AuthRequest carries one authentication attempt. Session may be nil. MFACode
// is empty when no code is available.
type AuthRequest struct {
	Username Secret
	Password Secret
	MFACode  Secret
	// RequestMFA is used only after the server returns an MFA form.
	// It continues that challenge without another password request.
	RequestMFA func(context.Context) (Secret, error)
	Session    *SessionState
}

// AuthResult reports a successful authentication. Session must be saved by
// the caller. AccessToken is in-memory only.
type AuthResult struct {
	AccessToken string
	ExpiresAt   time.Time
	Session     *SessionState
	Reused      bool
	MFAUsed     bool
}

// Config selects endpoints and seams. Empty AuthorizeEndpoint and
// TokenEndpoint are discovered from Issuer. HTTPClient, Clock, and Rand are
// private test seams.
type Config struct {
	Issuer            string
	AuthorizeEndpoint string
	TokenEndpoint     string
	ClientID          string
	RedirectURI       string
	HTTPClient        *http.Client
	Clock             func() time.Time
	Rand              io.Reader
	APIBaseURL        string
}

func (c *Config) withDefaults() Config {
	cfg := *c
	if cfg.Issuer == "" {
		cfg.Issuer = defaultIssuer
	}
	if cfg.ClientID == "" {
		cfg.ClientID = defaultClientID
	}
	if cfg.RedirectURI == "" {
		cfg.RedirectURI = defaultRedirectURI
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = defaultAPIBaseURL
	}
	return cfg
}

// Client performs Medicover authentication. It holds no per-account state;
// session state is passed in and returned for the caller to persist.
type Client struct {
	cfg Config
}

func NewClient(cfg Config) *Client {
	withDefaults := cfg.withDefaults()
	// Never follow redirects automatically: the flow must validate state,
	// issuer, callback host, and redirect count itself.
	transport := withDefaults.HTTPClient
	if transport.CheckRedirect == nil {
		clone := *transport
		clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
		withDefaults.HTTPClient = &clone
	}
	return &Client{cfg: withDefaults}
}

// discoveryDocument is the minimal OIDC metadata we need.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// Discover reads OIDC endpoints from the issuer and validates their hosts.
func (c *Client) Discover(ctx context.Context) (authorize, token string, err error) {
	if c.cfg.AuthorizeEndpoint != "" && c.cfg.TokenEndpoint != "" {
		return c.cfg.AuthorizeEndpoint, c.cfg.TokenEndpoint, nil
	}
	issuerURL, err := url.Parse(c.cfg.Issuer)
	if err != nil {
		return "", "", protocolChanged("invalid OIDC issuer")
	}
	discoveryURL := strings.TrimSuffix(c.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", "", temporary("cannot build OIDC discovery request")
	}
	response, err := c.cfg.HTTPClient.Do(request)
	if err != nil {
		return "", "", temporary("OIDC discovery is temporarily unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return "", "", rateLimitedError(response)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", temporary("OIDC discovery is temporarily unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return "", "", temporary("cannot read OIDC discovery response")
	}
	var document discoveryDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return "", "", protocolChanged("invalid OIDC discovery response")
	}
	if document.AuthorizationEndpoint == "" || document.TokenEndpoint == "" {
		return "", "", protocolChanged("incomplete OIDC discovery response")
	}
	if err := validateDiscoveryHosts(issuerURL.Host, document); err != nil {
		return "", "", err
	}
	return document.AuthorizationEndpoint, document.TokenEndpoint, nil
}

func validateDiscoveryHosts(issuerHost string, document discoveryDocument) error {
	for _, endpoint := range []string{document.AuthorizationEndpoint, document.TokenEndpoint} {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return protocolChanged("invalid OIDC endpoint")
		}
		if parsed.Scheme != "https" && !isLocalHost(parsed.Host) {
			return protocolChanged("insecure OIDC endpoint")
		}
		// Endpoints must live on the issuer host or a local test host.
		// Production keeps them on the same host; tests use 127.0.0.1.
		if !equalHost(parsed.Host, issuerHost) && !isLocalHost(parsed.Host) {
			return protocolChanged("unexpected OIDC endpoint host")
		}
	}
	return nil
}

func isLocalHost(hostport string) bool {
	host, _, _ := strings.Cut(hostport, ":")
	host = strings.ToLower(strings.Trim(host, "[]"))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func equalHost(left, right string) bool {
	leftHost, _, _ := strings.Cut(left, ":")
	rightHost, _, _ := strings.Cut(right, ":")
	return strings.EqualFold(leftHost, rightHost)
}

// NewPKCE builds a verifier and its S256 challenge.
func NewPKCE(random io.Reader) (verifier, challenge string, err error) {
	raw := make([]byte, 64)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// NewState builds a cryptographically random OIDC state value.
func NewState(random io.Reader) (string, error) {
	raw := make([]byte, 24)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// NewDeviceID builds a random device identifier (UUID v4 text).
func NewDeviceID(random io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// Authenticate performs one authentication attempt. It first tries a trusted
// session reuse and falls back to a full password (and MFA) login.
func (c *Client) Authenticate(ctx context.Context, req AuthRequest) (AuthResult, error) {
	session := req.Session
	if session == nil {
		session = &SessionState{}
	} else {
		clone := *session
		session = &clone
	}
	if strings.TrimSpace(session.DeviceID) == "" {
		deviceID, err := NewDeviceID(c.cfg.Rand)
		if err != nil {
			return AuthResult{}, temporary("cannot prepare authentication")
		}
		session.DeviceID = deviceID
	}
	authorizeEndpoint, tokenEndpoint, err := c.Discover(ctx)
	if err != nil {
		return AuthResult{}, err
	}

	// Attempt trusted reuse before asking for credentials.
	if len(session.Cookies) > 0 || strings.TrimSpace(session.RefreshToken) != "" {
		if result, reuseErr := c.tryReuse(ctx, authorizeEndpoint, tokenEndpoint, session); reuseErr == nil {
			return result, nil
		} else {
			var medicoverErr *Error
			// Only fall through to a full login for expected reuse misses.
			// Protocol, rate-limit, and temporary failures are returned.
			if errors.As(reuseErr, &medicoverErr) {
				switch medicoverErr.Code {
				case CodeAuthRequired, CodeInvalidCredentials, CodeMFARequired:
					// Fall through to password login.
				default:
					return AuthResult{}, reuseErr
				}
			} else {
				return AuthResult{}, reuseErr
			}
		}
	}

	if strings.TrimSpace(req.Username.Expose()) == "" || strings.TrimSpace(req.Password.Expose()) == "" {
		return AuthResult{}, authRequired("authentication is required")
	}
	return c.fullLogin(ctx, authorizeEndpoint, tokenEndpoint, req, session)
}

// tryReuse performs an authorize request with saved cookies. A returned code
// is exchanged without touching the password.
func (c *Client) tryReuse(ctx context.Context, authorizeEndpoint, tokenEndpoint string, session *SessionState) (AuthResult, error) {
	state, err := NewState(c.cfg.Rand)
	if err != nil {
		return AuthResult{}, temporary("cannot prepare authentication")
	}
	verifier, challenge, err := NewPKCE(c.cfg.Rand)
	if err != nil {
		return AuthResult{}, temporary("cannot prepare authentication")
	}
	authorizeURL := buildAuthorizeURL(authorizeEndpoint, c.cfg, session.DeviceID, state, challenge)
	cookies := &cookieStore{cookies: session.Cookies, now: c.cfg.Clock()}
	outcome, err := c.followAuthorize(ctx, authorizeURL, cookies)
	if err != nil {
		return AuthResult{}, err
	}
	if outcome.code == "" {
		return AuthResult{}, authRequired("trusted session is not valid")
	}
	if err := validateState(outcome.state, state); err != nil {
		return AuthResult{}, err
	}
	if err := validateIssuerParam(outcome.issuer, c.cfg.Issuer); err != nil {
		return AuthResult{}, err
	}
	tokens, err := c.exchangeCode(ctx, tokenEndpoint, outcome.code, verifier)
	if err != nil {
		return AuthResult{}, err
	}
	newSession := &SessionState{
		DeviceID:     session.DeviceID,
		Cookies:      cookies.persist(),
		RefreshToken: tokens.RefreshToken,
		UpdatedAt:    c.cfg.Clock().UTC().Format(time.RFC3339Nano),
	}
	if strings.TrimSpace(newSession.RefreshToken) == "" {
		newSession.RefreshToken = strings.TrimSpace(session.RefreshToken)
	}
	return AuthResult{
		AccessToken: tokens.AccessToken,
		ExpiresAt:   tokens.ExpiresAt,
		Session:     newSession,
		Reused:      true,
	}, nil
}

// fullLogin runs the password (and MFA) flow.
func (c *Client) fullLogin(ctx context.Context, authorizeEndpoint, tokenEndpoint string, req AuthRequest, session *SessionState) (AuthResult, error) {
	state, err := NewState(c.cfg.Rand)
	if err != nil {
		return AuthResult{}, temporary("cannot prepare authentication")
	}
	verifier, challenge, err := NewPKCE(c.cfg.Rand)
	if err != nil {
		return AuthResult{}, temporary("cannot prepare authentication")
	}
	authorizeURL := buildAuthorizeURL(authorizeEndpoint, c.cfg, session.DeviceID, state, challenge)
	cookies := &cookieStore{cookies: session.Cookies, now: c.cfg.Clock()}

	// If the first authorize already returns a code, the device is trusted.
	if outcome, err := c.followAuthorize(ctx, authorizeURL, cookies); err != nil {
		var medicoverErr *Error
		if errors.As(err, &medicoverErr) {
			switch medicoverErr.Code {
			case CodeTemporary, CodeRateLimited, CodeProtocolChanged:
				return AuthResult{}, err
			}
		} else {
			return AuthResult{}, err
		}
		// AuthRequired misses fall through to the password form.
	} else if outcome.code != "" {
		if err := validateState(outcome.state, state); err != nil {
			return AuthResult{}, err
		}
		if err := validateIssuerParam(outcome.issuer, c.cfg.Issuer); err != nil {
			return AuthResult{}, err
		}
		tokens, err := c.exchangeCode(ctx, tokenEndpoint, outcome.code, verifier)
		if err != nil {
			return AuthResult{}, err
		}
		return AuthResult{
			AccessToken: tokens.AccessToken,
			ExpiresAt:   tokens.ExpiresAt,
			Session: &SessionState{
				DeviceID:     session.DeviceID,
				Cookies:      cookies.persist(),
				RefreshToken: firstNonEmpty(tokens.RefreshToken, strings.TrimSpace(session.RefreshToken)),
				UpdatedAt:    c.cfg.Clock().UTC().Format(time.RFC3339Nano),
			},
			Reused: true,
		}, nil
	}

	loginPage, err := c.getPage(ctx, authorizeURL, cookies)
	if err != nil {
		return AuthResult{}, err
	}
	loginForm, err := parseLoginForm(loginPage.body, loginPage.url)
	if err != nil {
		return AuthResult{}, err
	}
	loginResponse, err := c.postForm(ctx, loginForm.action, loginForm.valuesWithCredentials(req.Username.Expose(), req.Password.Expose()), cookies, loginPage.url)
	if err != nil {
		return AuthResult{}, err
	}
	// A redirect to the callback carries the code.
	if code, returnedState, issuer, ok := codeFromLocation(loginResponse.location, c.cfg.RedirectURI); ok {
		if err := validateState(returnedState, state); err != nil {
			return AuthResult{}, err
		}
		if err := validateIssuerParam(issuer, c.cfg.Issuer); err != nil {
			return AuthResult{}, err
		}
		tokens, err := c.exchangeCode(ctx, tokenEndpoint, code, verifier)
		if err != nil {
			return AuthResult{}, err
		}
		return AuthResult{
			AccessToken: tokens.AccessToken,
			ExpiresAt:   tokens.ExpiresAt,
			Session: &SessionState{
				DeviceID:     session.DeviceID,
				Cookies:      cookies.persist(),
				RefreshToken: firstNonEmpty(tokens.RefreshToken, strings.TrimSpace(session.RefreshToken)),
				UpdatedAt:    c.cfg.Clock().UTC().Format(time.RFC3339Nano),
			},
		}, nil
	}
	// A redirect to an MFA page continues the flow.
	if isMFARedirect(loginResponse.location) {
		return c.handleMFA(ctx, tokenEndpoint, loginResponse, cookies, session, state, verifier, req.MFACode.Expose(), req.RequestMFA)
	}
	// Medicover can intermediate through /connect/authorize/callback before
	// reaching the registered callback. Follow one allowed redirect chain
	// instead of failing a valid login as a protocol change.
	if strings.TrimSpace(loginResponse.location) != "" && !loginResponse.isHTML {
		if result, followed, err := c.followPostRedirect(ctx, tokenEndpoint, loginResponse.location, cookies, session, state, verifier); err != nil {
			return AuthResult{}, err
		} else if followed {
			return result, nil
		}
	}
	// A repeated login form means the credentials were rejected. Any other
	// page shape is a protocol change, never an invalid-password error.
	if loginResponse.isHTML {
		if hasPasswordField(loginResponse.body) {
			return AuthResult{}, invalidCredentials("invalid username or password")
		}
		return AuthResult{}, protocolChanged("unexpected login response")
	}
	return AuthResult{}, protocolChanged("unexpected login response")
}

// followPostRedirect GETs an intermediate redirect left by a login or MFA
// POST (for example /connect/authorize/callback) and exchanges the code when
// the chain reaches the registered callback.
func (c *Client) followPostRedirect(ctx context.Context, tokenEndpoint, location string, cookies *cookieStore, session *SessionState, state, verifier string) (AuthResult, bool, error) {
	page, err := c.getPage(ctx, location, cookies)
	if err != nil {
		return AuthResult{}, false, err
	}
	if page.redirectedToCode {
		if err := validateState(page.codeState, state); err != nil {
			return AuthResult{}, false, err
		}
		if err := validateIssuerParam(page.codeIssuer, c.cfg.Issuer); err != nil {
			return AuthResult{}, false, err
		}
		tokens, err := c.exchangeCode(ctx, tokenEndpoint, page.code, verifier)
		if err != nil {
			return AuthResult{}, false, err
		}
		return AuthResult{
			AccessToken: tokens.AccessToken,
			ExpiresAt:   tokens.ExpiresAt,
			Session: &SessionState{
				DeviceID:     session.DeviceID,
				Cookies:      cookies.persist(),
				RefreshToken: firstNonEmpty(tokens.RefreshToken, strings.TrimSpace(session.RefreshToken)),
				UpdatedAt:    c.cfg.Clock().UTC().Format(time.RFC3339Nano),
			},
		}, true, nil
	}
	if page.isHTML && hasPasswordField(page.body) {
		return AuthResult{}, false, invalidCredentials("invalid username or password")
	}
	return AuthResult{}, false, nil
}

func (c *Client) handleMFA(ctx context.Context, tokenEndpoint string, loginResponse *formResponse, cookies *cookieStore, session *SessionState, state, verifier, mfaCode string, requestMFA func(context.Context) (Secret, error)) (AuthResult, error) {
	mfaURL := loginResponse.location
	if mfaURL == "" {
		return AuthResult{}, protocolChanged("missing MFA location")
	}
	mfaPage, err := c.getPage(ctx, mfaURL, cookies)
	if err != nil {
		return AuthResult{}, err
	}
	// A redirect from the MFA page means the device is already trusted.
	if mfaPage.redirectedToCode {
		if err := validateState(mfaPage.codeState, state); err != nil {
			return AuthResult{}, err
		}
		if err := validateIssuerParam(mfaPage.codeIssuer, c.cfg.Issuer); err != nil {
			return AuthResult{}, err
		}
		tokens, err := c.exchangeCode(ctx, tokenEndpoint, mfaPage.code, verifier)
		if err != nil {
			return AuthResult{}, err
		}
		return AuthResult{
			AccessToken: tokens.AccessToken,
			ExpiresAt:   tokens.ExpiresAt,
			Session: &SessionState{
				DeviceID:     session.DeviceID,
				Cookies:      cookies.persist(),
				RefreshToken: firstNonEmpty(tokens.RefreshToken, strings.TrimSpace(session.RefreshToken)),
				UpdatedAt:    c.cfg.Clock().UTC().Format(time.RFC3339Nano),
			},
			MFAUsed: false,
			Reused:  true,
		}, nil
	}
	if !mfaPage.isHTML {
		return AuthResult{}, protocolChanged("unexpected MFA response")
	}
	// An expired intermediate session can return the password form instead
	// of an MFA form. That is an authentication failure, never a protocol
	// change.
	if hasPasswordField(mfaPage.body) && !hasMFAField(mfaPage.body) {
		return AuthResult{}, authRequired("authentication is required")
	}
	mfaForm, err := parseMFAForm(mfaPage.body, mfaPage.url)
	if err != nil {
		return AuthResult{}, err
	}
	if strings.TrimSpace(mfaCode) == "" && requestMFA != nil {
		value, promptErr := requestMFA(ctx)
		if promptErr != nil {
			return AuthResult{}, &Error{Code: CodeCancelled, Message: "authentication was cancelled"}
		}
		mfaCode = value.Expose()
	}
	if strings.TrimSpace(mfaCode) == "" {
		return AuthResult{}, &Error{Code: CodeMFARequired, Message: "multi-factor authentication is required"}
	}
	if !isMFACodeFormat(mfaCode) {
		return AuthResult{}, invalidCredentials("invalid multi-factor code")
	}
	submitResponse, err := c.postForm(ctx, mfaForm.action, mfaForm.valuesWithCode(mfaCode), cookies, mfaPage.url)
	if err != nil {
		return AuthResult{}, err
	}
	if code, returnedState, issuer, ok := codeFromLocation(submitResponse.location, c.cfg.RedirectURI); ok {
		if err := validateState(returnedState, state); err != nil {
			return AuthResult{}, err
		}
		if err := validateIssuerParam(issuer, c.cfg.Issuer); err != nil {
			return AuthResult{}, err
		}
		tokens, err := c.exchangeCode(ctx, tokenEndpoint, code, verifier)
		if err != nil {
			return AuthResult{}, err
		}
		return AuthResult{
			AccessToken: tokens.AccessToken,
			ExpiresAt:   tokens.ExpiresAt,
			Session: &SessionState{
				DeviceID:     session.DeviceID,
				Cookies:      cookies.persist(),
				RefreshToken: firstNonEmpty(tokens.RefreshToken, strings.TrimSpace(session.RefreshToken)),
				UpdatedAt:    c.cfg.Clock().UTC().Format(time.RFC3339Nano),
			},
			MFAUsed: true,
		}, nil
	}
	if strings.TrimSpace(submitResponse.location) != "" && !submitResponse.isHTML {
		if result, followed, err := c.followPostRedirect(ctx, tokenEndpoint, submitResponse.location, cookies, session, state, verifier); err != nil {
			return AuthResult{}, err
		} else if followed {
			result.MFAUsed = true
			return result, nil
		}
	}
	if submitResponse.isHTML {
		if hasMFAField(submitResponse.body) || hasPasswordField(submitResponse.body) {
			return AuthResult{}, invalidCredentials("invalid multi-factor code")
		}
		return AuthResult{}, protocolChanged("unexpected MFA response")
	}
	return AuthResult{}, protocolChanged("unexpected MFA response")
}

func isMFACodeFormat(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func buildAuthorizeURL(authorizeEndpoint string, cfg Config, deviceID, state, challenge string) string {
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", cfg.ClientID)
	values.Set("redirect_uri", cfg.RedirectURI)
	values.Set("scope", defaultScope)
	values.Set("state", state)
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")
	values.Set("response_mode", "query")
	// Medicover extensions stay inside this adapter. They are not required
	// to open the login page, but a stable device id supports trusted-device
	// reuse when the server honors it.
	values.Set("device_id", deviceID)
	values.Set("device_name", "Chrome")
	values.Set("app_version", currentAppVersion)
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	values.Set("ts", fmt.Sprintf("%d", clock().UnixMilli()))
	separator := "?"
	if strings.Contains(authorizeEndpoint, "?") {
		separator = "&"
	}
	return authorizeEndpoint + separator + values.Encode()
}

func validateState(returned, sent string) error {
	if returned == "" || sent == "" {
		return authRequired("invalid OIDC state")
	}
	if subtle.ConstantTimeCompare([]byte(returned), []byte(sent)) != 1 {
		return authRequired("invalid OIDC state")
	}
	return nil
}

func validateIssuerParam(returned, expectedIssuer string) error {
	if strings.TrimSpace(returned) == "" {
		return nil
	}
	expected, err := url.Parse(expectedIssuer)
	if err != nil {
		return protocolChanged("invalid OIDC issuer")
	}
	got, err := url.Parse(returned)
	if err != nil {
		return authRequired("invalid OIDC issuer")
	}
	if !strings.EqualFold(got.Scheme+"://"+got.Host, expected.Scheme+"://"+expected.Host) && !strings.EqualFold(returned, expectedIssuer) {
		return authRequired("invalid OIDC issuer")
	}
	return nil
}

// Refresh exchanges a refresh token for a new access token. It performs at
// most one HTTP call and never prompts.
func (c *Client) Refresh(ctx context.Context, session *SessionState) (Tokens, *SessionState, error) {
	if session == nil || strings.TrimSpace(session.RefreshToken) == "" {
		return Tokens{}, nil, authRequired("authentication is required")
	}
	_, tokenEndpoint, err := c.Discover(ctx)
	if err != nil {
		return Tokens{}, nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", session.RefreshToken)
	form.Set("client_id", c.cfg.ClientID)
	tokens, err := c.postToken(ctx, tokenEndpoint, form)
	if err != nil {
		return Tokens{}, nil, err
	}
	clone := *session
	clone.Cookies = (&cookieStore{cookies: session.Cookies, now: c.cfg.Clock()}).persist()
	if strings.TrimSpace(tokens.RefreshToken) != "" {
		clone.RefreshToken = tokens.RefreshToken
	}
	clone.UpdatedAt = c.cfg.Clock().UTC().Format(time.RFC3339Nano)
	return tokens, &clone, nil
}

// EnsureValid returns valid tokens, refreshing a short-lived access token
// before expiry when the server permits it. The persisted session never
// holds the access token; callers keep it in memory and pass it back.
func (c *Client) EnsureValid(ctx context.Context, session *SessionState, current Tokens) (Tokens, *SessionState, error) {
	if session == nil {
		return Tokens{}, nil, authRequired("authentication is required")
	}
	if strings.TrimSpace(current.AccessToken) != "" && !current.ExpiresAt.IsZero() {
		if c.cfg.Clock().Add(renewBeforeExpiry).Before(current.ExpiresAt) {
			return current, session, nil
		}
	}
	return c.Refresh(ctx, session)
}

// NeedsRefresh reports whether an access-token expiry requires renewal.
// Tokens are renewed 30 seconds before their exp value.
func NeedsRefresh(expiresAt time.Time, now time.Time) bool {
	if expiresAt.IsZero() {
		return true
	}
	return !now.Add(renewBeforeExpiry).Before(expiresAt)
}

type tokenSuccess struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

type tokenFailure struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (c *Client) exchangeCode(ctx context.Context, tokenEndpoint, code, verifier string) (Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", c.cfg.RedirectURI)
	form.Set("client_id", c.cfg.ClientID)
	return c.postToken(ctx, tokenEndpoint, form)
}

func (c *Client) postToken(ctx context.Context, tokenEndpoint string, form url.Values) (Tokens, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, temporary("token request cannot be built")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := c.cfg.HTTPClient.Do(request)
	if err != nil {
		return Tokens{}, temporary("token endpoint is temporarily unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return Tokens{}, temporary("cannot read token response")
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return Tokens{}, rateLimitedError(response)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Never expose response bodies from token endpoints.
		if isInvalidGrantBody(body) {
			return Tokens{}, authRequired("saved session is no longer valid")
		}
		if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout {
			return Tokens{}, temporary("token endpoint is temporarily unavailable")
		}
		return Tokens{}, protocolChanged("unexpected token response")
	}
	var failure tokenFailure
	_ = json.Unmarshal(body, &failure)
	if strings.EqualFold(strings.TrimSpace(failure.Error), "invalid_grant") {
		return Tokens{}, authRequired("saved session is no longer valid")
	}
	var success tokenSuccess
	if err := json.Unmarshal(body, &success); err != nil {
		return Tokens{}, protocolChanged("invalid token response")
	}
	if strings.TrimSpace(success.AccessToken) == "" {
		if strings.TrimSpace(failure.Error) != "" {
			return Tokens{}, protocolChanged("token request was rejected")
		}
		return Tokens{}, protocolChanged("invalid token response")
	}
	expiresIn := success.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}
	return Tokens{
		AccessToken:  success.AccessToken,
		RefreshToken: strings.TrimSpace(success.RefreshToken),
		ExpiresAt:    c.cfg.Clock().Add(time.Duration(expiresIn) * time.Second),
	}, nil
}

func isInvalidGrantBody(body []byte) bool {
	var failure tokenFailure
	if err := json.Unmarshal(body, &failure); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(failure.Error), "invalid_grant")
}

func rateLimitedError(response *http.Response) *Error {
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now)
	return &Error{Code: CodeRateLimited, Message: "Medicover rate limit was reached", RetryAfter: retryAfter}
}

func parseRetryAfter(value string, now func() time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var seconds int64
	if _, err := fmt.Sscanf(value, "%d", &seconds); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(value); err == nil {
		delay := parsed.Sub(now())
		if delay < 0 {
			return 0
		}
		return delay
	}
	return 0
}
