package medicover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const searchPath = "/appointments/api/v2/search-appointments"

type SearchCriteria struct {
	RegionIDs, SpecialtyIDs, ClinicIDs, DoctorIDs, LanguageIDs string
	VisitType, SearchType, StartDate, EndDate                  string
}

type Slot struct {
	Identity       string `json:"identity"`
	StableIdentity string `json:"stable_identity"`
	BookingString  string `json:"booking_string,omitempty"`
	Time           string `json:"time,omitempty"`
	Clinic         string `json:"clinic,omitempty"`
	Doctor         string `json:"doctor,omitempty"`
	Specialty      string `json:"specialty,omitempty"`
	VisitType      string `json:"visit_type,omitempty"`
}

type SearchResult struct {
	Slots []Slot `json:"slots"`
	Pages int    `json:"pages"`
}

func (c *Client) Search(ctx context.Context, accessToken string, criteria SearchCriteria) (SearchResult, error) {
	query := criteriaQuery(criteria)
	initial, err := c.getSearchJSON(ctx, searchPath+"/filters/initial-filters", query, accessToken)
	if err != nil {
		return SearchResult{}, err
	}
	if !validFilterResponse(initial) {
		return SearchResult{}, protocolChanged("incompatible initial filter response")
	}
	dependent, err := c.getSearchJSON(ctx, searchPath+"/filters", query, accessToken)
	if err != nil {
		return SearchResult{}, err
	}
	if !validFilterResponse(dependent) {
		return SearchResult{}, protocolChanged("incompatible dependent filter response")
	}
	page, pages := 1, 1
	result := SearchResult{Slots: []Slot{}}
	seenBookings := map[string]bool{}
	fallbackIndexes := map[string]int{}
	stableBookings := map[string]bool{}
	for page <= pages {
		q := cloneValues(query)
		q.Set("Page", strconv.Itoa(page))
		q.Set("PageSize", "5000")
		body, err := c.getSearchJSON(ctx, searchPath+"/slots", q, accessToken)
		if err != nil {
			if page > 1 && IsTemporary(err) {
				return SearchResult{}, &Error{Code: CodePartial, Message: "appointment search stopped after a partial response"}
			}
			return SearchResult{}, err
		}
		items, totalPages, err := decodeSlots(body)
		if err != nil {
			return SearchResult{}, protocolChanged("incompatible appointment slot response")
		}
		if totalPages > pages {
			pages = totalPages
		}
		for _, item := range items {
			if item.BookingString != "" {
				if seenBookings[item.BookingString] {
					continue
				}
				seenBookings[item.BookingString] = true
				stableBookings[item.StableIdentity] = true
				if index, ok := fallbackIndexes[item.StableIdentity]; ok {
					result.Slots[index] = item
					delete(fallbackIndexes, item.StableIdentity)
					continue
				}
				result.Slots = append(result.Slots, item)
				continue
			}
			if stableBookings[item.StableIdentity] {
				continue
			}
			if _, ok := fallbackIndexes[item.StableIdentity]; ok {
				continue
			}
			fallbackIndexes[item.StableIdentity] = len(result.Slots)
			result.Slots = append(result.Slots, item)
		}
		page++
	}
	result.Pages = pages
	return result, nil
}

func validFilterResponse(body []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return false
	}
	for _, key := range []string{"regions", "specialties", "clinics", "doctors", "doctorLanguages", "languages", "visitTypes"} {
		if raw, ok := fields[key]; ok {
			var values []json.RawMessage
			return json.Unmarshal(raw, &values) == nil
		}
	}
	return false
}

func criteriaQuery(c SearchCriteria) url.Values {
	q := url.Values{}
	for key, value := range map[string]string{"RegionIds": c.RegionIDs, "SpecialtyIds": c.SpecialtyIDs, "SelectedSpecialtyIds": c.SpecialtyIDs, "ClinicIds": c.ClinicIDs, "DoctorIds": c.DoctorIDs, "DoctorLanguageIds": c.LanguageIDs, "VisitType": c.VisitType, "SlotSearchType": c.SearchType, "StartTime": c.StartDate, "EndTime": c.EndDate} {
		if strings.TrimSpace(value) != "" {
			q.Set(key, value)
		}
	}
	q.Set("isOverbookingSearchDisabled", "true")
	return q
}

func cloneValues(source url.Values) url.Values {
	out := url.Values{}
	for k, v := range source {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (c *Client) getSearchJSON(ctx context.Context, path string, query url.Values, token string) ([]byte, error) {
	endpoint := strings.TrimSuffix(c.cfg.APIBaseURL, "/") + path + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, temporary("cannot build appointment request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Application-Version", currentAppVersion)
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Code: CodeCancelled, Message: "appointment search was cancelled"}
		}
		return nil, temporary("appointment search is temporarily unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, authRequired("Medicover authentication is required")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, rateLimitedError(resp)
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, &Error{Code: CodeConflicting, Message: "appointment search returned a conflicting response"}
	}
	if resp.StatusCode == http.StatusPreconditionFailed {
		return nil, &Error{Code: CodeStale, Message: "appointment search returned a stale response"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, temporary("appointment search returned an HTTP error")
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return nil, protocolChanged("appointment response is not JSON")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, temporary("cannot read appointment response")
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil || value == nil {
		return nil, protocolChanged("invalid appointment JSON response")
	}
	return body, nil
}

func decodeSlots(body []byte) ([]Slot, int, error) {
	var raw struct {
		Items      []json.RawMessage `json:"items"`
		TotalPages *int              `json:"totalPages"`
		PageCount  *int              `json:"pageCount"`
		TotalCount *int              `json:"totalCount"`
		PageSize   *int              `json:"pageSize"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Items == nil {
		return nil, 0, fmt.Errorf("missing items")
	}
	pages := 1
	if raw.TotalPages != nil {
		pages = *raw.TotalPages
	} else if raw.PageCount != nil {
		pages = *raw.PageCount
	} else if raw.TotalCount != nil && raw.PageSize != nil && *raw.PageSize > 0 {
		pages = (*raw.TotalCount + *raw.PageSize - 1) / *raw.PageSize
	}
	if pages < 1 {
		pages = 1
	}
	result := make([]Slot, 0, len(raw.Items))
	for _, data := range raw.Items {
		slot, err := decodeSlot(data)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, slot)
	}
	return result, pages, nil
}

func decodeSlot(data []byte) (Slot, error) {
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return Slot{}, fmt.Errorf("invalid slot")
	}
	text := func(keys ...string) string {
		var current any = value
		for _, k := range keys {
			m, ok := current.(map[string]any)
			if !ok {
				return ""
			}
			current = m[k]
			if current == nil {
				return ""
			}
		}
		switch x := current.(type) {
		case string:
			return strings.TrimSpace(x)
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		default:
			return ""
		}
	}
	first := func(paths ...[]string) string {
		for _, p := range paths {
			if v := text(p...); v != "" {
				return v
			}
		}
		return ""
	}
	s := Slot{BookingString: first([]string{"bookingString"}, []string{"booking", "bookingString"}), Time: first([]string{"appointmentDate"}, []string{"startTime"}, []string{"date"}), Clinic: first([]string{"clinic", "name"}, []string{"clinic", "value"}, []string{"clinicName"}), Doctor: first([]string{"doctor", "name"}, []string{"doctor", "value"}, []string{"doctorName"}), Specialty: first([]string{"specialty", "name"}, []string{"specialty", "value"}, []string{"specialtyName"}), VisitType: first([]string{"visitType"}, []string{"slotSearchType"})}
	if s.Time == "" {
		return Slot{}, fmt.Errorf("slot has no time")
	}
	h := sha256.Sum256([]byte(strings.Join([]string{s.Time, s.Clinic, s.Doctor, s.Specialty, s.VisitType}, "\x1f")))
	s.StableIdentity = "slot-" + hex.EncodeToString(h[:16])
	s.Identity = s.BookingString
	if s.Identity == "" {
		s.Identity = s.StableIdentity
	}
	return s, nil
}
