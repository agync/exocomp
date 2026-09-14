package web

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestVerdictSentenceRealHostAndNoEmptyReach is review P4's copy bug: the
// verdict sentence must name the real unreachable hosts, and a FAILED run with
// no parsed host rows must never read "Ansible could not reach ." (andList of
// an empty list joins to a trailing dot). The no-host case falls back to a
// truthful target reading.
func TestVerdictSentenceRealHostAndNoEmptyReach(t *testing.T) {
	withHosts := verdictSentence(store.Job{State: store.StateFailed}, jobOutcomeFacts{
		Unreachable: []string{"hypnotoad", "lrrr"},
		Hosts: []recapHostRow{
			{Host: "hypnotoad", Unreachable: 1},
			{Host: "lrrr", Unreachable: 1},
			{Host: "nibbler"},
		},
	})
	if !strings.Contains(withHosts, "hypnotoad and lrrr") {
		t.Errorf("verdictSentence must name the real hosts, got %q", withHosts)
	}
	if strings.Contains(withHosts, "reach .") {
		t.Errorf("verdictSentence must not read 'reach .' with hosts, got %q", withHosts)
	}

	empty := verdictSentence(store.Job{State: store.StateFailed}, jobOutcomeFacts{})
	if !strings.Contains(empty, "the target") {
		t.Errorf("verdictSentence must fall back to a truthful target reading, got %q", empty)
	}
	if strings.Contains(empty, "reach .") {
		t.Errorf("verdictSentence must not read 'reach .' with no hosts parsed, got %q", empty)
	}
}
