package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestApprovalStaleViewShape: a movement projects its serials always and the
// mover fields only when known; a zero MovedAt renders no timestamp.
func TestApprovalStaleViewShape(t *testing.T) {
	job := int64(42)
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	v := NewApprovalStaleView(store.PlanMovement{
		PlanSerial: 3, CurrentSerial: 9, MovedByJob: &job, MovedVia: "schedule", MovedAt: at,
	})
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"plan_serial":3`, `"current_serial":9`, `"moved_by_job":42`,
		`"moved_via":"schedule"`, `"moved_at":"2026-08-25T12:00:00Z"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("stale view %s missing %s", got, want)
		}
	}

	// An unattributed movement (serial moved outside the control plane) omits the
	// mover fields but still carries the serials.
	bare, _ := json.Marshal(NewApprovalStaleView(store.PlanMovement{PlanSerial: 1, CurrentSerial: 2}))
	if s := string(bare); strings.Contains(s, "moved_by_job") || strings.Contains(s, "moved_at") {
		t.Fatalf("bare stale view %s should omit mover fields", s)
	}
}

// TestApprovalProvenanceViewShape: a manual origin carries only its origin; an
// alert origin carries the alert, rule, and matched_at.
func TestApprovalProvenanceViewShape(t *testing.T) {
	manual, _ := json.Marshal(NewApprovalProvenanceView(OriginManual, "", "", nil))
	if s := string(manual); s != `{"origin":"manual"}` {
		t.Fatalf("manual provenance = %s, want only origin", s)
	}
	at := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	alert, _ := json.Marshal(NewApprovalProvenanceView(OriginAlert, "DiskFull", "apply-on-drift", &at))
	for _, want := range []string{`"origin":"alert"`, `"alert":"DiskFull"`,
		`"rule":"apply-on-drift"`, `"matched_at":"2026-08-25T09:00:00Z"`} {
		if !strings.Contains(string(alert), want) {
			t.Fatalf("alert provenance %s missing %s", alert, want)
		}
	}
}

// TestErrorStaleOmitted: an ordinary error keeps the three-field shape; only a
// stale_plan error carries the stale block.
func TestErrorStaleOmitted(t *testing.T) {
	plain, _ := json.Marshal(NewError(CodeConflict, "nope"))
	if strings.Contains(string(plain), "stale") {
		t.Fatalf("plain error %s should not carry a stale block", plain)
	}
	e := NewError(CodeStalePlan, "the plan is stale")
	e.Stale = NewApprovalStaleView(store.PlanMovement{PlanSerial: 1, CurrentSerial: 2})
	if !strings.Contains(string(mustJSON(t, e)), `"stale":`) {
		t.Fatalf("stale_plan error should carry a stale block")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
