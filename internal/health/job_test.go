package health

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// collectSink captures a driver's events, mirroring the backup job test's
// collector.
func collectSink() (executor.EventSink, func() []executor.Event) {
	var events []executor.Event
	sink := executor.SinkFunc(func(e executor.Event) {
		events = append(events, e)
	})
	return sink, func() []executor.Event { return append([]executor.Event(nil), events...) }
}

// TestDoctorJobDriverEmitsFindings is the GATE for the doctor job's finding
// shape: the driver runs the one probe (health.Probe) and emits exactly one
// finding event per subsystem entry, each carrying the structured name/status/
// detail the job page renders as a row. Dropping an entry must red.
func TestDoctorJobDriverEmitsFindings(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	_ = os.MkdirAll(dir, 0o700)
	drv := NewJobDriver(dir, func() (Store, func() error, error) {
		t.Fatal("doctor driver must not open a store with no database")
		return nil, nil, nil
	})

	sink, events := collectSink()
	res, err := drv.Run(context.Background(), executor.Spec{Kind: JobKind, Argv: Argv()}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 on a fresh host", res.ExitCode)
	}
	findings := events()
	if len(findings) == 0 {
		t.Fatal("doctor job emitted no finding events")
	}
	// The probe's fixed subsystem order (Probe's declaration order), the same
	// shape wire.HealthView carries.
	var want []string
	for _, e := range findings {
		if e.RawJSON == "" {
			t.Errorf("finding event %q carries no structured payload", e.Msg)
			continue
		}
		var f struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &f); err != nil {
			t.Errorf("finding event %q has unparseable payload: %v", e.Msg, err)
			continue
		}
		if f.Name == "" || f.Status == "" {
			t.Errorf("finding event %q lacks name or status", e.Msg)
			continue
		}
		want = append(want, f.Name+" "+f.Status)
	}
	for _, name := range []string{"data_dir", "secrets_protection", "optional_tools", "self_observability", "litestream", "privilege_posture"} {
		seen := false
		for _, w := range want {
			if strings.HasPrefix(w, name+" ") {
				seen = true
			}
		}
		if !seen {
			t.Errorf("finding list missing the %s subsystem", name)
		}
	}
	if !strings.Contains(strings.Join(want, " | "), "self_observability ") {
		t.Error("self-observability finding missing")
	}
}

// TestDoctorJobDriverFailsOnAFault is the exit-code gate: a FAIL finding makes
// the job exit non-zero (invariant 3 — exit code decides the outcome), every
// other status stays a success.
func TestDoctorJobDriverFailsOnAFault(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// An unreadable data dir is a hard fault the probe reports as fail.
	path := filepath.Join(dir, "data")
	_ = os.MkdirAll(path, 0o700)
	_ = os.Chmod(path, 0o000)
	defer func() { _ = os.Chmod(path, 0o700) }()

	drv := NewJobDriver(path, func() (Store, func() error, error) {
		return nil, nil, nil
	})
	sink, _ := collectSink()
	res, err := drv.Run(context.Background(), executor.Spec{Kind: JobKind, Argv: Argv()}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("a failing doctor probe must exit non-zero")
	}
}
