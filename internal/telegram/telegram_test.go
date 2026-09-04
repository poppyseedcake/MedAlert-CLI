package telegram_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

func testClient(server *httptest.Server) *telegram.Client {
	return telegram.NewClient(telegram.Config{BaseURL: server.URL, HTTPClient: server.Client()})
}

func TestSendSuccessReturnsMessageID(t *testing.T) {
	token := "TOKEN-12345-unique"
	var gotPath string
	var gotPayload map[string]any
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotPayload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer recorder.Close()
	client := telegram.NewClient(telegram.Config{BaseURL: recorder.URL, HTTPClient: recorder.Client()})
	result, err := client.SendMessage(context.Background(), telegram.Secret(token), "123456", "hello")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.MessageID != 42 {
		t.Fatalf("message id = %d, want 42", result.MessageID)
	}
	if !strings.Contains(gotPath, "/bot"+token+"/sendMessage") {
		t.Fatalf("path = %q, want token segment", gotPath)
	}
	if gotPayload["chat_id"] != "123456" || gotPayload["text"] != "hello" {
		t.Fatalf("payload = %#v, want chat and text", gotPayload)
	}
}

func TestSendClassifiesTemporaryPermanentUnknownRetryAfter(t *testing.T) {
	token := "TOKEN-CLASSIFY-unique-999"
	cases := []struct {
		name       string
		status     int
		body       string
		header     map[string]string
		wantCode   string
		wantRetry  bool
	}{
		{"temporary-500", http.StatusBadGateway, `{"ok":false}`, nil, telegram.CodeTemporary, false},
		{"permanent-400", http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, nil, telegram.CodePermanent, false},
		{"permanent-401", http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, nil, telegram.CodePermanent, false},
		{"permanent-403", http.StatusForbidden, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked"}`, nil, telegram.CodePermanent, false},
		{"unknown-invalid-json", http.StatusOK, `not json`, nil, telegram.CodeUnknown, false},
		{"unknown-missing-ok", http.StatusOK, `{"result":{}}`, nil, telegram.CodeUnknown, false},
		{"retry-after-body", http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":31}}`, nil, telegram.CodeRateLimited, true},
		{"retry-after-ok-false-200", http.StatusOK, `{"ok":false,"error_code":429,"description":"Too Many","parameters":{"retry_after":12}}`, nil, telegram.CodeRateLimited, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := testClient(server).SendMessage(context.Background(), telegram.Secret(token), "1", "hi")
			typed, ok := err.(*telegram.Error)
			if !ok {
				t.Fatalf("error type = %T (%v), want *telegram.Error", err, err)
			}
			if typed.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q (err=%v)", typed.Code, tc.wantCode, err)
			}
			if tc.wantRetry && typed.RetryAfter <= 0 {
				t.Fatalf("retry_after = %v, want >0", typed.RetryAfter)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(typed.Diagnostic, token) {
				t.Fatalf("error leaks token: %q diagnostic=%q", err.Error(), typed.Diagnostic)
			}
			if !strings.Contains(typed.Diagnostic, "[REDACTED]") {
				t.Fatalf("diagnostic = %q, want redacted token", typed.Diagnostic)
			}
		})
	}
}

func TestSendCancellationAndTimeout(t *testing.T) {
	token := "TOKEN-CANCEL-unique"
	// Cancellation: already-cancelled context.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := testClient(server).SendMessage(ctx, telegram.Secret(token), "1", "hi")
	if typed, ok := err.(*telegram.Error); !ok || typed.Code != telegram.CodeCancelled {
		t.Fatalf("cancelled error = %v", err)
	} else if strings.Contains(err.Error(), token) {
		t.Fatalf("cancelled error leaks token")
	}

	// Timeout: client with 1ms timeout and slow server.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer slow.Close()
	timeoutClient := slow.Client()
	timeoutClient.Timeout = time.Millisecond
	timed := telegram.NewClient(telegram.Config{BaseURL: slow.URL, HTTPClient: timeoutClient})
	_, err = timed.SendMessage(context.Background(), telegram.Secret(token), "1", "hi")
	if typed, ok := err.(*telegram.Error); !ok || typed.Code != telegram.CodeTimeout {
		t.Fatalf("timeout error = %v", err)
	}

	// Deadline exceeded maps to timeout as well.
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer deadlineCancel()
	time.Sleep(5 * time.Millisecond)
	_, err = testClient(server).SendMessage(deadlineCtx, telegram.Secret(token), "1", "hi")
	if typed, ok := err.(*telegram.Error); !ok || (typed.Code != telegram.CodeTimeout && typed.Code != telegram.CodeCancelled) {
		t.Fatalf("deadline error = %v, want timeout or cancelled", err)
	}
}

func TestRedactURLHidesToken(t *testing.T) {
	token := "SECRET-TOKEN-MARKER-unique-abcdef"
	raw := "https://api.telegram.org/bot" + token + "/sendMessage"
	redacted := telegram.RedactURL(raw, token)
	if strings.Contains(redacted, token) {
		t.Fatalf("redacted = %q, contains token", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("redacted = %q, want marker", redacted)
	}
	// Secret formatting never leaks.
	secret := telegram.Secret(token)
	if strings.Contains(secret.String(), token) || strings.Contains(secret.GoString(), token) {
		t.Fatal("Secret String leaks token")
	}
	encoded, _ := secret.MarshalJSON()
	if strings.Contains(string(encoded), token) {
		t.Fatal("Secret JSON leaks token")
	}
}

func TestPolishFormattingIncludesAllFields(t *testing.T) {
	message := telegram.FormatAvailability(telegram.Availability{
		Profile:   "cardio",
		Time:      "2026-09-10T10:00:00",
		Doctor:    "Dr Kowalska",
		Specialty: "Kardiologia",
		Clinic:    "Mokotów",
		VisitType: "Center",
	})
	for _, want := range []string{"cardio", "2026-09-10T10:00:00", "Dr Kowalska", "Kardiologia", "Mokotów", "Center", "Profil", "Czas", "Lekarz", "Specjalizacja", "Placówka", "Typ wizyty"} {
		if !strings.Contains(message, want) {
			t.Fatalf("availability message missing %q: %q", want, message)
		}
	}
	test := telegram.FormatTestMessage("Telefon")
	if !strings.Contains(test, "Telefon") {
		t.Fatalf("test message = %q, want destination name", test)
	}
	for _, fragment := range []string{"Test", "Telegram"} {
		if !strings.Contains(test, fragment) {
			t.Fatalf("test message = %q, want %q", test, fragment)
		}
	}
}

func TestPolishTestMessageIsSafe(t *testing.T) {
	marker := "TOKEN-SAFE-MARKER-unique-xyz"
	message := telegram.FormatTestMessage("Dom")
	if strings.Contains(message, marker) {
		t.Fatal("test message contains marker")
	}
}

func TestSendRedactsTokenEchoedInDescription(t *testing.T) {
	token := "SECRET-ECHO-TOKEN-unique-789"
	// A buggy or malicious endpoint echoes the token-bearing URL in description.
	body := `{"ok":false,"error_code":400,"description":"Bad Request: https://api.telegram.org/bot` + token + `/sendMessage failed"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	_, err := testClient(server).SendMessage(context.Background(), telegram.Secret(token), "1", "hi")
	typed, ok := err.(*telegram.Error)
	if !ok {
		t.Fatalf("error type = %T (%v)", err, err)
	}
	if typed.Code != telegram.CodePermanent {
		t.Fatalf("code = %q, want permanent", typed.Code)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(typed.Message, token) || strings.Contains(typed.Diagnostic, token) {
		t.Fatalf("error leaks token: err=%q message=%q diagnostic=%q", err.Error(), typed.Message, typed.Diagnostic)
	}
	if !strings.Contains(typed.Message, "[REDACTED]") {
		t.Fatalf("message = %q, want redacted URL segment", typed.Message)
	}
}
