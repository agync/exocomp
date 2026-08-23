package reactive_test

import (
	"context"
	"fmt"
	"testing"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// TestApprovalSettleClosesIncident is the item-3 guard: settling an approval
// emits a Resolved notification carrying the same Source/Host the opening page
// carried, which is the dedup identity a GoAlert transport closes the incident
// on. Proven for both terminal answers.
func TestApprovalSettleClosesIncident(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		decision string
	}{
		{"approve", reactive.DecisionApprove},
		{"reject", reactive.DecisionReject},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			h := newHarness(t, harnessOptions{})
			h.addRule(t, gatedRule("restart"))
			_, approval, job := h.gate(t, "DiskFull", "web-01")

			// The opening page identifies the approval and opens, never closes.
			open := h.sent.sent[0]
			wantSource := reactive.SourceApproval
			wantHost := fmt.Sprintf("%d", approval.ID)
			if open.Source != wantSource || open.Host != wantHost {
				t.Fatalf("opening page identity = %q/%q, want %q/%q",
					open.Source, open.Host, wantSource, wantHost)
			}
			if open.Resolved {
				t.Fatal("opening page is Resolved; it must open the incident, not close it")
			}

			if _, err := h.engine.Decide(ctx, approval.ID, tc.decision, h.token(t, job.ID), "cli"); err != nil {
				t.Fatalf("decide: %v", err)
			}

			// The settle closes: Resolved, with the opening page's exact identity so
			// GoAlert's dedup (Source/Host) lines the two halves up.
			closing := h.sent.sent[len(h.sent.sent)-1]
			if !closing.Resolved {
				t.Fatalf("settle notification is not Resolved: %q", closing.Title)
			}
			if closing.Source != open.Source || closing.Host != open.Host {
				t.Fatalf("close identity = %q/%q, want the opening page's %q/%q",
					closing.Source, closing.Host, open.Source, open.Host)
			}
		})
	}
}

// TestApprovalExpiryClosesIncident pins that the fail-closed TTL sweep also
// closes the incident: an expired approval's page is Resolved with the same
// identity, so a stranded incident does not outlive the approval.
func TestApprovalExpiryClosesIncident(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, gatedRule("restart"))
	_, approval, _ := h.gate(t, "DiskFull", "web-01")
	open := h.sent.sent[0]

	// Move past the TTL and run the clock-driven sweep.
	h.advance(reactive.DefaultApprovalTTL + 1)
	if expired, _, err := h.engine.SweepApprovals(ctx); err != nil {
		t.Fatalf("sweep approvals: %v", err)
	} else if expired != 1 {
		t.Fatalf("expired %d approvals, want 1", expired)
	}

	settled, err := h.store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if settled.State != store.ApprovalExpired {
		t.Fatalf("approval state = %s, want expired", settled.State)
	}

	closing := h.sent.sent[len(h.sent.sent)-1]
	if !closing.Resolved {
		t.Fatalf("expiry notification is not Resolved: %q", closing.Title)
	}
	if closing.Source != open.Source || closing.Host != open.Host {
		t.Fatalf("close identity = %q/%q, want %q/%q",
			closing.Source, closing.Host, open.Source, open.Host)
	}
}
