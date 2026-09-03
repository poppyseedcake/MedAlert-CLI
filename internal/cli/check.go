package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/session"
)

func runCheck(command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	if !settings.dry {
		writeError(stderr, command, "invalid_arguments", "check currently requires --dry", jsonOutput)
		return 2
	}
	if len(settings.positionals) > 1 || (settings.profileID != "" && len(settings.positionals) > 0 && settings.positionals[0] != settings.profileID) {
		writeError(stderr, command, "invalid_arguments", "use either --profile or one positional profile id", jsonOutput)
		return 2
	}
	id := settings.profileID
	if id == "" && len(settings.positionals) == 1 {
		id = settings.positionals[0]
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "profile id is required", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	profile, err := storage.GetProfile(id)
	if err != nil {
		return reportProfileError(stderr, command, err, jsonOutput)
	}
	account, err := storage.GetAccount(profile.AccountID)
	if err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	backend := sessionStoreFor(settings)
	var saved *medicover.SessionState
	if state, loadErr := backend.Load(account.ID); loadErr == nil {
		saved = state
	} else if !errors.Is(loadErr, session.ErrNotFound) && !isSessionCorrupt(loadErr) {
		writeError(stderr, command, "temporary_failure", "session storage is temporarily unavailable", jsonOutput)
		return 4
	}
	client := medicoverClientFor(settings)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	auth, authErr := client.Authenticate(ctx, medicover.AuthRequest{Session: saved})
	if authErr != nil && medicover.IsAuthRequired(authErr) {
		password, resolveErr := secrets.Resolve(account.PasswordSource, account.PasswordRef, account.ID, stdin, stderr, true)
		if resolveErr != nil {
			return reportSecretError(stderr, command, resolveErr, jsonOutput)
		}
		auth, authErr = client.Authenticate(ctx, medicover.AuthRequest{Username: medicover.Secret(account.Username), Password: medicover.Secret(password), Session: saved})
		password = ""
	}
	if authErr != nil {
		return reportMedicoverError(stderr, command, authErr, jsonOutput)
	}
	if err := backend.Save(account.ID, auth.Session); err != nil {
		writeError(stderr, command, "temporary_failure", "cannot save session state", jsonOutput)
		return 4
	}
	result, err := client.Search(ctx, auth.AccessToken, medicover.SearchCriteria{RegionIDs: profile.RegionIDs, SpecialtyIDs: profile.SpecialtyIDs, ClinicIDs: profile.ClinicIDs, DoctorIDs: profile.DoctorIDs, LanguageIDs: profile.LanguageIDs, VisitType: profile.VisitType, SearchType: profile.SearchType, StartDate: profile.StartDate, EndDate: profile.EndDate})
	if err != nil {
		return reportMedicoverError(stderr, command, err, jsonOutput)
	}
	data := map[string]any{"account": account.ID, "profile": profile.ID, "dry": true, "complete": true, "slots": result.Slots, "slot_count": len(result.Slots), "pages": result.Pages}
	if jsonOutput {
		writeResult(stdout, command, data)
		return 0
	}
	fmt.Fprintf(stdout, "Dry check for profile %s (account %s) found %d available slots.\n", profile.ID, account.ID, len(result.Slots))
	for _, slot := range result.Slots {
		fmt.Fprintf(stdout, "%s  %s  %s  %s\n", slot.Time, slot.Doctor, slot.Clinic, slot.Identity)
	}
	return 0
}
