package telegram_test

import (
	"strings"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

func TestPolishIncidentFormatting(t *testing.T) {
	failure := telegram.FormatOperationalFailure(telegram.OperationalProblem{
		Scope:   "profile",
		Account: "alice",
		Profile: "morning",
		Code:    "protocol_changed",
		Message: "portal changed",
	})
	for _, want := range []string{"Problem", "Zakres", "Konto", "alice", "Profil", "morning", "protocol_changed", "portal changed", "Błąd", "Opis"} {
		if !strings.Contains(failure, want) {
			t.Fatalf("failure missing %q: %q", want, failure)
		}
	}
	recovery := telegram.FormatOperationalRecovery(telegram.OperationalProblem{
		Scope:   "profile",
		Account: "alice",
		Profile: "morning",
	})
	for _, want := range []string{"wznowione", "Zakres", "Konto", "alice", "Profil", "morning"} {
		if !strings.Contains(recovery, want) {
			t.Fatalf("recovery missing %q: %q", want, recovery)
		}
	}
}
