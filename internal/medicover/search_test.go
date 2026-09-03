package medicover_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
)

func TestSearchUsesV2CriteriaPaginationAndStableFallback(t *testing.T) {
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		if !strings.HasPrefix(r.URL.Path, "/appointments/api/v2/") {
			t.Errorf("legacy path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/slots") {
			if r.URL.Query().Get("Page") == "1" {
				_, _ = w.Write([]byte(`{"items":[{"bookingString":"book-1","appointmentDate":"2026-09-10T10:00:00","clinic":{"name":"A"},"doctor":null,"specialty":{"value":"Cardiology"},"visitType":"Center"},{"bookingString":"book-2","appointmentDate":"2026-09-10T10:00:00","clinic":{"name":"A"},"doctor":null,"specialty":{"value":"Cardiology"},"visitType":"Center"}],"totalPages":2}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"appointmentDate":"2026-09-10T10:00:00","clinic":{"name":"A"},"doctor":null,"specialty":{"value":"Cardiology"},"visitType":"Center"},{"appointmentDate":"2026-09-11T10:00:00","clinic":null,"doctor":{"name":"Dr B"},"specialty":null,"visitType":"Center"}],"totalPages":2}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/initial-filters") {
			_, _ = w.Write([]byte(`{"regions":[],"unknown":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"clinics":[],"unknown":true}`))
	}))
	defer server.Close()
	client := medicover.NewClient(medicover.Config{APIBaseURL: server.URL, HTTPClient: server.Client()})
	result, err := client.Search(context.Background(), "access", medicover.SearchCriteria{RegionIDs: "204", SpecialtyIDs: "132", ClinicIDs: "10", DoctorIDs: "20", LanguageIDs: "4", VisitType: "Center", SearchType: "DiagnosticProcedure", StartDate: "2026-09-01", EndDate: "2026-09-30"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Slots) != 3 || result.Pages != 2 {
		t.Fatalf("result=%+v", result)
	}
	if result.Slots[0].Identity != "book-1" || result.Slots[1].Identity != "book-2" || result.Slots[0].StableIdentity != result.Slots[1].StableIdentity || result.Slots[2].Identity != result.Slots[2].StableIdentity {
		t.Fatalf("identities=%+v", result.Slots)
	}
	for _, request := range requests {
		for _, part := range []string{"RegionIds=204", "SpecialtyIds=132", "ClinicIds=10", "DoctorIds=20", "DoctorLanguageIds=4", "VisitType=Center", "SlotSearchType=DiagnosticProcedure", "StartTime=2026-09-01", "EndTime=2026-09-30"} {
			if !strings.Contains(request, part) {
				t.Errorf("%s missing %s", request, part)
			}
		}
	}
}

func TestSearchDistinguishesProtocolHTTPAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		code    string
	}{
		{"protocol", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"changed":[]}`))
		}, medicover.CodeProtocolChanged},
		{"http", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "failure", http.StatusBadGateway) }, medicover.CodeTemporary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.handler)
			defer s.Close()
			c := medicover.NewClient(medicover.Config{APIBaseURL: s.URL, HTTPClient: s.Client()})
			_, err := c.Search(context.Background(), "x", medicover.SearchCriteria{})
			var typed *medicover.Error
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatalf("error=%v", err)
			}
		})
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := medicover.NewClient(medicover.Config{APIBaseURL: s.URL, HTTPClient: s.Client()})
	_, err := c.Search(ctx, "x", medicover.SearchCriteria{})
	var typed *medicover.Error
	if !errors.As(err, &typed) || typed.Code != medicover.CodeCancelled {
		t.Fatalf("error=%v", err)
	}
}

func TestSearchRejectsConflictingDuplicateSlotRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/slots") {
			_, _ = w.Write([]byte(`{"items":[{"bookingString":"same-booking","appointmentDate":"2099-09-10T10:00:00Z"},{"bookingString":"same-booking","appointmentDate":"2099-09-11T10:00:00Z"}],"totalPages":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"regions":[]}`))
	}))
	defer server.Close()
	_, err := medicover.NewClient(medicover.Config{APIBaseURL: server.URL, HTTPClient: server.Client()}).Search(context.Background(), "access", medicover.SearchCriteria{})
	var typed *medicover.Error
	if !errors.As(err, &typed) || typed.Code != medicover.CodeConflicting {
		t.Fatalf("error = %v, want conflicting result", err)
	}
}

func TestSearchDistinguishesConflictStaleTimeoutAndPartial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"conflict", http.StatusConflict, medicover.CodeConflicting}, {"stale", http.StatusPreconditionFailed, medicover.CodeStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/slots") {
					w.WriteHeader(tc.status)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/initial-filters") {
					_, _ = w.Write([]byte(`{"regions":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{"clinics":[]}`))
			}))
			defer s.Close()
			_, err := medicover.NewClient(medicover.Config{APIBaseURL: s.URL, HTTPClient: s.Client()}).Search(context.Background(), "x", medicover.SearchCriteria{})
			var typed *medicover.Error
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatalf("error=%v", err)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(50 * time.Millisecond) }))
		defer s.Close()
		httpClient := s.Client()
		httpClient.Timeout = time.Millisecond
		_, err := medicover.NewClient(medicover.Config{APIBaseURL: s.URL, HTTPClient: httpClient}).Search(context.Background(), "x", medicover.SearchCriteria{})
		var typed *medicover.Error
		if !errors.As(err, &typed) || typed.Code != medicover.CodeTemporary {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("partial", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/initial-filters") {
				_, _ = w.Write([]byte(`{"regions":[]}`))
				return
			}
			if strings.HasSuffix(r.URL.Path, "/filters") {
				_, _ = w.Write([]byte(`{"clinics":[]}`))
				return
			}
			if r.URL.Query().Get("Page") == "1" {
				_, _ = w.Write([]byte(`{"items":[],"totalPages":2}`))
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer s.Close()
		_, err := medicover.NewClient(medicover.Config{APIBaseURL: s.URL, HTTPClient: s.Client()}).Search(context.Background(), "x", medicover.SearchCriteria{})
		var typed *medicover.Error
		if !errors.As(err, &typed) || typed.Code != medicover.CodePartial {
			t.Fatalf("error=%v", err)
		}
	})
}
