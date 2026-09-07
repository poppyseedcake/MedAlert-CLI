package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/poppyseedcake/MedAlert/internal/application"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

func runProfile(command string, settings options, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	switch command {
	case "profile list":
		return profileList(command, settings, stdout, stderr, jsonOutput)
	case "profile create":
		return profileCreate(command, settings, stdout, stderr, jsonOutput)
	case "profile show":
		return profileShow(command, settings, stdout, stderr, jsonOutput)
	case "profile edit":
		return profileEdit(command, settings, stdout, stderr, jsonOutput)
	case "profile enable":
		return profileSetEnabled(command, settings, true, stdout, stderr, jsonOutput)
	case "profile disable":
		return profileSetEnabled(command, settings, false, stdout, stderr, jsonOutput)
	case "profile delete":
		return profileDelete(command, settings, stdout, stderr, jsonOutput)
	default:
		writeError(stderr, command, "invalid_arguments", "a supported profile command is required", jsonOutput)
		return 2
	}
}

func resolveProfileID(settings options) string {
	if strings.TrimSpace(settings.profileID) != "" {
		return strings.TrimSpace(settings.profileID)
	}
	if len(settings.positionals) > 0 {
		return strings.TrimSpace(settings.positionals[0])
	}
	return ""
}

// checkProfileIDPositionals validates the --profile/positional combination.
// needsID requires exactly one profile id; list mode (needsID=false) rejects
// any positional.
func checkProfileIDPositionals(command string, settings options, needsID bool, stderr io.Writer, jsonOutput bool) (string, bool) {
	if strings.TrimSpace(settings.profileID) != "" && len(settings.positionals) > 0 && strings.TrimSpace(settings.positionals[0]) != strings.TrimSpace(settings.profileID) {
		writeError(stderr, command, "invalid_arguments", "use either --profile or a positional id, not both", jsonOutput)
		return "", false
	}
	if needsID {
		if len(settings.positionals) > 1 {
			writeError(stderr, command, "invalid_arguments", "too many arguments: expected at most one profile id", jsonOutput)
			return "", false
		}
	} else {
		if len(settings.positionals) > 0 {
			writeError(stderr, command, "invalid_arguments", "too many arguments for profile list", jsonOutput)
			return "", false
		}
	}
	return resolveProfileID(settings), true
}

func profileList(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	if _, ok := checkProfileIDPositionals(command, settings, false, stderr, jsonOutput); !ok {
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	profiles, err := storage.ListProfiles(strings.TrimSpace(settings.accountID))
	if err != nil {
		return reportProfileError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"profiles": profiles})
		return 0
	}
	if len(profiles) == 0 {
		fmt.Fprintln(stdout, "No profiles found.")
		return 0
	}
	for _, profile := range profiles {
		fmt.Fprintln(stdout, formatProfileLine(profile))
	}
	return 0
}

func profileCreate(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkProfileIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "profile id is required (use --profile or a positional id)", jsonOutput)
		return 2
	}
	accountID := strings.TrimSpace(settings.accountID)
	if accountID == "" {
		writeError(stderr, command, "invalid_arguments", "account id is required (use --account)", jsonOutput)
		return 2
	}
	if strings.TrimSpace(settings.region) == "" {
		writeError(stderr, command, "invalid_arguments", "region is required (use --region)", jsonOutput)
		return 2
	}
	if strings.TrimSpace(settings.specialty) == "" {
		writeError(stderr, command, "invalid_arguments", "specialty is required (use --specialty)", jsonOutput)
		return 2
	}
	if strings.TrimSpace(settings.checkIntervalRaw) == "" {
		writeError(stderr, command, "invalid_arguments", "check interval is required (use --check-interval-minutes)", jsonOutput)
		return 2
	}
	if strings.TrimSpace(settings.telegramIDsRaw) == "" {
		profile, err := applicationProfile(context.Background(), settings, id, "create")
		if err != nil {
			return reportApplicationProfileError(stderr, command, err, jsonOutput)
		}
		return writeApplicationProfile(stdout, stderr, settings, command, profile, fmt.Sprintf("Created profile %s for account %s.\n", profile.ID, profile.AccountID), jsonOutput)
	}
	profile, err := buildProfileFromOptions(id, accountID, settings)
	if err != nil {
		writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
		return 2
	}
	profile.Enabled = !settings.profileDisabled
	if err := store.ValidateProfile(profile); err != nil {
		return reportProfileError(stderr, command, err, jsonOutput)
	}
	var telegramIDs []string
	if strings.TrimSpace(settings.telegramIDsRaw) != "" {
		parsed, err := store.ParseDestinationIDList(settings.telegramIDsRaw)
		if err != nil {
			return reportTelegramError(stderr, command, err, jsonOutput)
		}
		telegramIDs = parsed
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	// Create the profile and its links in one transaction so a missing
	// destination never leaves a partial orphan row behind.
	created, err := storage.CreateProfileWithDestinations(profile, telegramIDs)
	if err != nil {
		// Map destination errors to the telegram domain; profile errors stay
		// in the profile domain for stable automation codes.
		if errors.Is(err, store.ErrDestinationNotFound) || errors.Is(err, store.ErrDestinationInvalid) {
			return reportTelegramError(stderr, command, err, jsonOutput)
		}
		return reportProfileError(stderr, command, err, jsonOutput)
	}
	if err := writeProfileWithDestinations(stdout, command, storage, created, fmt.Sprintf("Created profile %s for account %s.\n", created.ID, created.AccountID), jsonOutput); err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	return 0
}

func profileShow(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkProfileIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "profile id is required (use --profile or a positional id)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	profile, err := storage.GetProfile(strings.TrimSpace(id))
	if err != nil {
		return reportProfileError(stderr, command, err, jsonOutput)
	}
	if err := writeProfileWithDestinations(stdout, command, storage, profile, "", jsonOutput); err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	return 0
}

func profileEdit(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkProfileIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "profile id is required (use --profile or a positional id)", jsonOutput)
		return 2
	}
	if strings.TrimSpace(settings.telegramIDsRaw) != "" && settings.clearTelegram {
		writeError(stderr, command, "invalid_arguments", "use either --telegram or --clear-telegram, not both", jsonOutput)
		return 2
	}
	var telegramIDs []string
	hasTelegramChange := false
	if strings.TrimSpace(settings.telegramIDsRaw) != "" {
		parsed, err := store.ParseDestinationIDList(settings.telegramIDsRaw)
		if err != nil {
			return reportTelegramError(stderr, command, err, jsonOutput)
		}
		telegramIDs = parsed
		hasTelegramChange = true
	} else if settings.clearTelegram {
		telegramIDs = []string{}
		hasTelegramChange = true
	}
	if !hasTelegramChange {
		updated, err := applicationProfile(context.Background(), settings, strings.TrimSpace(id), "edit")
		if err != nil {
			return reportApplicationProfileError(stderr, command, err, jsonOutput)
		}
		return writeApplicationProfile(stdout, stderr, settings, command, updated, fmt.Sprintf("Updated profile %s for account %s.\n", updated.ID, updated.AccountID), jsonOutput)
	}
	update, hasChange, err := buildProfileUpdateFromOptions(settings)
	if err != nil {
		writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
		return 2
	}
	if !hasChange && !hasTelegramChange {
		writeError(stderr, command, "invalid_arguments", "no profile changes requested (use --region, --specialty, --clinic, --doctor, --language, --visit-type, --search-type, --start-date, --end-date, --check-interval-minutes, --telegram, or a --clear-* flag)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	// Fetch first so a missing profile reports profile_not_found before any
	// validation of the combined row. Editing keeps the stable identity,
	// the account, and the enabled state. Criteria and links commit together
	// so a concurrent destination delete never leaves criteria updated
	// without the requested links.
	if _, err := storage.GetProfile(strings.TrimSpace(id)); err != nil {
		return reportProfileError(stderr, command, err, jsonOutput)
	}
	updated, err := storage.UpdateProfileWithDestinations(strings.TrimSpace(id), update, hasChange, telegramIDs, hasTelegramChange)
	if err != nil {
		if errors.Is(err, store.ErrDestinationNotFound) || errors.Is(err, store.ErrDestinationInvalid) {
			return reportTelegramError(stderr, command, err, jsonOutput)
		}
		return reportProfileError(stderr, command, err, jsonOutput)
	}
	if err := writeProfileWithDestinations(stdout, command, storage, updated, fmt.Sprintf("Updated profile %s for account %s.\n", updated.ID, updated.AccountID), jsonOutput); err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	return 0
}

func profileSetEnabled(command string, settings options, enabled bool, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkProfileIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "profile id is required (use --profile or a positional id)", jsonOutput)
		return 2
	}
	action := "disable"
	if enabled {
		action = "enable"
	}
	updated, err := applicationProfile(context.Background(), settings, strings.TrimSpace(id), action)
	if err != nil {
		return reportApplicationProfileError(stderr, command, err, jsonOutput)
	}
	verb := "Enabled"
	if !enabled {
		verb = "Disabled"
	}
	return writeApplicationProfile(stdout, stderr, settings, command, updated, fmt.Sprintf("%s profile %s for account %s.\n", verb, updated.ID, updated.AccountID), jsonOutput)
}

func profileDelete(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkProfileIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "profile id is required (use --profile or a positional id)", jsonOutput)
		return 2
	}
	current, err := applicationProfile(context.Background(), settings, strings.TrimSpace(id), "delete")
	if err != nil {
		return reportApplicationProfileError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"deleted": current.ID, "account": current.AccountID})
		return 0
	}
	fmt.Fprintf(stdout, "Deleted profile %s for account %s.\n", current.ID, current.AccountID)
	return 0
}

// buildProfileFromOptions normalizes create flags into a Profile. It returns a
// plain error for invalid_arguments so callers keep one error code path.
func buildProfileFromOptions(id, accountID string, settings options) (store.Profile, error) {
	region, err := store.NormalizeIDList(settings.region)
	if err != nil || region == "" {
		return store.Profile{}, fmt.Errorf("region must be comma-separated Medicover IDs")
	}
	specialty, err := store.NormalizeIDList(settings.specialty)
	if err != nil || specialty == "" {
		return store.Profile{}, fmt.Errorf("specialty must be comma-separated Medicover IDs")
	}
	clinic, err := store.NormalizeIDList(settings.clinic)
	if err != nil {
		return store.Profile{}, fmt.Errorf("clinic IDs must be comma-separated positive integers")
	}
	doctor, err := store.NormalizeIDList(settings.doctor)
	if err != nil {
		return store.Profile{}, fmt.Errorf("doctor IDs must be comma-separated positive integers")
	}
	language, err := store.NormalizeIDList(settings.language)
	if err != nil {
		return store.Profile{}, fmt.Errorf("language IDs must be comma-separated positive integers")
	}
	searchType, err := store.NormalizeSearchType(settings.searchType)
	if err != nil {
		return store.Profile{}, fmt.Errorf("search type must be Standard or DiagnosticProcedure")
	}
	visitType := strings.TrimSpace(settings.visitType)
	startDate, err := store.NormalizeDate(settings.startDate, true)
	if err != nil {
		return store.Profile{}, fmt.Errorf("start date must be YYYY-MM-DD")
	}
	endDate, err := store.NormalizeDate(settings.endDate, true)
	if err != nil {
		return store.Profile{}, fmt.Errorf("end date must be YYYY-MM-DD")
	}
	if startDate != "" && endDate != "" && endDate < startDate {
		return store.Profile{}, fmt.Errorf("end date must not be before start date")
	}
	interval, err := parseCheckInterval(settings.checkIntervalRaw)
	if err != nil {
		return store.Profile{}, err
	}
	return store.Profile{
		ID:                   strings.TrimSpace(id),
		AccountID:            strings.TrimSpace(accountID),
		RegionIDs:            region,
		SpecialtyIDs:         specialty,
		ClinicIDs:            clinic,
		DoctorIDs:            doctor,
		LanguageIDs:          language,
		VisitType:            visitType,
		SearchType:           searchType,
		StartDate:            startDate,
		EndDate:              endDate,
		CheckIntervalMinutes: interval,
		Enabled:              true,
	}, nil
}

// buildProfileUpdateFromOptions normalizes edit flags into a ProfileUpdate.
// Clear flags reset an optional field to its empty (any) form, except search
// type which resets to the default Standard.
func buildProfileUpdateFromOptions(settings options) (store.ProfileUpdate, bool, error) {
	var update store.ProfileUpdate
	hasChange := false
	if strings.TrimSpace(settings.region) != "" {
		normalized, err := store.NormalizeIDList(settings.region)
		if err != nil || normalized == "" {
			return update, false, fmt.Errorf("region must be comma-separated Medicover IDs")
		}
		update.RegionIDs = &normalized
		hasChange = true
	}
	if strings.TrimSpace(settings.specialty) != "" {
		normalized, err := store.NormalizeIDList(settings.specialty)
		if err != nil || normalized == "" {
			return update, false, fmt.Errorf("specialty must be comma-separated Medicover IDs")
		}
		update.SpecialtyIDs = &normalized
		hasChange = true
	}
	if strings.TrimSpace(settings.clinic) != "" && settings.clearClinic {
		return update, false, fmt.Errorf("use either --clinic or --clear-clinic, not both")
	}
	if strings.TrimSpace(settings.clinic) != "" {
		normalized, err := store.NormalizeIDList(settings.clinic)
		if err != nil {
			return update, false, fmt.Errorf("clinic IDs must be comma-separated positive integers")
		}
		update.ClinicIDs = &normalized
		hasChange = true
	} else if settings.clearClinic {
		empty := ""
		update.ClinicIDs = &empty
		hasChange = true
	}
	if strings.TrimSpace(settings.doctor) != "" && settings.clearDoctor {
		return update, false, fmt.Errorf("use either --doctor or --clear-doctor, not both")
	}
	if strings.TrimSpace(settings.doctor) != "" {
		normalized, err := store.NormalizeIDList(settings.doctor)
		if err != nil {
			return update, false, fmt.Errorf("doctor IDs must be comma-separated positive integers")
		}
		update.DoctorIDs = &normalized
		hasChange = true
	} else if settings.clearDoctor {
		empty := ""
		update.DoctorIDs = &empty
		hasChange = true
	}
	if strings.TrimSpace(settings.language) != "" && settings.clearLanguage {
		return update, false, fmt.Errorf("use either --language or --clear-language, not both")
	}
	if strings.TrimSpace(settings.language) != "" {
		normalized, err := store.NormalizeIDList(settings.language)
		if err != nil {
			return update, false, fmt.Errorf("language IDs must be comma-separated positive integers")
		}
		update.LanguageIDs = &normalized
		hasChange = true
	} else if settings.clearLanguage {
		empty := ""
		update.LanguageIDs = &empty
		hasChange = true
	}
	if strings.TrimSpace(settings.visitType) != "" && settings.clearVisitType {
		return update, false, fmt.Errorf("use either --visit-type or --clear-visit-type, not both")
	}
	if strings.TrimSpace(settings.visitType) != "" {
		update.VisitType = ptr(strings.TrimSpace(settings.visitType))
		hasChange = true
	} else if settings.clearVisitType {
		empty := ""
		update.VisitType = &empty
		hasChange = true
	}
	if strings.TrimSpace(settings.searchType) != "" && settings.clearSearchType {
		return update, false, fmt.Errorf("use either --search-type or --clear-search-type, not both")
	}
	if strings.TrimSpace(settings.searchType) != "" {
		normalized, err := store.NormalizeSearchType(settings.searchType)
		if err != nil {
			return update, false, fmt.Errorf("search type must be Standard or DiagnosticProcedure")
		}
		update.SearchType = &normalized
		hasChange = true
	} else if settings.clearSearchType {
		update.SearchType = ptr(store.SearchTypeStandard)
		hasChange = true
	}
	if strings.TrimSpace(settings.startDate) != "" && settings.clearStartDate {
		return update, false, fmt.Errorf("use either --start-date or --clear-start-date, not both")
	}
	if strings.TrimSpace(settings.startDate) != "" {
		normalized, err := store.NormalizeDate(settings.startDate, true)
		if err != nil || normalized == "" {
			return update, false, fmt.Errorf("start date must be YYYY-MM-DD")
		}
		update.StartDate = &normalized
		hasChange = true
	} else if settings.clearStartDate {
		empty := ""
		update.StartDate = &empty
		hasChange = true
	}
	if strings.TrimSpace(settings.endDate) != "" && settings.clearEndDate {
		return update, false, fmt.Errorf("use either --end-date or --clear-end-date, not both")
	}
	if strings.TrimSpace(settings.endDate) != "" {
		normalized, err := store.NormalizeDate(settings.endDate, true)
		if err != nil || normalized == "" {
			return update, false, fmt.Errorf("end date must be YYYY-MM-DD")
		}
		update.EndDate = &normalized
		hasChange = true
	} else if settings.clearEndDate {
		empty := ""
		update.EndDate = &empty
		hasChange = true
	}
	if strings.TrimSpace(settings.checkIntervalRaw) != "" {
		interval, err := parseCheckInterval(settings.checkIntervalRaw)
		if err != nil {
			return update, false, err
		}
		update.CheckIntervalMinutes = &interval
		hasChange = true
	}
	return update, hasChange, nil
}

func parseCheckInterval(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	number, err := strconv.Atoi(trimmed)
	if err != nil || number < 1 || number > 43200 {
		return 0, fmt.Errorf("check interval must be 1..43200 minutes")
	}
	return number, nil
}

func ptr(value string) *string { return &value }

func applicationProfile(ctx context.Context, settings options, id, action string) (store.Profile, error) {
	return application.New(application.Config{
		Database:         settings.database,
		SessionDir:       settings.sessionDir,
		MedicoverBaseURL: settings.medicoverBaseURL,
		TelegramBaseURL:  settings.telegramBaseURL,
	}).Profile(ctx, application.ProfileRequest{
		Action: action,
		ID:     id,
		Values: application.ProfileValues{
			AccountID: settings.accountID, RegionIDs: settings.region, SpecialtyIDs: settings.specialty,
			ClinicIDs: settings.clinic, DoctorIDs: settings.doctor, LanguageIDs: settings.language,
			VisitType: settings.visitType, SearchType: settings.searchType, StartDate: settings.startDate,
			EndDate: settings.endDate, CheckIntervalMinutes: settings.checkIntervalRaw,
			Enabled: !settings.profileDisabled,
		},
		Clear: profileClearFields(settings),
	})
}

func profileClearFields(settings options) []string {
	fields := []string{}
	if settings.clearClinic {
		fields = append(fields, "clinic_ids")
	}
	if settings.clearDoctor {
		fields = append(fields, "doctor_ids")
	}
	if settings.clearLanguage {
		fields = append(fields, "language_ids")
	}
	if settings.clearVisitType {
		fields = append(fields, "visit_type")
	}
	if settings.clearSearchType {
		fields = append(fields, "search_type")
	}
	if settings.clearStartDate {
		fields = append(fields, "start_date")
	}
	if settings.clearEndDate {
		fields = append(fields, "end_date")
	}
	return fields
}

func reportApplicationProfileError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	code, detail := application.ErrorInfo(err)
	switch code {
	case "account_not_found":
		return reportAccountError(stderr, command, err, jsonOutput)
	case "profile_exists", "profile_not_found":
		return reportProfileError(stderr, command, err, jsonOutput)
	case "invalid_arguments":
		writeError(stderr, command, code, detail, jsonOutput)
		return 2
	default:
		return reportStoreError(stderr, command, err, jsonOutput)
	}
}

func writeApplicationProfile(stdout, stderr io.Writer, settings options, command string, profile store.Profile, textTemplate string, jsonOutput bool) int {
	// The application owns the mutation. This adapter only reads the safe
	// profile and its non-secret destination ids for the established output.
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	if err := writeProfileWithDestinations(stdout, command, storage, profile, textTemplate, jsonOutput); err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	return 0
}

func writeProfile(stdout io.Writer, command string, profile store.Profile, textTemplate string, jsonOutput bool) {
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"profile": profile})
		return
	}
	if textTemplate != "" {
		fmt.Fprint(stdout, textTemplate)
	}
	fmt.Fprintf(stdout, "ID: %s\n", profile.ID)
	fmt.Fprintf(stdout, "Account: %s\n", profile.AccountID)
	fmt.Fprintf(stdout, "Enabled: %s\n", formatEnabled(profile.Enabled))
	fmt.Fprintf(stdout, "Region: %s\n", orAny(profile.RegionIDs))
	fmt.Fprintf(stdout, "Specialty: %s\n", orAny(profile.SpecialtyIDs))
	fmt.Fprintf(stdout, "Clinic: %s\n", orAny(profile.ClinicIDs))
	fmt.Fprintf(stdout, "Doctor: %s\n", orAny(profile.DoctorIDs))
	fmt.Fprintf(stdout, "Language: %s\n", orAny(profile.LanguageIDs))
	fmt.Fprintf(stdout, "Visit type: %s\n", orAny(profile.VisitType))
	fmt.Fprintf(stdout, "Search type: %s\n", orAny(profile.SearchType))
	fmt.Fprintf(stdout, "Start date: %s\n", orAny(profile.StartDate))
	fmt.Fprintf(stdout, "End date: %s\n", orAny(profile.EndDate))
	fmt.Fprintf(stdout, "Check interval: %d minutes\n", profile.CheckIntervalMinutes)
}

// writeProfileWithDestinations renders a profile together with its linked
// Telegram Destinations. JSON keeps the stable profile object and adds the
// destination id list so automation can process links without parsing text.
// A destination read failure is returned so callers surface database errors
// instead of reporting an incorrect empty link list.
func writeProfileWithDestinations(stdout io.Writer, command string, storage *store.Store, profile store.Profile, textTemplate string, jsonOutput bool) error {
	destinationIDs := []string{}
	if storage != nil {
		ids, err := storage.ListProfileDestinationIDs(profile.ID)
		if err != nil {
			return err
		}
		destinationIDs = ids
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"profile": profile, "telegram_destinations": destinationIDs})
		return nil
	}
	if textTemplate != "" {
		fmt.Fprint(stdout, textTemplate)
	}
	fmt.Fprintf(stdout, "ID: %s\n", profile.ID)
	fmt.Fprintf(stdout, "Account: %s\n", profile.AccountID)
	fmt.Fprintf(stdout, "Enabled: %s\n", formatEnabled(profile.Enabled))
	fmt.Fprintf(stdout, "Region: %s\n", orAny(profile.RegionIDs))
	fmt.Fprintf(stdout, "Specialty: %s\n", orAny(profile.SpecialtyIDs))
	fmt.Fprintf(stdout, "Clinic: %s\n", orAny(profile.ClinicIDs))
	fmt.Fprintf(stdout, "Doctor: %s\n", orAny(profile.DoctorIDs))
	fmt.Fprintf(stdout, "Language: %s\n", orAny(profile.LanguageIDs))
	fmt.Fprintf(stdout, "Visit type: %s\n", orAny(profile.VisitType))
	fmt.Fprintf(stdout, "Search type: %s\n", orAny(profile.SearchType))
	fmt.Fprintf(stdout, "Start date: %s\n", orAny(profile.StartDate))
	fmt.Fprintf(stdout, "End date: %s\n", orAny(profile.EndDate))
	fmt.Fprintf(stdout, "Check interval: %d minutes\n", profile.CheckIntervalMinutes)
	if len(destinationIDs) == 0 {
		fmt.Fprintf(stdout, "Telegram: %s\n", "none")
	} else {
		fmt.Fprintf(stdout, "Telegram: %s\n", strings.Join(destinationIDs, ","))
	}
	return nil
}

func formatProfileLine(profile store.Profile) string {
	return fmt.Sprintf("%s %s %s region:%s specialty:%s interval:%dm",
		profile.ID, profile.AccountID, formatEnabled(profile.Enabled),
		orAny(profile.RegionIDs), orAny(profile.SpecialtyIDs), profile.CheckIntervalMinutes)
}

func formatEnabled(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func orAny(value string) string {
	if strings.TrimSpace(value) == "" {
		return "any"
	}
	return value
}
