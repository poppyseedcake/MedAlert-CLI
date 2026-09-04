// Package telegram owns Polish message formatting and direct Telegram Bot
// API calls. It is a concrete deep module: there is no general provider
// interface and no provider selection logic.
//
// Callers never see request URLs, transport JSON, or raw tokens. All token
// handling stays inside this module and errors never contain the token,
// including the token segment in the Telegram request URL.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.telegram.org"
	requestTimeout = 15 * time.Second

	// Delivery outcome codes returned across the seam. HTTP and token
	// details never cross the seam.
	CodeTemporary   = "temporary_failure"
	CodePermanent   = "permanent_failure"
	CodeUnknown     = "unknown_delivery"
	CodeRateLimited = "rate_limited"
	CodeCancelled   = "cancelled"
	CodeTimeout     = "timeout"
)

// Error is the only error type that crosses the Telegram seam. Message is
// always sanitized: it never contains the bot token, including the token
// segment of the request URL. Diagnostic carries a redacted URL for operators.
type Error struct {
	Code       string
	Message    string
	RetryAfter time.Duration
	Diagnostic string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Secret hides a bot token in normal formatting. Use Expose only at the exact
// transport boundary that needs the raw value.
type Secret string

func (s Secret) String() string   { return "[REDACTED]" }
func (s Secret) GoString() string { return "[REDACTED]" }
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}

// Expose returns the raw token value. Callers must never log or return it.
func (s Secret) Expose() string { return string(s) }

// Config selects endpoints and seams. BaseURL defaults to the production Bot
// API. HTTPClient and Clock are private test seams.
type Config struct {
	BaseURL    string
	HTTPClient *http.Client
}

func (c *Config) withDefaults() Config {
	cfg := *c
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	return cfg
}

// Client sends Telegram messages. It holds no per-destination state; tokens
// and chat identifiers are passed per call.
type Client struct {
	cfg Config
}

func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg.withDefaults()}
}

// Result reports a delivered message.
type Result struct {
	MessageID int64  `json:"message_id"`
	ChatID    string `json:"chat_id"`
}

// Availability carries the display fields for one Polish availability
// message. All fields are display data only.
type Availability struct {
	Profile   string
	Time      string
	Doctor    string
	Specialty string
	Clinic    string
	VisitType string
}

// FormatTestMessage builds a safe Polish test message. It never contains the
// bot token.
func FormatTestMessage(destinationName string) string {
	name := strings.TrimSpace(destinationName)
	if name == "" {
		name = "Telegram"
	}
	return fmt.Sprintf("MedAlert: Test powiadomienia Telegram dla „%s”. Jeśli widzisz tę wiadomość, powiadomienia działają.", name)
}

// FormatAvailability builds a Polish availability message that always
// includes the profile, time, doctor, specialty, clinic, and visit type.
// Empty values render as an em dash so the labels stay visible.
func FormatAvailability(availability Availability) string {
	value := func(raw string) string {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return "—"
		}
		return trimmed
	}
	var builder strings.Builder
	builder.WriteString("MedAlert: Dostępny termin\n")
	fmt.Fprintf(&builder, "Profil: %s\n", value(availability.Profile))
	fmt.Fprintf(&builder, "Czas: %s\n", value(availability.Time))
	fmt.Fprintf(&builder, "Lekarz: %s\n", value(availability.Doctor))
	fmt.Fprintf(&builder, "Specjalizacja: %s\n", value(availability.Specialty))
	fmt.Fprintf(&builder, "Placówka: %s\n", value(availability.Clinic))
	fmt.Fprintf(&builder, "Typ wizyty: %s", value(availability.VisitType))
	return builder.String()
}

// SendMessage sends one text message through sendMessage. On success it
// returns the delivery result. On failure it returns a classified *Error
// without the token.
func (c *Client) SendMessage(ctx context.Context, token Secret, chatID, text string) (Result, error) {
	rawToken := token.Expose()
	if strings.TrimSpace(rawToken) == "" {
		return Result{}, &Error{Code: CodePermanent, Message: "telegram destination is missing its bot token", Diagnostic: redactedEndpoint(c.cfg.BaseURL)}
	}
	if strings.TrimSpace(chatID) == "" {
		return Result{}, &Error{Code: CodePermanent, Message: "telegram destination is missing its chat identifier", Diagnostic: redactedEndpoint(c.cfg.BaseURL)}
	}
	if strings.TrimSpace(text) == "" {
		return Result{}, &Error{Code: CodePermanent, Message: "telegram message is empty", Diagnostic: redactedEndpoint(c.cfg.BaseURL)}
	}
	endpoint := strings.TrimSuffix(strings.TrimSpace(c.cfg.BaseURL), "/") + "/bot" + rawToken + "/sendMessage"
	diagnostic := redactedEndpoint(c.cfg.BaseURL)
	payload, err := json.Marshal(map[string]any{
		"chat_id":                  strings.TrimSpace(chatID),
		"disable_web_page_preview": true,
		"text":                     text,
	})
	if err != nil {
		return Result{}, &Error{Code: CodeUnknown, Message: "cannot build telegram message", Diagnostic: diagnostic}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Result{}, &Error{Code: CodeUnknown, Message: "cannot build telegram request", Diagnostic: diagnostic}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.cfg.HTTPClient.Do(request)
	if err != nil {
		return Result{}, classifyTransportError(ctx, err, diagnostic)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, classifyTransportError(ctx, ctx.Err(), diagnostic)
		}
		return Result{}, &Error{Code: CodeTemporary, Message: "cannot read telegram response", Diagnostic: diagnosticWithStatus(diagnostic, response.StatusCode)}
	}
	return classifyResponse(response.StatusCode, response.Header.Get("Retry-After"), body, diagnostic)
}

// RedactURL replaces the token segment of a Telegram Bot API URL with
// [REDACTED]. It is exported for diagnostics and tests: operators see the
// endpoint shape without the secret.
func RedactURL(rawURL, token string) string {
	if rawURL == "" {
		return ""
	}
	trimmedToken := strings.TrimSpace(token)
	if trimmedToken != "" && strings.Contains(rawURL, trimmedToken) {
		return strings.ReplaceAll(rawURL, trimmedToken, "[REDACTED]")
	}
	// Fall back to masking any /bot<something>/ segment.
	start := strings.Index(rawURL, "/bot")
	if start < 0 {
		return rawURL
	}
	rest := rawURL[start+len("/bot"):]
	end := strings.Index(rest, "/")
	if end < 0 {
		return rawURL[:start] + "/bot[REDACTED]"
	}
	return rawURL[:start] + "/bot[REDACTED]" + rest[end:]
}

func redactedEndpoint(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	return base + "/bot[REDACTED]/sendMessage"
}

func diagnosticWithStatus(diagnostic string, status int) string {
	return fmt.Sprintf("%s status=%d", diagnostic, status)
}

func classifyTransportError(ctx context.Context, err error, diagnostic string) *Error {
	if ctx != nil && ctx.Err() == context.Canceled {
		return &Error{Code: CodeCancelled, Message: "telegram send was cancelled", Diagnostic: diagnostic}
	}
	if ctx != nil && ctx.Err() == context.DeadlineExceeded {
		return &Error{Code: CodeTimeout, Message: "telegram send timed out", Diagnostic: diagnostic}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Code: CodeCancelled, Message: "telegram send was cancelled", Diagnostic: diagnostic}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: "telegram send timed out", Diagnostic: diagnostic}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Code: CodeTimeout, Message: "telegram send timed out", Diagnostic: diagnostic}
	}
	// http.Client timeouts surface as *url.Error with Timeout() true, which
	// the net.Error branch already covers. Anything else is a temporary
	// network failure that is safe to retry.
	return &Error{Code: CodeTemporary, Message: "telegram is temporarily unavailable", Diagnostic: diagnostic}
}

type telegramResponse struct {
	OK          *bool `json:"ok"`
	Result      *struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
	ErrorCode   *int `json:"error_code"`
	Description string `json:"description"`
	Parameters  *struct {
		RetryAfter *int `json:"retry_after"`
	} `json:"parameters"`
}

func classifyResponse(status int, retryAfterHeader string, body []byte, diagnostic string) (Result, error) {
	withStatus := diagnosticWithStatus(diagnostic, status)
	if status == http.StatusTooManyRequests {
		delay := parseRetryAfterBody(body, retryAfterHeader)
		return Result{}, &Error{Code: CodeRateLimited, Message: "telegram rate limit was reached", RetryAfter: delay, Diagnostic: withStatus}
	}
	if status == http.StatusRequestTimeout {
		return Result{}, &Error{Code: CodeTimeout, Message: "telegram send timed out", Diagnostic: withStatus}
	}
	if status >= 500 {
		return Result{}, &Error{Code: CodeTemporary, Message: "telegram is temporarily unavailable", Diagnostic: withStatus}
	}
	// Decode the envelope for 2xx and for Telegram-style 4xx bodies that
	// carry ok:false with an error_code.
	var envelope telegramResponse
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.OK == nil {
		// Non-JSON or shape changes are unknown: the message may or may not
		// have been accepted, so callers keep it pending and retry.
		if status >= 200 && status < 300 {
			return Result{}, &Error{Code: CodeUnknown, Message: "unknown telegram delivery", Diagnostic: withStatus}
		}
		if status == 408 {
			return Result{}, &Error{Code: CodeTimeout, Message: "telegram send timed out", Diagnostic: withStatus}
		}
		if status >= 400 && status < 500 {
			return Result{}, &Error{Code: CodePermanent, Message: "telegram rejected the message", Diagnostic: withStatus}
		}
		return Result{}, &Error{Code: CodeUnknown, Message: "unknown telegram delivery", Diagnostic: withStatus}
	}
	if *envelope.OK {
		var messageID int64
		if envelope.Result != nil {
			messageID = envelope.Result.MessageID
		}
		return Result{MessageID: messageID}, nil
	}
	// ok:false: prefer the Telegram error_code, fall back to HTTP status.
	code := status
	if envelope.ErrorCode != nil {
		code = *envelope.ErrorCode
	}
	if delay, ok := retryAfterFromEnvelope(envelope, retryAfterHeader); ok {
		return Result{}, &Error{Code: CodeRateLimited, Message: "telegram rate limit was reached", RetryAfter: delay, Diagnostic: withStatus}
	}
	switch {
	case code == 429:
		delay := parseRetryAfterBody(body, retryAfterHeader)
		return Result{}, &Error{Code: CodeRateLimited, Message: "telegram rate limit was reached", RetryAfter: delay, Diagnostic: withStatus}
	case code == 400, code == 401, code == 403, code == 404:
		return Result{}, &Error{Code: CodePermanent, Message: safeDescription(envelope.Description), Diagnostic: withStatus}
	case code >= 500:
		return Result{}, &Error{Code: CodeTemporary, Message: "telegram is temporarily unavailable", Diagnostic: withStatus}
	case code == 408:
		return Result{}, &Error{Code: CodeTimeout, Message: "telegram send timed out", Diagnostic: withStatus}
	case code >= 400 && code < 500:
		return Result{}, &Error{Code: CodePermanent, Message: safeDescription(envelope.Description), Diagnostic: withStatus}
	default:
		return Result{}, &Error{Code: CodeUnknown, Message: "unknown telegram delivery", Diagnostic: withStatus}
	}
}

func retryAfterFromEnvelope(envelope telegramResponse, header string) (time.Duration, bool) {
	if envelope.Parameters != nil && envelope.Parameters.RetryAfter != nil && *envelope.Parameters.RetryAfter > 0 {
		return time.Duration(*envelope.Parameters.RetryAfter) * time.Second, true
	}
	if strings.TrimSpace(header) != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second, true
		}
	}
	return 0, false
}

func parseRetryAfterBody(body []byte, header string) time.Duration {
	var envelope telegramResponse
	if err := json.Unmarshal(body, &envelope); err == nil {
		if delay, ok := retryAfterFromEnvelope(envelope, header); ok {
			return delay
		}
	}
	if strings.TrimSpace(header) != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return 0
}

func safeDescription(description string) string {
	trimmed := strings.TrimSpace(description)
	if trimmed == "" {
		return "telegram rejected the message"
	}
	if len(trimmed) > 500 {
		return trimmed[:500]
	}
	return trimmed
}

// IsTemporary reports whether a send should be retried later.
func IsTemporary(err error) bool {
	var telegramErr *Error
	if errors.As(err, &telegramErr) {
		return telegramErr.Code == CodeTemporary || telegramErr.Code == CodeUnknown || telegramErr.Code == CodeRateLimited || telegramErr.Code == CodeTimeout
	}
	return false
}

// IsPermanent reports whether a send failed without a retry.
func IsPermanent(err error) bool {
	var telegramErr *Error
	if errors.As(err, &telegramErr) {
		return telegramErr.Code == CodePermanent
	}
	return false
}
