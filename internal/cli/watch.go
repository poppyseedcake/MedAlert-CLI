package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/monitoring"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

const (
	defaultWatchPollInterval = 5 * time.Second
	minWatchPollInterval     = 50 * time.Millisecond
	maxWatchPollInterval     = 5 * time.Minute
	watchRunTimeout          = 60 * time.Second
)

// watchEvent is one JSON Lines machine event on stdout. Text logs always use
// standard error; stdout stays empty in text mode.
type watchEvent struct {
	SchemaVersion int            `json:"schema_version"`
	Command       string         `json:"command"`
	Event         string         `json:"event"`
	Timestamp     string         `json:"timestamp"`
	Data          map[string]any `json:"data"`
}

func runWatch(command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for watch", jsonOutput)
		return 2
	}
	pollInterval, err := resolveWatchPollInterval(settings)
	if err != nil {
		writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
		return 2
	}
	maxIterations, err := resolveWatchMaxIterations(settings)
	if err != nil {
		writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()

	backend := sessionStoreFor(settings)
	client := medicoverClientFor(settings)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	stoppedSignal := make(chan string, 1)
	go func() {
		select {
		case received := <-signals:
			name := "SIGTERM"
			if received == syscall.SIGINT {
				name = "SIGINT"
			}
			select {
			case stoppedSignal <- name:
			default:
			}
			logWatch(stderr, "received %s, shutting down", name)
			cancel()
		case <-ctx.Done():
		}
	}()

	watcher := &watchLoop{
		command:    command,
		storage:    storage,
		client:     client,
		backend:    backend,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		jsonOutput: jsonOutput,
	}
	emitStarted(watcher, pollInterval, maxIterations)
	logWatch(stderr, "watching enabled profiles (poll every %s)", pollInterval)

	lastRuns := map[string]time.Time{}
	paused := map[string]bool{}
	var activeMu sync.Mutex
	active := map[string]bool{}

	iteration := 0
	for {
		select {
		case <-ctx.Done():
			emitStopped(watcher, stoppedSignalName(stoppedSignal), iteration, "signal")
			logWatch(stderr, "watch stopped after %d iterations", iteration)
			return 0
		default:
		}
		if maxIterations > 0 && iteration >= maxIterations {
			emitStopped(watcher, "", iteration, "completed")
			logWatch(stderr, "watch completed %d iterations", iteration)
			return 0
		}
		iteration++
		completed := watcher.runIteration(ctx, iteration, lastRuns, paused, &activeMu, active)
		if completed {
			// runIteration returns true when its work finished without an
			// early shutdown; the loop then waits for the next poll.
			select {
			case <-ctx.Done():
				emitStopped(watcher, stoppedSignalName(stoppedSignal), iteration, "signal")
				logWatch(stderr, "watch stopped after %d iterations", iteration)
				return 0
			case <-time.After(pollInterval):
			}
		} else {
			emitStopped(watcher, stoppedSignalName(stoppedSignal), iteration, "signal")
			logWatch(stderr, "watch stopped after %d iterations", iteration)
			return 0
		}
	}
}

func stoppedSignalName(channel chan string) string {
	select {
	case name := <-channel:
		return name
	default:
		return ""
	}
}

type watchLoop struct {
	command    string
	storage    *store.Store
	client     *medicover.Client
	backend    session.Store
	stdin      *os.File
	stdout     io.Writer
	stderr     io.Writer
	jsonOutput bool
}

// runIteration reloads saved configuration, selects due profiles, and runs
// them concurrently with per-profile isolation. It returns false when the
// watch context was cancelled and the caller should stop without sleeping.
func (w *watchLoop) runIteration(ctx context.Context, iteration int, lastRuns map[string]time.Time, paused map[string]bool, activeMu *sync.Mutex, active map[string]bool) bool {
	if ctx.Err() != nil {
		return false
	}
	profiles, err := w.storage.ListProfiles("")
	if err != nil {
		logWatch(w.stderr, "cannot reload profiles: %s", shortWatchMessage(err))
		return ctx.Err() == nil
	}
	now := time.Now().UTC()
	// Drop tracking for deleted or disabled profiles so a reload takes
	// effect between planned runs without a restart.
	alive := map[string]store.Profile{}
	for _, profile := range profiles {
		if !profile.Enabled {
			delete(lastRuns, profile.ID)
			continue
		}
		alive[profile.ID] = profile
	}
	for id := range lastRuns {
		if _, ok := alive[id]; !ok {
			delete(lastRuns, id)
		}
	}
	// Backfill last-run times from durable history so a restart respects
	// intervals instead of checking everything immediately.
	for id, profile := range alive {
		if _, known := lastRuns[id]; known {
			continue
		}
		if last, ok := latestRunStart(w.storage, id); ok {
			lastRuns[id] = last
			_ = profile
		}
	}
	due := monitoring.DueProfiles(profiles, lastRuns, now)
	if len(due) == 0 {
		return ctx.Err() == nil
	}
	// Group due profiles by account so one authentication attempt serves all
	// of that account's profiles in this iteration. Paused accounts are
	// included so a later iteration can retry authentication and resume;
	// authenticateAccount emits paused/resumed only on transitions.
	byAccount := map[string][]store.Profile{}
	for _, profile := range due {
		activeMu.Lock()
		isActive := active[profile.ID]
		activeMu.Unlock()
		if isActive {
			continue
		}
		byAccount[profile.AccountID] = append(byAccount[profile.AccountID], profile)
	}
	if len(byAccount) == 0 {
		return ctx.Err() == nil
	}
	var group sync.WaitGroup
	var stateMu sync.Mutex
	for accountID, accountProfiles := range byAccount {
		if ctx.Err() != nil {
			break
		}
		account, err := w.storage.GetAccount(accountID)
		if err != nil {
			logWatch(w.stderr, "cannot load account %s: %s", accountID, shortWatchMessage(err))
			continue
		}
		token, ok := w.authenticateAccount(ctx, account, paused)
		if !ok {
			continue
		}
		for _, profile := range accountProfiles {
			if ctx.Err() != nil {
				break
			}
			activeMu.Lock()
			if active[profile.ID] {
				activeMu.Unlock()
				continue
			}
			active[profile.ID] = true
			activeMu.Unlock()
			// Refresh the profile row after authentication: another process
			// may have edited it while authentication was in flight.
			fresh, err := w.storage.GetProfile(profile.ID)
			if err != nil || !fresh.Enabled {
				activeMu.Lock()
				delete(active, profile.ID)
				activeMu.Unlock()
				continue
			}
			group.Add(1)
			go func(selected store.Profile, accountValue store.Account, accessToken string) {
				defer group.Done()
				defer func() {
					activeMu.Lock()
					delete(active, selected.ID)
					activeMu.Unlock()
				}()
				started := time.Now().UTC()
				w.checkOne(ctx, selected, accountValue, accessToken, started)
				stateMu.Lock()
				// A cancelled shutdown must not reschedule: the process is
				// leaving and the next start backfills from durable history.
				if ctx.Err() == nil {
					lastRuns[selected.ID] = started
				}
				stateMu.Unlock()
			}(fresh, account, token)
		}
	}
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		// Cancel active work in a controlled way: monitoring.Check records a
		// cancelled run without touching availability episodes, then the
		// goroutines release their in-process guards.
		group.Wait()
		return false
	case <-done:
		return ctx.Err() == nil
	}
}

// authenticateAccount returns an access token for one account, reusing the
// trusted session when possible. Authentication Required pauses only that
// account; other accounts continue. It returns false when the account must
// be skipped this iteration.
func (w *watchLoop) authenticateAccount(ctx context.Context, account store.Account, paused map[string]bool) (string, bool) {
	var saved *medicover.SessionState
	if state, err := w.backend.Load(account.ID); err == nil {
		saved = state
	} else if errors.Is(err, session.ErrNotFound) || isSessionCorrupt(err) {
		saved = nil
	} else if errors.Is(err, session.ErrUnsafe) {
		logWatch(w.stderr, "account %s session is not safe: %s", account.ID, shortWatchMessage(err))
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": "invalid_arguments", "message": shortWatchMessage(err)})
		return "", false
	} else {
		logWatch(w.stderr, "account %s session is temporarily unavailable", account.ID)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": "temporary_failure", "message": "session storage is temporarily unavailable"})
		return "", false
	}
	authCtx, cancel := context.WithTimeout(ctx, watchRunTimeout)
	defer cancel()
	auth, err := w.client.Authenticate(authCtx, medicover.AuthRequest{Session: saved})
	if err == nil {
		if saveErr := w.backend.Save(account.ID, auth.Session); saveErr != nil {
			logWatch(w.stderr, "account %s cannot save session state", account.ID)
			w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": "temporary_failure", "message": "cannot save session state"})
			return "", false
		}
		if paused[account.ID] {
			delete(paused, account.ID)
			logWatch(w.stderr, "account %s authentication recovered", account.ID)
			w.emitEvent("auth_resumed", map[string]any{"account": account.ID})
		}
		return auth.AccessToken, true
	}
	if !medicover.IsAuthRequired(err) && !medicover.IsInvalidCredentials(err) {
		// Temporary, rate-limited, and protocol failures are per-iteration
		// profile failures, not an account pause. Other accounts continue.
		code, message := watchMedicoverFailure(err)
		logWatch(w.stderr, "account %s authentication %s: %s", account.ID, code, message)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": code, "message": message})
		return "", false
	}
	password, resolveErr := secrets.Resolve(account.PasswordSource, account.PasswordRef, account.ID, w.stdin, w.stderr, true)
	if resolveErr != nil {
		pauseWatchAccount(w, paused, account, "authentication_required", "authentication is required")
		return "", false
	}
	retrySecret := medicover.Secret(password)
	password = ""
	retry, retryErr := w.client.Authenticate(authCtx, medicover.AuthRequest{
		Username: medicover.Secret(account.Username),
		Password: retrySecret,
		Session:  saved,
	})
	if retryErr != nil {
		if medicover.IsAuthRequired(retryErr) || medicover.IsInvalidCredentials(retryErr) {
			pauseWatchAccount(w, paused, account, "authentication_required", "authentication is required")
			return "", false
		}
		code, message := watchMedicoverFailure(retryErr)
		logWatch(w.stderr, "account %s authentication %s: %s", account.ID, code, message)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": code, "message": message})
		return "", false
	}
	if saveErr := w.backend.Save(account.ID, retry.Session); saveErr != nil {
		logWatch(w.stderr, "account %s cannot save session state", account.ID)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": "temporary_failure", "message": "cannot save session state"})
		return "", false
	}
	if paused[account.ID] {
		delete(paused, account.ID)
		logWatch(w.stderr, "account %s authentication recovered", account.ID)
		w.emitEvent("auth_resumed", map[string]any{"account": account.ID})
	}
	return retry.AccessToken, true
}

func pauseWatchAccount(w *watchLoop, paused map[string]bool, account store.Account, code, message string) {
	if !paused[account.ID] {
		paused[account.ID] = true
		logWatch(w.stderr, "account %s requires authentication, pausing its profiles", account.ID)
		w.emitEvent("auth_paused", map[string]any{"account": account.ID, "code": code, "message": message})
	}
}

// checkOne runs a single enabled profile with failure isolation. A failure
// never stops other profiles; SQLite leases skip duplicates without treating
// them as failures.
func (w *watchLoop) checkOne(ctx context.Context, profile store.Profile, account store.Account, accessToken string, started time.Time) {
	w.emitEvent("run_started", map[string]any{"account": account.ID, "profile": profile.ID})
	logWatch(w.stderr, "checking profile %s (account %s)", profile.ID, account.ID)
	runCtx, cancel := context.WithTimeout(ctx, watchRunTimeout)
	defer cancel()
	result, err := monitoring.Check(runCtx, w.storage, profile, account, w.client, accessToken, started)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrObservationRunActive):
			logWatch(w.stderr, "profile %s skipped: observation run is already active", profile.ID)
			w.emitEvent("run_skipped", map[string]any{"account": account.ID, "profile": profile.ID, "code": "run_active", "message": "observation run is already active"})
			return
		case errors.Is(err, store.ErrObservationRunStale):
			logWatch(w.stderr, "profile %s skipped: %s", profile.ID, shortWatchMessage(err))
			w.emitEvent("run_failed", map[string]any{"account": account.ID, "profile": profile.ID, "code": "stale_result", "message": shortWatchMessage(err)})
			return
		case errors.Is(err, store.ErrObservationRunConflicting):
			logWatch(w.stderr, "profile %s conflicting result: %s", profile.ID, shortWatchMessage(err))
			w.emitEvent("run_failed", map[string]any{"account": account.ID, "profile": profile.ID, "code": "conflicting_result", "message": shortWatchMessage(err)})
			return
		case errors.Is(err, store.ErrProfileDisabled):
			logWatch(w.stderr, "profile %s is disabled, skipping", profile.ID)
			return
		}
		code, message := watchMedicoverFailure(err)
		// A shutdown race reports cancelled, never a failure: the run was
		// recorded without touching availability episodes.
		if ctx.Err() != nil && code == "cancelled" {
			logWatch(w.stderr, "profile %s cancelled during shutdown", profile.ID)
			w.emitEvent("run_failed", map[string]any{"account": account.ID, "profile": profile.ID, "code": code, "message": message})
			return
		}
		logWatch(w.stderr, "profile %s check %s: %s", profile.ID, code, message)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "profile": profile.ID, "code": code, "message": message})
		return
	}
	logWatch(w.stderr, "profile %s completed: %d slots, %d newly available, %d ended", profile.ID, len(result.Search.Slots), len(result.Reconciliation.NewEpisodes), len(result.Reconciliation.EndedEpisodes))
	w.emitEvent("run_completed", map[string]any{
		"account":              account.ID,
		"profile":              profile.ID,
		"run":                  result.Reconciliation.Run.ID,
		"slot_count":           len(result.Search.Slots),
		"newly_available":      len(result.Reconciliation.NewEpisodes),
		"ended":                len(result.Reconciliation.EndedEpisodes),
		"active_episode_count": countActiveEpisodes(w.storage, profile.ID),
	})
}

func countActiveEpisodes(storage *store.Store, profileID string) int {
	episodes, err := storage.ListAvailabilityEpisodes(profileID)
	if err != nil {
		return 0
	}
	active := 0
	for _, episode := range episodes {
		if episode.Active {
			active++
		}
	}
	return active
}

func latestRunStart(storage *store.Store, profileID string) (time.Time, bool) {
	runs, err := storage.ListObservationRuns(profileID)
	if err != nil || len(runs) == 0 {
		return time.Time{}, false
	}
	latest := time.Time{}
	for _, run := range runs {
		parsed, err := time.Parse(time.RFC3339Nano, run.StartedAt)
		if err != nil {
			continue
		}
		if parsed.After(latest) {
			latest = parsed
		}
	}
	if latest.IsZero() {
		return time.Time{}, false
	}
	return latest, true
}

func watchMedicoverFailure(err error) (string, string) {
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) {
		switch medicoverErr.Code {
		case medicover.CodeAuthRequired:
			return "authentication_required", shortWatchMessage(err)
		case medicover.CodeMFARequired:
			return "mfa_required", shortWatchMessage(err)
		case medicover.CodeInvalidCredentials:
			return "invalid_credentials", shortWatchMessage(err)
		case medicover.CodeRateLimited:
			return "rate_limited", shortWatchMessage(err)
		case medicover.CodeTemporary:
			return "temporary_failure", shortWatchMessage(err)
		case medicover.CodeProtocolChanged:
			return "protocol_changed", shortWatchMessage(err)
		case medicover.CodeCancelled:
			return "cancelled", shortWatchMessage(err)
		case medicover.CodePartial:
			return "partial_result", shortWatchMessage(err)
		case medicover.CodeConflicting:
			return "conflicting_result", shortWatchMessage(err)
		case medicover.CodeStale:
			return "stale_result", shortWatchMessage(err)
		}
	}
	if errors.Is(err, secrets.ErrMissingInput) {
		return "missing_input", shortWatchMessage(err)
	}
	return "temporary_failure", shortWatchMessage(err)
}

func shortWatchMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	// Medicover messages are already sanitized; store messages never contain
	// secrets. Truncate only to keep logs and JSON Lines stable.
	if len(message) > 500 {
		return message[:500]
	}
	if message == "" {
		return "observation failed"
	}
	return message
}

func (w *watchLoop) emitEvent(event string, data map[string]any) {
	if !w.jsonOutput {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	writeJSON(w.stdout, watchEvent{
		SchemaVersion: resultSchemaVersion,
		Command:       "watch",
		Event:         event,
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		Data:          data,
	})
}

func emitStarted(w *watchLoop, pollInterval time.Duration, maxIterations int) {
	w.emitEvent("started", map[string]any{"poll_interval_ms": pollInterval.Milliseconds(), "max_iterations": maxIterations})
}

func emitStopped(w *watchLoop, signalName string, iterations int, reason string) {
	data := map[string]any{"iterations": iterations, "reason": reason}
	if signalName != "" {
		data["signal"] = signalName
	}
	w.emitEvent("stopped", data)
}

func logWatch(stderr io.Writer, format string, args ...any) {
	stamp := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(stderr, "%s watch: %s\n", stamp, fmt.Sprintf(format, args...))
}

func resolveWatchPollInterval(settings options) (time.Duration, error) {
	raw := strings.TrimSpace(settings.pollIntervalRaw)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("MEDALERT_WATCH_POLL_INTERVAL"))
	}
	if raw == "" {
		return defaultWatchPollInterval, nil
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("poll interval must be a duration like 500ms, 5s, or 1m")
	}
	if interval < minWatchPollInterval || interval > maxWatchPollInterval {
		return 0, fmt.Errorf("poll interval must be between %s and %s", minWatchPollInterval, maxWatchPollInterval)
	}
	return interval, nil
}

func resolveWatchMaxIterations(settings options) (int, error) {
	if settings.watchOnce {
		return 1, nil
	}
	raw := strings.TrimSpace(settings.maxIterationsRaw)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("MEDALERT_WATCH_MAX_ITERATIONS"))
	}
	if raw == "" {
		return 0, nil
	}
	count, err := strconv.Atoi(raw)
	if err != nil || count < 0 || count > 1000000 {
		return 0, fmt.Errorf("max iterations must be 0..1000000")
	}
	return count, nil
}

// checkWatchFlags rejects every flag watch does not consume so typos fail
// instead of being silently ignored. Watch always runs non-interactively and
// covers every enabled profile, so account, profile, criteria, secret, and
// dry flags are never valid here.
func checkWatchFlags(settings options) error {
	switch {
	case settings.accountID != "":
		return fmt.Errorf("--account is not supported for watch")
	case settings.username != "":
		return fmt.Errorf("--username is not supported for watch")
	case settings.passwordFile != "":
		return fmt.Errorf("--password-file is not supported for watch")
	case settings.passwordPrompt:
		return fmt.Errorf("--password-prompt is not supported for watch")
	case settings.noStoredPassword:
		return fmt.Errorf("--no-stored-password is not supported for watch")
	case settings.mfaCodeFile != "":
		return fmt.Errorf("--mfa-code-file is not supported for watch")
	case settings.forgetSecret:
		return fmt.Errorf("--forget-secret is not supported for watch")
	case settings.profileID != "":
		return fmt.Errorf("--profile is not supported for watch")
	case settings.region != "":
		return fmt.Errorf("--region is not supported for watch")
	case settings.specialty != "":
		return fmt.Errorf("--specialty is not supported for watch")
	case settings.clinic != "":
		return fmt.Errorf("--clinic is not supported for watch")
	case settings.doctor != "":
		return fmt.Errorf("--doctor is not supported for watch")
	case settings.language != "":
		return fmt.Errorf("--language is not supported for watch")
	case settings.visitType != "":
		return fmt.Errorf("--visit-type is not supported for watch")
	case settings.searchType != "":
		return fmt.Errorf("--search-type is not supported for watch")
	case settings.startDate != "":
		return fmt.Errorf("--start-date is not supported for watch")
	case settings.endDate != "":
		return fmt.Errorf("--end-date is not supported for watch")
	case settings.checkIntervalRaw != "":
		return fmt.Errorf("--check-interval-minutes is not supported for watch")
	case settings.profileDisabled:
		return fmt.Errorf("--disabled is not supported for watch")
	case settings.clearClinic:
		return fmt.Errorf("--clear-clinic is not supported for watch")
	case settings.clearDoctor:
		return fmt.Errorf("--clear-doctor is not supported for watch")
	case settings.clearLanguage:
		return fmt.Errorf("--clear-language is not supported for watch")
	case settings.clearVisitType:
		return fmt.Errorf("--clear-visit-type is not supported for watch")
	case settings.clearSearchType:
		return fmt.Errorf("--clear-search-type is not supported for watch")
	case settings.clearStartDate:
		return fmt.Errorf("--clear-start-date is not supported for watch")
	case settings.clearEndDate:
		return fmt.Errorf("--clear-end-date is not supported for watch")
	case settings.dry:
		return fmt.Errorf("--dry is not supported for watch")
	}
	return nil
}
