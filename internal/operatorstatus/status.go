// Package operatorstatus reads the facts and required actions shared by the
// command and terminal adapters. It never prompts or changes saved data.
package operatorstatus

import (
	"errors"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

// Account reports session availability without exposing saved session data.
type Account struct {
	ID            string
	Username      string
	Authenticated bool
	AuthRequired  bool
}

// Action identifies a required operator action. Adapters supply display text.
type Action struct {
	Code  string
	Scope string
	ID    string
}

// Status contains saved configuration, historical failures, and required actions.
// Failure lists retain cancellations to preserve the history contract.
type Status struct {
	Accounts              []Account
	Profiles              []store.Profile
	Destinations          []store.Destination
	DestinationProfiles   map[string][]string
	ActiveIncidents       []store.Incident
	PermanentFailures     []store.Delivery
	OperationalDeliveries []store.IncidentDelivery
	RequiredActions       []Action
	RetentionDays         int
}

// Read evaluates the saved operator state at now. Session read errors leave
// authentication unknown unless the session is missing or corrupt.
func Read(storage *store.Store, backend session.Store, issuer string, now time.Time) (Status, error) {
	data := Status{Accounts: []Account{}, RequiredActions: []Action{}}
	accounts, err := storage.ListAccounts()
	if err != nil {
		return data, err
	}
	for _, account := range accounts {
		state := Account{ID: account.ID, Username: account.Username, Authenticated: true}
		if saved, loadErr := backend.Load(account.ID); loadErr != nil {
			if errors.Is(loadErr, session.ErrNotFound) || errors.Is(loadErr, session.ErrCorrupt) {
				state.Authenticated = false
				state.AuthRequired = true
			} else {
				state.Authenticated = false
			}
		} else if !medicover.HasUsableSessionCookiesForIssuer(saved, issuer, now) {
			state.Authenticated = false
			state.AuthRequired = true
		}
		data.Accounts = append(data.Accounts, state)
	}
	data.Profiles, err = storage.ListProfiles("")
	if err != nil {
		return data, err
	}
	data.DestinationProfiles = map[string][]string{}
	for _, profile := range data.Profiles {
		if !profile.Enabled {
			data.RequiredActions = append(data.RequiredActions, Action{Code: "profile_disabled", Scope: "profile", ID: profile.ID})
		}
		linked, linkErr := storage.ListProfileDestinationIDs(profile.ID)
		if linkErr != nil {
			return data, linkErr
		}
		for _, destinationID := range linked {
			data.DestinationProfiles[destinationID] = append(data.DestinationProfiles[destinationID], profile.ID)
		}
	}
	data.Destinations, err = storage.ListDestinations()
	if err != nil {
		return data, err
	}
	for _, destination := range data.Destinations {
		if !destination.Enabled {
			data.RequiredActions = append(data.RequiredActions, Action{Code: "destination_disabled", Scope: "destination", ID: destination.ID})
		}
		if destination.LastTestStatus == store.DeliveryPermanentFailure {
			data.RequiredActions = append(data.RequiredActions, Action{Code: "destination_test_failure", Scope: "destination", ID: destination.ID})
		}
	}
	data.ActiveIncidents, err = storage.ListIncidents(store.IncidentStatusActive, "", 1000)
	if err != nil {
		return data, err
	}
	for _, incident := range data.ActiveIncidents {
		id := incident.ScopeID
		if id == "" {
			id = incident.ID
		}
		data.RequiredActions = append(data.RequiredActions, Action{Code: "active_incident", Scope: incident.ScopeType, ID: id})
		if incident.ScopeType == store.IncidentScopeAccount && accountPauseIncident(incident) {
			for index := range data.Accounts {
				if data.Accounts[index].ID == incident.ScopeID {
					data.Accounts[index].AuthRequired = true
					data.Accounts[index].Authenticated = false
				}
			}
		}
	}
	// Incidents can override saved cookies. Derive account actions only after
	// that override so each account has one current action.
	for _, account := range data.Accounts {
		if account.AuthRequired {
			data.RequiredActions = append(data.RequiredActions, Action{Code: "authentication_required", Scope: "account", ID: account.ID})
		} else if !account.Authenticated {
			data.RequiredActions = append(data.RequiredActions, Action{Code: "session_unavailable", Scope: "account", ID: account.ID})
		}
	}
	data.PermanentFailures, err = storage.ListRecentDeliveries("", store.DeliveryPermanentFailure, 1000)
	if err != nil {
		return data, err
	}
	destinationFailures := map[string]struct{}{}
	for _, delivery := range data.PermanentFailures {
		if IsCancelledDelivery(delivery.Status, delivery.LastError) {
			continue
		}
		data.RequiredActions = append(data.RequiredActions, Action{Code: "permanent_failure", Scope: "delivery", ID: delivery.ID})
		if destinationID := strings.TrimSpace(delivery.DestinationID); destinationID != "" {
			if _, seen := destinationFailures[destinationID]; !seen {
				data.RequiredActions = append(data.RequiredActions, Action{Code: "destination_delivery_failure", Scope: "destination", ID: destinationID})
				destinationFailures[destinationID] = struct{}{}
			}
		}
	}
	data.OperationalDeliveries, err = storage.ListRecentIncidentDeliveriesByStatus("", store.DeliveryPermanentFailure, 1000)
	if err != nil {
		return data, err
	}
	for _, delivery := range data.OperationalDeliveries {
		if IsCancelledDelivery(delivery.Status, delivery.LastError) {
			continue
		}
		data.RequiredActions = append(data.RequiredActions, Action{Code: "permanent_failure", Scope: "operational_delivery", ID: delivery.ID})
		if destinationID := strings.TrimSpace(delivery.DestinationID); destinationID != "" {
			if _, seen := destinationFailures[destinationID]; !seen {
				data.RequiredActions = append(data.RequiredActions, Action{Code: "destination_delivery_failure", Scope: "destination", ID: destinationID})
				destinationFailures[destinationID] = struct{}{}
			}
		}
	}
	data.RetentionDays, err = storage.GetHistoryRetentionDays()
	if errors.Is(err, store.ErrHistoryInvalid) {
		data.RetentionDays = store.DefaultHistoryRetentionDays
		data.RequiredActions = append(data.RequiredActions, Action{Code: "invalid_retention", Scope: "history", ID: "retention"})
	} else if err != nil {
		return data, err
	}
	return data, nil
}

func accountPauseIncident(incident store.Incident) bool {
	if incident.Kind == store.IncidentKindAuth {
		return true
	}
	switch strings.TrimSpace(incident.FailureCode) {
	case "authentication_required", "mfa_required", "invalid_credentials":
		return true
	default:
		return false
	}
}

// IsCancelledDelivery recognizes historical cancellations stored as permanent failures.
// These records stay in history but do not require operator action.
func IsCancelledDelivery(status, message string) bool {
	if status != store.DeliveryPermanentFailure {
		return false
	}
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "slot is no longer available") ||
		strings.Contains(message, "incident ended before failure was delivered")
}
