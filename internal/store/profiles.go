// Package store profile operations own Observation Profile configuration.
//
// SQLite stores profile search criteria, check intervals, and enabled state.
// Profiles keep a stable identity when criteria are edited. Disabling a
// profile pauses observation without deleting its row. Deleting a profile
// removes its configuration; future Observation History tables reference
// profiles with ON DELETE CASCADE so history disappears with the profile.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var profileIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

var visitTypePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Allowed SlotSearchType values. "0" is accepted as a legacy alias for
// Standard and is normalized to Standard.
const (
	SearchTypeStandard            = "Standard"
	SearchTypeDiagnosticProcedure = "DiagnosticProcedure"
	defaultSearchType             = SearchTypeStandard
	minCheckIntervalMinutes       = 1
	maxCheckIntervalMinutes       = 43200
	maxMedicoverFilterID          = 10000000
	dateLayout                    = "2006-01-02"
)

// Profile describes one Observation Profile with a stable identity.
// ID lists are stored as normalized comma-separated Medicover IDs, for
// example "204" or "204,205". An empty list means "any".
type Profile struct {
	ID                   string `json:"id"`
	AccountID            string `json:"account_id"`
	RegionIDs            string `json:"region_ids"`
	SpecialtyIDs         string `json:"specialty_ids"`
	ClinicIDs            string `json:"clinic_ids"`
	DoctorIDs            string `json:"doctor_ids"`
	LanguageIDs          string `json:"language_ids"`
	VisitType            string `json:"visit_type"`
	SearchType           string `json:"search_type"`
	StartDate            string `json:"start_date"`
	EndDate              string `json:"end_date"`
	CheckIntervalMinutes int    `json:"check_interval_minutes"`
	Enabled              bool   `json:"enabled"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

// ProfileUpdate carries optional edit fields. Nil means keep the current value.
// A pointer to an empty string clears an optional list, date, or visit type.
type ProfileUpdate struct {
	RegionIDs            *string
	SpecialtyIDs         *string
	ClinicIDs            *string
	DoctorIDs            *string
	LanguageIDs          *string
	VisitType            *string
	SearchType           *string
	StartDate            *string
	EndDate              *string
	CheckIntervalMinutes *int
}

var (
	// ErrProfileExists is returned when a profile id is already taken.
	ErrProfileExists = errors.New("profile already exists")
	// ErrProfileNotFound is returned when a profile id does not exist.
	ErrProfileNotFound = errors.New("profile not found")
	// ErrProfileInvalid is returned when profile input fails validation.
	ErrProfileInvalid = errors.New("invalid profile input")
)

// CreateProfile stores a new observation profile. The referenced account must
// already exist. The profile starts enabled unless Enabled is false.
func (s *Store) CreateProfile(profile Profile) (Profile, error) {
	if err := ValidateProfile(profile); err != nil {
		return Profile{}, err
	}
	if _, err := s.GetAccount(profile.AccountID); err != nil {
		if errors.Is(err, ErrAccountNotFound) || errors.Is(err, ErrAccountInvalid) {
			return Profile{}, fmt.Errorf("%w: %s", ErrAccountNotFound, profile.AccountID)
		}
		return Profile{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	profile.CreatedAt = now
	profile.UpdatedAt = now
	enabled := 0
	if profile.Enabled {
		enabled = 1
	}
	// Default an empty search type to Standard so every row carries an
	// explicit value for later filter and slot requests.
	if strings.TrimSpace(profile.SearchType) == "" {
		profile.SearchType = defaultSearchType
	}
	_, err := s.db.Exec(
		`INSERT INTO profiles (id, account_id, region_ids, specialty_ids, clinic_ids, doctor_ids, language_ids, visit_type, search_type, start_date, end_date, check_interval_minutes, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		profile.ID, profile.AccountID, profile.RegionIDs, profile.SpecialtyIDs, profile.ClinicIDs, profile.DoctorIDs, profile.LanguageIDs, profile.VisitType, profile.SearchType, profile.StartDate, profile.EndDate, profile.CheckIntervalMinutes, enabled, profile.CreatedAt, profile.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "primary") {
			return Profile{}, fmt.Errorf("%w: %s", ErrProfileExists, profile.ID)
		}
		if strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			return Profile{}, fmt.Errorf("%w: %s", ErrAccountNotFound, profile.AccountID)
		}
		return Profile{}, fmt.Errorf("create profile: %w", err)
	}
	return profile, nil
}

// ListProfiles returns profiles ordered by account and id. An empty
// accountFilter returns every profile; otherwise only that account's profiles.
func (s *Store) ListProfiles(accountFilter string) ([]Profile, error) {
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(accountFilter) == "" {
		rows, err = s.db.Query(`SELECT id, account_id, region_ids, specialty_ids, clinic_ids, doctor_ids, language_ids, visit_type, search_type, start_date, end_date, check_interval_minutes, enabled, created_at, updated_at FROM profiles ORDER BY account_id, id`)
	} else {
		if !profileIDPattern.MatchString(accountFilter) {
			return nil, fmt.Errorf("%w: account id %q", ErrProfileInvalid, accountFilter)
		}
		rows, err = s.db.Query(`SELECT id, account_id, region_ids, specialty_ids, clinic_ids, doctor_ids, language_ids, visit_type, search_type, start_date, end_date, check_interval_minutes, enabled, created_at, updated_at FROM profiles WHERE account_id = ? ORDER BY id`, accountFilter)
	}
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer rows.Close()
	profiles := []Profile{}
	for rows.Next() {
		var profile Profile
		var enabled int
		if err := rows.Scan(&profile.ID, &profile.AccountID, &profile.RegionIDs, &profile.SpecialtyIDs, &profile.ClinicIDs, &profile.DoctorIDs, &profile.LanguageIDs, &profile.VisitType, &profile.SearchType, &profile.StartDate, &profile.EndDate, &profile.CheckIntervalMinutes, &enabled, &profile.CreatedAt, &profile.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list profiles: %w", err)
		}
		profile.Enabled = enabled == 1
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	return profiles, nil
}

// GetProfile returns one profile by its stable id.
func (s *Store) GetProfile(id string) (Profile, error) {
	if !profileIDPattern.MatchString(id) {
		return Profile{}, fmt.Errorf("%w: profile id %q", ErrProfileInvalid, id)
	}
	var profile Profile
	var enabled int
	err := s.db.QueryRow(
		`SELECT id, account_id, region_ids, specialty_ids, clinic_ids, doctor_ids, language_ids, visit_type, search_type, start_date, end_date, check_interval_minutes, enabled, created_at, updated_at FROM profiles WHERE id = ?`, id,
	).Scan(&profile.ID, &profile.AccountID, &profile.RegionIDs, &profile.SpecialtyIDs, &profile.ClinicIDs, &profile.DoctorIDs, &profile.LanguageIDs, &profile.VisitType, &profile.SearchType, &profile.StartDate, &profile.EndDate, &profile.CheckIntervalMinutes, &enabled, &profile.CreatedAt, &profile.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, id)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("show profile: %w", err)
	}
	profile.Enabled = enabled == 1
	return profile, nil
}

// UpdateProfile changes search criteria while keeping the profile identity,
// its account, and its enabled state. Use SetProfileEnabled to pause or
// resume observation.
func (s *Store) UpdateProfile(id string, update ProfileUpdate) (Profile, error) {
	current, err := s.GetProfile(id)
	if err != nil {
		return Profile{}, err
	}
	if update.RegionIDs != nil {
		current.RegionIDs = *update.RegionIDs
	}
	if update.SpecialtyIDs != nil {
		current.SpecialtyIDs = *update.SpecialtyIDs
	}
	if update.ClinicIDs != nil {
		current.ClinicIDs = *update.ClinicIDs
	}
	if update.DoctorIDs != nil {
		current.DoctorIDs = *update.DoctorIDs
	}
	if update.LanguageIDs != nil {
		current.LanguageIDs = *update.LanguageIDs
	}
	if update.VisitType != nil {
		current.VisitType = *update.VisitType
	}
	if update.SearchType != nil {
		current.SearchType = *update.SearchType
	}
	if update.StartDate != nil {
		current.StartDate = *update.StartDate
	}
	if update.EndDate != nil {
		current.EndDate = *update.EndDate
	}
	if update.CheckIntervalMinutes != nil {
		current.CheckIntervalMinutes = *update.CheckIntervalMinutes
	}
	if err := ValidateProfile(current); err != nil {
		return Profile{}, err
	}
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	enabled := 0
	if current.Enabled {
		enabled = 1
	}
	result, err := s.db.Exec(
		`UPDATE profiles SET region_ids = ?, specialty_ids = ?, clinic_ids = ?, doctor_ids = ?, language_ids = ?, visit_type = ?, search_type = ?, start_date = ?, end_date = ?, check_interval_minutes = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		current.RegionIDs, current.SpecialtyIDs, current.ClinicIDs, current.DoctorIDs, current.LanguageIDs, current.VisitType, current.SearchType, current.StartDate, current.EndDate, current.CheckIntervalMinutes, enabled, current.UpdatedAt, id,
	)
	if err != nil {
		return Profile{}, fmt.Errorf("edit profile: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Profile{}, fmt.Errorf("edit profile: %w", err)
	}
	if affected == 0 {
		return Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, id)
	}
	return current, nil
}

// SetProfileEnabled pauses (false) or resumes (true) observation without
// deleting the profile row. Saved state and history rows stay untouched.
func (s *Store) SetProfileEnabled(id string, enabled bool) (Profile, error) {
	current, err := s.GetProfile(id)
	if err != nil {
		return Profile{}, err
	}
	current.Enabled = enabled
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	value := 0
	if enabled {
		value = 1
	}
	result, err := s.db.Exec(`UPDATE profiles SET enabled = ?, updated_at = ? WHERE id = ?`, value, current.UpdatedAt, id)
	if err != nil {
		return Profile{}, fmt.Errorf("set profile enabled: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Profile{}, fmt.Errorf("set profile enabled: %w", err)
	}
	if affected == 0 {
		return Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, id)
	}
	return current, nil
}

// DeleteProfile removes the profile configuration. Observation History tables
// reference profiles with ON DELETE CASCADE, so history that belongs to the
// profile disappears in the same transaction.
func (s *Store) DeleteProfile(id string) error {
	if !profileIDPattern.MatchString(id) {
		return fmt.Errorf("%w: profile id %q", ErrProfileInvalid, id)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Delete known history tables when they exist. Older databases (schema 3
	// without later history tables) simply skip the missing tables so the
	// profile delete still succeeds.
	for _, table := range []string{"observation_runs", "availability_episodes", "telegram_deliveries", "operational_incidents"} {
		if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE profile_id = ?`, table), id); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
				return fmt.Errorf("delete profile history: %w", err)
			}
		}
	}
	result, err := tx.Exec(`DELETE FROM profiles WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrProfileNotFound, id)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	return nil
}

// ValidateProfile checks identity, account reference, search criteria, date
// range, and check interval without touching the database.
func ValidateProfile(profile Profile) error {
	if !profileIDPattern.MatchString(profile.ID) {
		return fmt.Errorf("%w: profile id must match [A-Za-z0-9_-], start alphanumerically, max 64", ErrProfileInvalid)
	}
	if !profileIDPattern.MatchString(profile.AccountID) {
		return fmt.Errorf("%w: account id must match [A-Za-z0-9_-], start alphanumerically, max 64", ErrProfileInvalid)
	}
	region, err := NormalizeIDList(profile.RegionIDs)
	if err != nil || region == "" {
		return fmt.Errorf("%w: region is required (comma-separated Medicover IDs)", ErrProfileInvalid)
	}
	specialty, err := NormalizeIDList(profile.SpecialtyIDs)
	if err != nil || specialty == "" {
		return fmt.Errorf("%w: specialty is required (comma-separated Medicover IDs)", ErrProfileInvalid)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"clinic", profile.ClinicIDs},
		{"doctor", profile.DoctorIDs},
		{"language", profile.LanguageIDs},
	} {
		if _, err := NormalizeIDList(field.value); err != nil {
			return fmt.Errorf("%w: %s IDs must be comma-separated positive integers", ErrProfileInvalid, field.name)
		}
	}
	if _, err := NormalizeSearchType(profile.SearchType); err != nil {
		return fmt.Errorf("%w: search type must be Standard or DiagnosticProcedure", ErrProfileInvalid)
	}
	if strings.TrimSpace(profile.VisitType) != "" {
		visit := strings.TrimSpace(profile.VisitType)
		if !visitTypePattern.MatchString(visit) {
			return fmt.Errorf("%w: visit type must match [A-Za-z0-9_-], max 64", ErrProfileInvalid)
		}
	}
	if _, err := NormalizeDate(profile.StartDate, true); err != nil {
		return fmt.Errorf("%w: start date must be YYYY-MM-DD", ErrProfileInvalid)
	}
	if _, err := NormalizeDate(profile.EndDate, true); err != nil {
		return fmt.Errorf("%w: end date must be YYYY-MM-DD", ErrProfileInvalid)
	}
	start := strings.TrimSpace(profile.StartDate)
	end := strings.TrimSpace(profile.EndDate)
	if start != "" && end != "" && end < start {
		return fmt.Errorf("%w: end date must not be before start date", ErrProfileInvalid)
	}
	if profile.CheckIntervalMinutes < minCheckIntervalMinutes || profile.CheckIntervalMinutes > maxCheckIntervalMinutes {
		return fmt.Errorf("%w: check interval must be %d..%d minutes", ErrProfileInvalid, minCheckIntervalMinutes, maxCheckIntervalMinutes)
	}
	return nil
}

// NormalizeIDList trims, validates, deduplicates, and rejoins a
// comma-separated Medicover ID list. Empty input normalizes to "" (any).
// IDs must be positive integers within a sane portal range.
func NormalizeIDList(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	parts := strings.Split(trimmed, ",")
	seen := map[string]bool{}
	ordered := []int{}
	byString := map[int]string{}
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		if strings.Contains(item, " ") || strings.Contains(item, "\t") {
			return "", fmt.Errorf("invalid id list %q", raw)
		}
		number, err := strconv.Atoi(item)
		if err != nil || number < 1 || number > maxMedicoverFilterID {
			return "", fmt.Errorf("invalid id list %q", raw)
		}
		canonical := strconv.Itoa(number)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		ordered = append(ordered, number)
		byString[number] = canonical
	}
	if len(ordered) == 0 {
		return "", nil
	}
	// Keep numeric order stable so "205,204" and "204,205" map to one form
	// and repeated checks hash the same criteria.
	sort.Ints(ordered)
	normalized := make([]string, 0, len(ordered))
	for _, number := range ordered {
		normalized = append(normalized, byString[number])
	}
	return strings.Join(normalized, ","), nil
}

// NormalizeSearchType trims and validates the SlotSearchType. Legacy "0"
// maps to Standard. Empty maps to the default Standard.
func NormalizeSearchType(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultSearchType, nil
	}
	if trimmed == "0" {
		return SearchTypeStandard, nil
	}
	switch trimmed {
	case SearchTypeStandard, SearchTypeDiagnosticProcedure:
		return trimmed, nil
	default:
		return "", fmt.Errorf("invalid search type %q", raw)
	}
}

// NormalizeDate trims and validates an optional YYYY-MM-DD date. Empty input
// normalizes to "" when allowEmpty is true.
func NormalizeDate(raw string, allowEmpty bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("date is required")
	}
	if _, err := time.Parse(dateLayout, trimmed); err != nil {
		return "", fmt.Errorf("invalid date %q", raw)
	}
	return trimmed, nil
}
