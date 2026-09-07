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
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

const (
	defaultWatchPollInterval = 5 * time.Second
	minWatchPollInterval     = 50 * time.Millisecond
	maxWatchPollInterval     = 5 * time.Minute
	watchRunTimeout          = 60 * time.Second
	// authFailureBackoff throttles authentication retries per account so a
	// profile with a long check interval does not hammer Medicover every
	// poll when login keeps failing with a temporary error.
	authFailureBackoff = 60 * time.Second
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

	watcher := &watchLoop{
		command:         command,
		storage:         storage,
		client:          client,
		backend:         backend,
		stdin:           stdin,
		stdout:          stdout,
		stderr:          stderr,
		jsonOutput:      jsonOutput,
		telegramBaseURL: settings.telegramBaseURL,
		lastRuns:        map[string]time.Time{},
		paused:          map[string]bool{},
		active:          map[string]bool{},
		authActive:      map[string]bool{},
		authRetryAt:     map[string]time.Time{},
	}
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
			watcher.log("received %s, shutting down", name)
			cancel()
		case <-ctx.Done():
		}
	}()

	watcher.emitEvent("started", map[string]any{"poll_interval_ms": pollInterval.Milliseconds(), "max_iterations": maxIterations})
	watcher.log("watching enabled profiles (poll every %s)", pollInterval)

	iteration := 0
	for {
		select {
		case <-ctx.Done():
			watcher.waitForBackground()
			watcher.emitEvent("stopped", stoppedData(stoppedSignalName(stoppedSignal), iteration, "signal"))
			watcher.log("watch stopped after %d iterations", iteration)
			return 0
		default:
		}
		if maxIterations > 0 && iteration >= maxIterations {
			return watcher.stopCompleted(ctx, stoppedSignal, iteration)
		}
		iteration++
		if !watcher.launchIteration(ctx) {
			watcher.waitForBackground()
			watcher.emitEvent("stopped", stoppedData(stoppedSignalName(stoppedSignal), iteration, "signal"))
			watcher.log("watch stopped after %d iterations", iteration)
			return 0
		}
		// Check the limit before sleeping so the final iteration exits
		// immediately instead of waiting one extra poll interval.
		if maxIterations > 0 && iteration >= maxIterations {
			return watcher.stopCompleted(ctx, stoppedSignal, iteration)
		}
		select {
		case <-ctx.Done():
			watcher.waitForBackground()
			watcher.emitEvent("stopped", stoppedData(stoppedSignalName(stoppedSignal), iteration, "signal"))
			watcher.log("watch stopped after %d iterations", iteration)
			return 0
		case <-time.After(pollInterval):
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

func stoppedData(signalName string, iterations int, reason string) map[string]any {
	data := map[string]any{"iterations": iterations, "reason": reason}
	if signalName != "" {
		data["signal"] = signalName
	}
	return data
}

// stopCompleted waits for background work and then reports either completion
// or a signal that arrived while waiting. A SIGINT/SIGTERM during the final
// waitForBackground must not be misreported as a clean completion.
func (w *watchLoop) stopCompleted(ctx context.Context, stoppedSignal chan string, iteration int) int {
	w.waitForBackground()
	if ctx.Err() != nil {
		name := stoppedSignalName(stoppedSignal)
		w.emitEvent("stopped", stoppedData(name, iteration, "signal"))
		w.log("watch stopped after %d iterations", iteration)
		return 0
	}
	w.emitEvent("stopped", stoppedData("", iteration, "completed"))
	w.log("watch completed %d iterations", iteration)
	return 0
}

type watchLoop struct {
	command         string
	storage         *store.Store
	client          *medicover.Client
	backend         session.Store
	stdin           *os.File
	stdout          io.Writer
	stderr          io.Writer
	jsonOutput      bool
	telegramBaseURL string

	// mu guards lastRuns, paused, authActive and authRetryAt. activeMu guards
	// active. writeMu serializes all stdout/stderr writes so concurrent
	// profile workers can safely use arbitrary io.Writers (for example
	// bytes.Buffer via RunWithIO). wg tracks background authentication and
	// check workers so shutdown and --once wait for active work to finish in
	// a controlled way.
	mu       sync.Mutex
	activeMu sync.Mutex
	writeMu  sync.Mutex
	wg       sync.WaitGroup

	lastRuns    map[string]time.Time
	paused      map[string]bool
	active      map[string]bool
	authActive  map[string]bool
	authRetryAt map[string]time.Time
}

func (w *watchLoop) waitForBackground() {
	w.wg.Wait()
}

// launchIteration reloads saved configuration, selects due profiles, and
// launches their work in the background without waiting for it. Slow
// authentication or a slow profile check never blocks configuration reload
// or other accounts/profiles: each account authenticates in its own
// goroutine and each profile checks in its own goroutine, with per-profile
// active guards and SQLite leases preventing duplicates. It returns false
// only when the watch context was already cancelled and the caller should
// stop without sleeping.
func (w *watchLoop) launchIteration(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	profiles, err := w.storage.ListProfiles("")
	if err != nil {
		w.log("cannot reload profiles: %s", shortWatchMessage(err))
		return ctx.Err() == nil
	}
	now := time.Now().UTC()
	alive := map[string]store.Profile{}
	for _, profile := range profiles {
		if !profile.Enabled {
			continue
		}
		alive[profile.ID] = profile
	}
	w.mu.Lock()
	for id := range w.lastRuns {
		if _, ok := alive[id]; !ok {
			delete(w.lastRuns, id)
		}
	}
	missing := []string{}
	for id := range alive {
		if _, known := w.lastRuns[id]; !known {
			missing = append(missing, id)
		}
	}
	w.mu.Unlock()
	// Backfill last-run times from durable history so a restart respects
	// intervals instead of checking everything immediately. DB reads run
	// outside the lock so background completions are not blocked.
	for _, id := range missing {
		if last, ok := latestRunStart(w.storage, id); ok {
			w.mu.Lock()
			if _, known := w.lastRuns[id]; !known {
				w.lastRuns[id] = last
			}
			w.mu.Unlock()
		}
	}
	// Maintenance: finalize rows stuck after an interrupted final claim.
	// Stuck rows (pending at the attempt budget with an expired lease) are
	// excluded from due selection, so they cannot schedule a run by
	// themselves and would otherwise wait for another successful run — or
	// linger indefinitely for profiles that never run (long intervals,
	// disabled or paused profiles). Reaping needs no observation or
	// authentication and is a single indexed SELECT when empty, so it runs
	// for every listed profile on every iteration.
	for _, profile := range profiles {
		if _, err := w.storage.ReapExpiredMaxAttemptClaims(profile.ID, now); err != nil {
			w.log("cannot reap telegram deliveries for profile %s: %s", profile.ID, shortWatchMessage(err))
		}
	}
	if _, err := w.storage.ReapExpiredIncidentClaims(now); err != nil {
		w.log("cannot reap incident deliveries: %s", shortWatchMessage(err))
	}
	// Automatic history retention: old terminal records are removed on every
	// iteration according to the saved policy. Failures are logged but never
	// stop monitoring.
	if _, err := w.storage.PruneHistory(now); err != nil {
		w.log("cannot prune history: %s", shortWatchMessage(err))
	}
	// Incident retries need no observation run, so due incident deliveries
	// are sent directly from the iteration. Otherwise a retry whose backoff
	// passed (including a Telegram retry_after) would wait for the next due
	// profile or authentication attempt, up to a full check interval away,
	// delaying an operational alert substantially. Claims fence concurrent
	// sends across iterations and profile workers.
	if ctx.Err() == nil {
		if dueIncidents, err := w.storage.ListDueIncidentDeliveries(now); err != nil {
			w.log("cannot list incident deliveries: %s", shortWatchMessage(err))
		} else if len(dueIncidents) > 0 {
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				w.processWatchIncidents(ctx)
			}()
		}
	}
	w.mu.Lock()
	due := monitoring.DueProfiles(profiles, w.lastRuns, now)
	w.mu.Unlock()
	// Retry-due: profiles with pending Telegram work whose latest observation
	// was complete become due for a fresh observation run before the repeated
	// delivery, even before their interval elapses. Failed runs respect the
	// interval so persistent Medicover failures do not hammer the portal
	// every poll while pending exists. Valid retry_after values already gate
	// ListDueDeliveries through next_attempt_at.
	if ctx.Err() == nil {
		dueIDs := map[string]bool{}
		for _, profile := range due {
			dueIDs[profile.ID] = true
		}
		for _, profile := range profiles {
			if !profile.Enabled || dueIDs[profile.ID] {
				continue
			}
			retryDue, err := w.storage.IsRetryDue(profile.ID, now)
			if err != nil {
				w.log("cannot check telegram retry for profile %s: %s", profile.ID, shortWatchMessage(err))
				continue
			}
			if retryDue {
				due = append(due, profile)
			}
		}
	}
	if len(due) == 0 {
		return ctx.Err() == nil
	}
	// Group due profiles by account so one authentication attempt serves all
	// of that account's profiles in this iteration. Paused accounts are
	// included so a later iteration can retry authentication and resume;
	// authenticateAccount emits paused/resumed only on transitions. Accounts
	// in auth backoff or with an authentication already in flight are
	// skipped here; the per-account guard in runAccountProfiles re-checks
	// atomically to close the race between concurrent iterations.
	byAccount := map[string][]store.Profile{}
	groupNow := time.Now().UTC()
	for _, profile := range due {
		w.mu.Lock()
		retryAt := w.authRetryAt[profile.AccountID]
		authBusy := w.authActive[profile.AccountID]
		w.mu.Unlock()
		if authBusy || (!retryAt.IsZero() && groupNow.Before(retryAt)) {
			continue
		}
		w.activeMu.Lock()
		isActive := w.active[profile.ID]
		w.activeMu.Unlock()
		if isActive {
			continue
		}
		byAccount[profile.AccountID] = append(byAccount[profile.AccountID], profile)
	}
	if len(byAccount) == 0 {
		return ctx.Err() == nil
	}
	for accountID, accountProfiles := range byAccount {
		if ctx.Err() != nil {
			return false
		}
		w.wg.Add(1)
		go w.runAccountProfiles(ctx, accountID, accountProfiles)
	}
	return ctx.Err() == nil
}

// runAccountProfiles authenticates one account and launches its due profiles.
// It runs in its own goroutine so a slow account never delays other accounts.
// A per-account guard ensures at most one authentication flow per account is
// in flight: concurrent iterations for the same account skip instead of
// stacking up parallel logins.
func (w *watchLoop) runAccountProfiles(ctx context.Context, accountID string, accountProfiles []store.Profile) {
	defer w.wg.Done()
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	if w.authActive[accountID] {
		w.mu.Unlock()
		return
	}
	if retryAt, ok := w.authRetryAt[accountID]; ok && time.Now().UTC().Before(retryAt) {
		w.mu.Unlock()
		return
	}
	w.authActive[accountID] = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.authActive, accountID)
		w.mu.Unlock()
	}()
	account, err := w.storage.GetAccount(accountID)
	if err != nil {
		w.log("cannot load account %s: %s", accountID, shortWatchMessage(err))
		return
	}
	token, ok := w.authenticateAccount(ctx, account)
	if !ok {
		return
	}
	for _, profile := range accountProfiles {
		if ctx.Err() != nil {
			return
		}
		// Refresh the profile row after authentication: another process may
		// have edited it while authentication was in flight.
		fresh, err := w.storage.GetProfile(profile.ID)
		if err != nil || !fresh.Enabled {
			continue
		}
		// Re-evaluate due status against the refreshed configuration and the
		// current schedule. An operator may have lengthened the interval
		// while authentication was in progress; the saved configuration wins
		// over the snapshot taken before authentication. Retry-due profiles
		// stay due for a fresh observation before the repeated delivery.
		w.mu.Lock()
		last := w.lastRuns[fresh.ID]
		stillDue := monitoring.IsProfileDue(fresh, last, time.Now().UTC())
		w.mu.Unlock()
		if !stillDue {
			retryDue, err := w.storage.IsRetryDue(fresh.ID, time.Now().UTC())
			if err != nil || !retryDue {
				continue
			}
		}
		w.activeMu.Lock()
		if w.active[fresh.ID] {
			w.activeMu.Unlock()
			continue
		}
		w.active[fresh.ID] = true
		w.activeMu.Unlock()
		w.wg.Add(1)
		go w.runSingleProfile(ctx, fresh, account, token)
	}
}

// runSingleProfile runs one profile check in the background and records its
// start time for future scheduling.
func (w *watchLoop) runSingleProfile(ctx context.Context, profile store.Profile, account store.Account, accessToken string) {
	defer w.wg.Done()
	defer func() {
		w.activeMu.Lock()
		delete(w.active, profile.ID)
		w.activeMu.Unlock()
	}()
	started := time.Now().UTC()
	w.checkOne(ctx, profile, account, accessToken, started)
	w.mu.Lock()
	// A cancelled shutdown must not reschedule: the process is leaving and
	// the next start backfills from durable history.
	if ctx.Err() == nil {
		w.lastRuns[profile.ID] = started
	}
	w.mu.Unlock()
}

// authenticateAccount returns an access token for one account, reusing the
// trusted session when possible. Authentication Required pauses only that
// account; other accounts continue. It returns false when the account must
// be skipped this iteration.
func (w *watchLoop) authenticateAccount(ctx context.Context, account store.Account) (string, bool) {
	var saved *medicover.SessionState
	if state, err := w.backend.Load(account.ID); err == nil {
		saved = state
	} else if errors.Is(err, session.ErrNotFound) || isSessionCorrupt(err) {
		saved = nil
	} else if errors.Is(err, session.ErrUnsafe) {
		w.log("account %s session is not safe: %s", account.ID, shortWatchMessage(err))
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": "invalid_arguments", "message": shortWatchMessage(err)})
		w.setAuthBackoff(account.ID, authFailureBackoff)
		return "", false
	} else {
		w.log("account %s session is temporarily unavailable", account.ID)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": "temporary_failure", "message": "session storage is temporarily unavailable"})
		w.setAuthBackoff(account.ID, authFailureBackoff)
		return "", false
	}
	authCtx, cancel := context.WithTimeout(ctx, watchRunTimeout)
	defer cancel()
	auth, err := w.client.Authenticate(authCtx, medicover.AuthRequest{Session: saved})
	if err == nil {
		if saveErr := w.backend.Save(account.ID, auth.Session); saveErr != nil {
			w.log("account %s cannot save session state", account.ID)
			w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": "temporary_failure", "message": "cannot save session state"})
			w.setAuthBackoff(account.ID, authFailureBackoff)
			return "", false
		}
		w.clearAuthBackoff(account.ID)
		w.mu.Lock()
		if w.paused[account.ID] {
			delete(w.paused, account.ID)
			w.mu.Unlock()
			w.log("account %s authentication recovered", account.ID)
			w.emitEvent("auth_resumed", map[string]any{"account": account.ID})
		} else {
			w.mu.Unlock()
		}
		return auth.AccessToken, true
	}
	if !medicover.IsAuthRequired(err) && !medicover.IsInvalidCredentials(err) {
		// Temporary, rate-limited, and protocol failures are per-iteration
		// profile failures, not an account pause. Other accounts continue.
		// Throttle retries per account so a 30-minute profile does not
		// re-attempt login every poll; rate_limited respects RetryAfter.
		code, message := watchMedicoverFailure(err)
		w.log("account %s authentication %s: %s", account.ID, code, message)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": code, "message": message})
		w.trackWatchAccountFailure(ctx, account, err)
		w.setAuthBackoff(account.ID, authFailureDelay(err))
		return "", false
	}
	password, resolveErr := secrets.Resolve(account.PasswordSource, account.PasswordRef, account.ID, w.stdin, w.stderr, true)
	if resolveErr != nil {
		w.pauseAccount(account, "authentication_required", "authentication is required")
		w.setAuthBackoff(account.ID, authFailureBackoff)
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
			w.pauseAccount(account, "authentication_required", "authentication is required")
			w.trackWatchAccountFailure(ctx, account, retryErr)
			w.setAuthBackoff(account.ID, authFailureBackoff)
			return "", false
		}
		code, message := watchMedicoverFailure(retryErr)
		w.log("account %s authentication %s: %s", account.ID, code, message)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": code, "message": message})
		w.trackWatchAccountFailure(ctx, account, retryErr)
		w.setAuthBackoff(account.ID, authFailureDelay(retryErr))
		return "", false
	}
	if saveErr := w.backend.Save(account.ID, retry.Session); saveErr != nil {
		w.log("account %s cannot save session state", account.ID)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "code": "temporary_failure", "message": "cannot save session state"})
		w.setAuthBackoff(account.ID, authFailureBackoff)
		return "", false
	}
	w.clearAuthBackoff(account.ID)
	w.mu.Lock()
	if w.paused[account.ID] {
		delete(w.paused, account.ID)
		w.mu.Unlock()
		w.log("account %s authentication recovered", account.ID)
		w.emitEvent("auth_resumed", map[string]any{"account": account.ID})
	} else {
		w.mu.Unlock()
	}
	return retry.AccessToken, true
}

// setAuthBackoff delays the next authentication attempt for one account so
// temporary login failures do not retry on every poll.
func (w *watchLoop) setAuthBackoff(accountID string, delay time.Duration) {
	if delay <= 0 {
		delay = authFailureBackoff
	}
	w.mu.Lock()
	if w.authRetryAt == nil {
		w.authRetryAt = map[string]time.Time{}
	}
	w.authRetryAt[accountID] = time.Now().UTC().Add(delay)
	w.mu.Unlock()
}

func (w *watchLoop) clearAuthBackoff(accountID string) {
	w.mu.Lock()
	delete(w.authRetryAt, accountID)
	w.mu.Unlock()
}

// authFailureDelay respects the server-provided RetryAfter for rate limits
// and falls back to a fixed backoff otherwise.
func authFailureDelay(err error) time.Duration {
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) && medicoverErr.Code == medicover.CodeRateLimited && medicoverErr.RetryAfter > 0 {
		return medicoverErr.RetryAfter
	}
	return authFailureBackoff
}

func (w *watchLoop) pauseAccount(account store.Account, code, message string) {
	w.mu.Lock()
	already := w.paused[account.ID]
	if !already {
		w.paused[account.ID] = true
	}
	w.mu.Unlock()
	if !already {
		w.log("account %s requires authentication, pausing its profiles", account.ID)
		w.emitEvent("auth_paused", map[string]any{"account": account.ID, "code": code, "message": message})
	}
}

// trackWatchAccountFailure records an auth-phase operational incident and
// queues its failure notification. It never stops other accounts: store
// failures are logged and the caller continues with backoff.
func (w *watchLoop) trackWatchAccountFailure(ctx context.Context, account store.Account, authErr error) {
	if !shouldRecordAccountIncident(authErr) {
		return
	}
	code, message := incidentFailureCode(authErr)
	now := time.Now().UTC()
	incident, newly, err := monitoring.RecordAccountFailure(w.storage, account.ID, code, message, now)
	if err != nil {
		w.log("account %s cannot record incident: %s", account.ID, shortWatchMessage(err))
		return
	}
	if newly {
		w.log("account %s operational incident started: %s", account.ID, code)
		w.emitEvent("incident_started", map[string]any{"scope": store.IncidentScopeAccount, "account": account.ID, "incident": incident.ID, "code": code, "message": shortWatchMessage(authErr)})
	} else if incident.ConsecutiveFailures > 1 {
		w.emitEvent("incident_updated", map[string]any{"scope": store.IncidentScopeAccount, "account": account.ID, "incident": incident.ID, "code": code, "consecutive_failures": incident.ConsecutiveFailures})
	}
	w.processWatchIncidents(ctx)
}

// trackWatchProfileFailure records a search-phase operational incident for
// one profile without stopping other profiles. A search-phase authentication
// failure means the account session is bad, so it records one account-level
// problem instead of a profile incident.
func (w *watchLoop) trackWatchProfileFailure(ctx context.Context, profile store.Profile, account store.Account, checkErr error) {
	if medicover.IsAuthRequired(checkErr) {
		// Search-phase authentication failure means the account session is
		// bad: one account-level problem, not one problem per profile.
		w.trackWatchAccountFailure(ctx, account, checkErr)
		return
	}
	if !shouldRecordProfileIncident(checkErr) {
		return
	}
	code, message := incidentFailureCode(checkErr)
	now := time.Now().UTC()
	incident, newly, err := monitoring.RecordProfileFailure(w.storage, profile, code, message, now)
	if err != nil {
		w.log("profile %s cannot record incident: %s", profile.ID, shortWatchMessage(err))
		return
	}
	if newly {
		w.log("profile %s operational incident started: %s", profile.ID, code)
		w.emitEvent("incident_started", map[string]any{"scope": store.IncidentScopeProfile, "account": account.ID, "profile": profile.ID, "incident": incident.ID, "code": code, "message": shortWatchMessage(checkErr)})
	} else if incident.ConsecutiveFailures > 1 {
		w.emitEvent("incident_updated", map[string]any{"scope": store.IncidentScopeProfile, "account": account.ID, "profile": profile.ID, "incident": incident.ID, "code": code, "consecutive_failures": incident.ConsecutiveFailures})
	}
	w.processWatchIncidents(ctx)
}

// resolveWatchIncidentsOnSuccess ends profile and account incidents after one
// complete successful run and emits recovery events for newly created
// recovery notifications.
func (w *watchLoop) resolveWatchIncidentsOnSuccess(ctx context.Context, profile store.Profile) {
	now := time.Now().UTC()
	profileIncident, recoveries, _, err := w.storage.ResolveIncident(store.IncidentScopeProfile, profile.ID, profile.ID, "", now)
	if err != nil {
		w.log("profile %s cannot resolve incident: %s", profile.ID, shortWatchMessage(err))
	} else if profileIncident.ID != "" && len(recoveries) > 0 {
		w.log("profile %s operational incident resolved", profile.ID)
		w.emitEvent("incident_resolved", map[string]any{"scope": store.IncidentScopeProfile, "account": profile.AccountID, "profile": profile.ID, "incident": profileIncident.ID})
	}
	if accountIncident, accountRecoveries, _, err := w.storage.ResolveIncident(store.IncidentScopeAccount, profile.AccountID, "", "", now); err != nil {
		w.log("account %s cannot resolve incident: %s", profile.AccountID, shortWatchMessage(err))
	} else if accountIncident.ID != "" && len(accountRecoveries) > 0 {
		w.log("account %s operational incident resolved", profile.AccountID)
		w.emitEvent("incident_resolved", map[string]any{"scope": store.IncidentScopeAccount, "account": profile.AccountID, "incident": accountIncident.ID})
	}
	w.processWatchIncidents(ctx)
}

// trackWatchDestinationIncidents creates destination incidents for permanent
// non-stale availability failures and resolves incidents for destinations
// that delivered successfully. It never stops other profiles.
func (w *watchLoop) trackWatchDestinationIncidents(ctx context.Context, profile store.Profile, summary monitoring.DeliverySummary) error {
	now := time.Now().UTC()
	failed := map[string]bool{}
	for _, delivery := range summary.Deliveries {
		if delivery.Status != store.DeliveryPermanentFailure {
			continue
		}
		if strings.Contains(strings.ToLower(delivery.LastError), "no longer available") {
			continue
		}
		if strings.Contains(strings.ToLower(delivery.LastError), "incident ended before") {
			continue
		}
		if strings.Contains(strings.ToLower(delivery.LastError), "5 attempts") {
			continue
		}
		failed[delivery.DestinationID] = true
		incident, newly, err := monitoring.RecordDestinationFailure(w.storage, profile, delivery.DestinationID, "permanent_failure", delivery.LastError, now)
		if err != nil {
			return err
		}
		if newly {
			w.log("profile %s destination %s delivery failed permanently", profile.ID, delivery.DestinationID)
			w.emitEvent("incident_started", map[string]any{"scope": store.IncidentScopeDestination, "account": profile.AccountID, "profile": profile.ID, "destination": delivery.DestinationID, "incident": incident.ID, "code": "permanent_failure"})
		}
	}
	for _, delivery := range summary.Deliveries {
		if delivery.Status != store.DeliveryDelivered {
			continue
		}
		// Destinations that also failed in this same pass keep their
		// incident: the route is still broken, and resolving now would
		// cancel the just-created failure notification before it is
		// reported.
		if failed[delivery.DestinationID] {
			continue
		}
		if incident, _, _, err := w.storage.ResolveIncident(store.IncidentScopeDestination, delivery.DestinationID, profile.ID, delivery.DestinationID, now); err != nil {
			return err
		} else if incident.ID != "" {
			w.log("profile %s destination %s delivery recovered", profile.ID, delivery.DestinationID)
			w.emitEvent("incident_resolved", map[string]any{"scope": store.IncidentScopeDestination, "account": profile.AccountID, "profile": profile.ID, "destination": delivery.DestinationID, "incident": incident.ID})
		}
	}
	return nil
}

// processWatchIncidents sends due operational failure/recovery notifications
// without blocking monitoring. Delivery store errors are logged; Telegram
// retry state stays pending for later cycles. One destination's failure never
// stops other destinations.
func (w *watchLoop) processWatchIncidents(ctx context.Context) {
	sender := telegramSenderFor(options{telegramBaseURL: w.telegramBaseURL})
	resolve := func(destination store.Destination) (telegram.Secret, error) {
		token, err := secrets.ResolveTelegramToken(destination.TokenSource, destination.TokenRef, destination.ID, w.stdin, w.stderr, true)
		if err != nil {
			return "", err
		}
		secret := telegram.Secret(token)
		token = ""
		return secret, nil
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	summary, err := monitoring.ProcessIncidentDeliveries(deliveryCtx, w.storage, sender, resolve, time.Now().UTC())
	if err != nil {
		w.log("incident delivery error: %s", shortWatchMessage(err))
		return
	}
	if summary.Attempted > 0 || summary.Failed > 0 {
		w.log("incidents telegram: %d delivered, %d pending, %d failed", summary.Delivered, summary.StillRetry, summary.Failed)
	}
}

// checkOne runs a single enabled profile with failure isolation. A failure
// never stops other profiles; SQLite leases skip duplicates without treating
// them as failures.
func (w *watchLoop) checkOne(ctx context.Context, profile store.Profile, account store.Account, accessToken string, started time.Time) {
	w.emitEvent("run_started", map[string]any{"account": account.ID, "profile": profile.ID})
	w.log("checking profile %s (account %s)", profile.ID, account.ID)
	runCtx, cancel := context.WithTimeout(ctx, watchRunTimeout)
	defer cancel()
	result, err := monitoring.Check(runCtx, w.storage, profile, account, w.client, accessToken, started)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrObservationRunActive):
			w.log("profile %s skipped: observation run is already active", profile.ID)
			w.emitEvent("run_skipped", map[string]any{"account": account.ID, "profile": profile.ID, "code": "run_active", "message": "observation run is already active"})
			return
		case errors.Is(err, store.ErrObservationRunStale):
			w.log("profile %s skipped: %s", profile.ID, shortWatchMessage(err))
			w.emitEvent("run_failed", map[string]any{"account": account.ID, "profile": profile.ID, "code": "stale_result", "message": shortWatchMessage(err)})
			return
		case errors.Is(err, store.ErrObservationRunConflicting):
			w.log("profile %s conflicting result: %s", profile.ID, shortWatchMessage(err))
			w.emitEvent("run_failed", map[string]any{"account": account.ID, "profile": profile.ID, "code": "conflicting_result", "message": shortWatchMessage(err)})
			return
		case errors.Is(err, store.ErrProfileDisabled):
			w.log("profile %s is disabled, skipping", profile.ID)
			return
		}
		code, message := watchMedicoverFailure(err)
		// A shutdown race reports cancelled, never a failure: the run was
		// recorded without touching availability episodes.
		if ctx.Err() != nil && code == "cancelled" {
			w.log("profile %s cancelled during shutdown", profile.ID)
			w.emitEvent("run_failed", map[string]any{"account": account.ID, "profile": profile.ID, "code": code, "message": message})
			return
		}
		w.log("profile %s check %s: %s", profile.ID, code, message)
		w.emitEvent("run_failed", map[string]any{"account": account.ID, "profile": profile.ID, "code": code, "message": message})
		w.trackWatchProfileFailure(ctx, profile, account, err)
		return
	}
	// One complete successful run ends operational incidents. Failed runs
	// never reach here, so they never resolve incidents and never end
	// availability episodes.
	w.resolveWatchIncidentsOnSuccess(ctx, profile)
	deliveries := w.deliverAfterWatchCheck(ctx, profile, result)
	if err := w.trackWatchDestinationIncidents(ctx, profile, deliveries); err != nil {
		w.log("profile %s cannot track destination incidents: %s", profile.ID, shortWatchMessage(err))
	}
	w.processWatchIncidents(ctx)
	if deliveries.Attempted > 0 || deliveries.Cancelled > 0 || deliveries.Failed > 0 {
		w.log("profile %s telegram: %d delivered, %d pending, %d failed, %d cancelled", profile.ID, deliveries.Delivered, deliveries.StillRetry, deliveries.Failed, deliveries.Cancelled)
	}
	w.log("profile %s completed: %d slots, %d newly available, %d ended", profile.ID, len(result.Search.Slots), len(result.Reconciliation.NewEpisodes), len(result.Reconciliation.EndedEpisodes))
	w.emitEvent("run_completed", map[string]any{
		"account":              account.ID,
		"profile":              profile.ID,
		"run":                  result.Reconciliation.Run.ID,
		"slot_count":           len(result.Search.Slots),
		"newly_available":      len(result.Reconciliation.NewEpisodes),
		"ended":                len(result.Reconciliation.EndedEpisodes),
		"active_episode_count": countActiveEpisodes(w.storage, profile.ID),
		"delivered":            deliveries.Delivered,
		"delivery_failed":      deliveries.Failed,
		"delivery_pending":     deliveries.StillRetry,
		"delivery_cancelled":   deliveries.Cancelled,
	})
}

// deliverAfterWatchCheck runs durable Telegram notifications after a complete
// watch run. Observation success wins: delivery store errors are logged but
// still report run_completed, and Telegram retryable failures stay pending
// for later cycles. Token resolution never prompts so automation never blocks.
func (w *watchLoop) deliverAfterWatchCheck(ctx context.Context, profile store.Profile, result monitoring.CheckResult) monitoring.DeliverySummary {
	sender := telegramSenderFor(options{telegramBaseURL: w.telegramBaseURL})
	resolve := func(destination store.Destination) (telegram.Secret, error) {
		token, err := secrets.ResolveTelegramToken(destination.TokenSource, destination.TokenRef, destination.ID, w.stdin, w.stderr, true)
		if err != nil {
			return "", err
		}
		secret := telegram.Secret(token)
		token = ""
		return secret, nil
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	summary, err := monitoring.ProcessAvailabilityDeliveries(deliveryCtx, w.storage, profile, result.Reconciliation, sender, resolve, time.Now().UTC())
	if err != nil {
		w.log("profile %s delivery error: %s", profile.ID, shortWatchMessage(err))
		return monitoring.DeliverySummary{Deliveries: []store.Delivery{}}
	}
	return summary
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
	if err != nil {
		return time.Time{}, false
	}
	return monitoring.LatestRunStart(runs)
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

// emitEvent writes one JSON Lines machine event. Writes are serialized so
// concurrent profile workers can safely share arbitrary io.Writers.
func (w *watchLoop) emitEvent(event string, data map[string]any) {
	if !w.jsonOutput {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	writeJSON(w.stdout, watchEvent{
		SchemaVersion: resultSchemaVersion,
		Command:       "watch",
		Event:         event,
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		Data:          data,
	})
}

// log writes one text log line to standard error. Writes are serialized for
// the same reason as machine events.
func (w *watchLoop) log(format string, args ...any) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	stamp := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(w.stderr, "%s watch: %s\n", stamp, fmt.Sprintf(format, args...))
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
// covers every enabled profile, so account, profile, criteria, secret,
// telegram, and dry flags are never valid here.
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
	case settings.telegramIDsRaw != "":
		return fmt.Errorf("--telegram is not supported for watch")
	case settings.telegramName != "":
		return fmt.Errorf("--name is not supported for watch")
	case settings.chatID != "":
		return fmt.Errorf("--chat-id is not supported for watch")
	case settings.tokenFile != "":
		return fmt.Errorf("--token-file is not supported for watch")
	case settings.tokenPrompt:
		return fmt.Errorf("--token-prompt is not supported for watch")
	case settings.noStoredToken:
		return fmt.Errorf("--no-stored-token is not supported for watch")
	case settings.clearTelegram:
		return fmt.Errorf("--clear-telegram is not supported for watch")
	case settings.dry:
		return fmt.Errorf("--dry is not supported for watch")
	}
	return nil
}
