package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestJobDetailPrivilegedRunCardChip is the at-a-glance gate for the job
// detail: a run requested UNSANDBOXED wears the fail-tinted chip in the run
// card's chip row; a sandboxed run does not. Dropping the chip must red.
func TestJobDetailPrivilegedRunCardChip(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	privileged := seedJobWithRecap(t, h, store.StateSucceeded, 0,
		[]string{"PLAY RECAP", "node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0"},
		nil, store.JobArgs{Argv: []string{"/bin/true", "deploy/site.yml"}, Privileged: true})

	_, page := getBody(t, client, ts.URL+"/jobs/"+itoa64(privileged.ID))
	want := `<span class="chip chip--unsandboxed"><span class="dot"></span>unsandboxed</span>`
	if !strings.Contains(page, want) {
		t.Errorf("privileged job detail missing the unsandboxed chip %q", want)
	}

	plain := seedJobWithRecap(t, h, store.StateSucceeded, 0,
		[]string{"PLAY RECAP", "node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0"},
		nil, store.JobArgs{Argv: []string{"/bin/true", "deploy/site.yml"}})
	_, plainPage := getBody(t, client, ts.URL+"/jobs/"+itoa64(plain.ID))
	if strings.Contains(plainPage, "chip--unsandboxed") {
		t.Error("sandboxed job detail wears the unsandboxed chip")
	}
}

// TestJobsListPrivilegedMarker is the list gate: a privileged row carries the
// "unsandboxed" mono fact beside its source (and in the phone line-2 meta),
// so the ledger distinguishes it from a sandboxed run at a glance; a sandboxed
// row does not. Dropping the fact must red.
func TestJobsListPrivilegedMarker(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	privileged := seedJobWithRecap(t, h, store.StateSucceeded, 0,
		[]string{"PLAY RECAP", "node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0"},
		nil, store.JobArgs{Argv: []string{"/bin/true", "deploy/site.yml"}, Source: "tree", Privileged: true})

	_, body := getBody(t, client, ts.URL+"/jobs")
	if !strings.Contains(body, `<a class="jobs-row" href="/jobs/`+itoa64(privileged.ID)+`">`) {
		t.Fatalf("privileged row %d missing from the list", privileged.ID)
	}
	if !strings.Contains(body, `<small class="mono">tree · unsandboxed</small>`) {
		t.Errorf("privileged list row missing the source · unsandboxed fact:\n%s", body)
	}
	// The phone line-2 meta carries the same fact.
	if !strings.Contains(body, "tree · unsandboxed · ") {
		t.Error("privileged list row missing unsandboxed in the phone meta")
	}

	plain := seedJobWithRecap(t, h, store.StateSucceeded, 0,
		[]string{"PLAY RECAP", "node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0"},
		nil, store.JobArgs{Argv: []string{"/bin/true", "deploy/site.yml"}, Source: "tree"})
	if plain.Args.Privileged {
		t.Fatal("plain seed job is privileged")
	}
	// The plain row's source fact has no unsandboxed token.
	if strings.Contains(body, "tree · unsandboxed · unsandboxed") {
		t.Error("sandboxed list row fabricated an unsandboxed fact")
	}
}

// TestJobDetailPrivilegedLogBands is the events-feed gate: the enqueue-time
// audit record (service.recordPrivileged) and the queue's execution-time
// record both render as fail-tinted log bands, so the log says a privileged
// run was queued and ran unsandboxed instead of dimming them among the system
// lines. Dropping either band case must red.
func TestJobDetailPrivilegedLogBands(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	job := seedJobWithRecap(t, h, store.StateSucceeded, 0,
		[]string{"PLAY RECAP", "node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0"},
		nil, store.JobArgs{Argv: []string{"/bin/true", "deploy/site.yml"}, Privileged: true})
	for _, msg := range []string{
		"run requested UNSANDBOXED (privileged) by operator alice via web",
		"running UNSANDBOXED (privileged): no kernel sandbox for this job",
	} {
		if _, err := h.store.AppendJobEvent(context.Background(), store.JobEvent{
			JobID: job.ID, Level: store.LevelSystem, Msg: msg,
		}); err != nil {
			t.Fatalf("append %q: %v", msg, err)
		}
	}

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, page := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if n := strings.Count(page, `class="logband unsandboxed"`); n != 2 {
		t.Errorf("privileged job log has %d unsandboxed bands, want 2:\n%s", n, page)
	}
	// The band carries the "▸ " prefix; a plain system line would render the
	// message without it, so these gate the band treatment, not just presence.
	for _, want := range []string{
		"▸ run requested UNSANDBOXED (privileged) by operator alice via web",
		"▸ running UNSANDBOXED (privileged): no kernel sandbox for this job",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("privileged job log missing the banded line %q", want)
		}
	}
}

// TestPrivilegeChipAndBandCSS pins the two new state styles: the chip reuses
// the fail signal the launcher's privileged block uses, and the log band is
// fail-tinted. A missing rule is a silent loss of the visual distinction.
func TestPrivilegeChipAndBandCSS(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.chip--unsandboxed {`,
		`.log .logband.unsandboxed {`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the privilege style %q", want)
		}
	}
}

// TestSourceLauncherPrivilegedNudge is the decision-point gate: the launcher's
// privileged toggle names the sandbox-preserving alternative (reach the host
// over SSH) beside the escalation it offers, so the operator is nudged away
// from the unsandboxed run when only the far side needs elevation. Dropping
// the sentence must red.
func TestSourceLauncherPrivilegedNudge(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/sources/"+name)
	if !strings.Contains(body, "Prefer reaching the host over SSH when only the far side needs elevation") {
		t.Errorf("launcher privileged block missing the sandbox-preserving nudge:\n%s", body)
	}
}

// TestJobRerunDialogShowsPrivilegedPosture is the de-escalation gate: the rerun
// dialog of a job that ran unsandboxed names that fact and offers the
// escalation toggle OFF. A rerun re-escalates only by a deliberate act, never by
// inheriting the original's posture (the handler trusts no client state).
// Dropping the fact, or pre-checking the toggle, must red.
func TestJobRerunDialogShowsPrivilegedPosture(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedTerminalJob(t, h, store.JobArgs{Argv: []string{"/bin/true"}, Privileged: true})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(body, `Job #`+itoa64(job.ID)+` ran <span class="fail-text">unsandboxed</span>`) {
		t.Errorf("rerun dialog must name the original's unsandboxed posture:\n%s", body)
	}
	if !strings.Contains(body, `<input type="checkbox" role="switch" name="privileged" value="1" class="toggle-cb">`) {
		t.Errorf("rerun dialog must offer the privileged toggle unchecked:\n%s", body)
	}
	if strings.Contains(body, `name="privileged" value="1" class="toggle-cb" checked`) {
		t.Error("the privileged toggle must not inherit the original's escalation")
	}
}

// TestJobRerunDialogSandboxedOffersEscalation: a sandboxed job's rerun dialog
// still carries the launcher's shared privileged toggle, OFF by default, and
// claims no unsandboxed fact it does not have.
func TestJobRerunDialogSandboxedOffersEscalation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedTerminalJob(t, h, store.JobArgs{Argv: []string{"/bin/true"}})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if strings.Contains(body, `ran <span class="fail-text">unsandboxed</span>`) {
		t.Error("a sandboxed job's rerun dialog must not claim it ran unsandboxed")
	}
	if !strings.Contains(body, `<input type="checkbox" role="switch" name="privileged" value="1" class="toggle-cb">`) {
		t.Errorf("rerun dialog must offer the privileged toggle:\n%s", body)
	}
}
