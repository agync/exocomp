package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// seedFailedShell writes a terminal FAILED shell job straight into the store:
// the queue is not told, so the rows below are the whole truth the detail page
// reads. CreateGatedJob lands the row in awaiting_approval, a state no harness
// worker claims, and the transition is done directly so a live runner can never
// grab it first.
func seedFailedShell(t *testing.T, h *harness, argv []string) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateGatedJob(ctx, store.NewJob{
		Runbook: "command", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: argv},
	})
	if err != nil {
		t.Fatalf("seed shell job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateAwaitingApproval, store.StateRunning); err != nil {
		t.Fatalf("start seeded shell job: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "failed (exit 2)",
	}); err != nil {
		t.Fatalf("append failed band: %v", err)
	}
	if err := h.store.SetJobExitCode(ctx, job.ID, 2); err != nil {
		t.Fatalf("set exit code: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateFailed); err != nil {
		t.Fatalf("finish seeded shell job: %v", err)
	}
	return job
}

// TestVerdictSentenceNonAnsibleKindIsHonest is the copy gate: a FAILED job
// whose kind never ran Ansible (job #1 is kind=shell, argv /bin/sh -c exit 2)
// must not read as an Ansible reachability failure. Only the ansible kind keeps
// the host sentences.
func TestVerdictSentenceNonAnsibleKindIsHonest(t *testing.T) {
	shell := verdictSentence(store.Job{Kind: "shell", State: store.StateFailed}, jobOutcomeFacts{})
	if strings.Contains(shell, "Ansible") {
		t.Errorf("a shell job's verdict must not name Ansible, got %q", shell)
	}
	if !strings.Contains(shell, "The command failed") {
		t.Errorf("a shell job's verdict must name the process outcome, got %q", shell)
	}

	tofu := verdictSentence(store.Job{Kind: "tofu", State: store.StateFailed}, jobOutcomeFacts{})
	if strings.Contains(tofu, "Ansible") {
		t.Errorf("a tofu job's verdict must not name Ansible, got %q", tofu)
	}

	ansible := verdictSentence(store.Job{Kind: "ansible", State: store.StateFailed}, jobOutcomeFacts{})
	if !strings.Contains(ansible, "Ansible could not reach the target.") {
		t.Errorf("an ansible job keeps its host sentence, got %q", ansible)
	}
}

// TestJobDetailFailedShellSentenceNotAnsible renders the failed shell job's
// detail page and asserts the verdict band's sentence is the process outcome,
// never the Ansible reachability line a non-ansible run could not have hit.
func TestJobDetailFailedShellSentenceNotAnsible(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedShell(t, h, []string{"/bin/sh", "-c", "exit 2"})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if code != http.StatusOK {
		t.Fatalf("/jobs/%d = %d, want 200", job.ID, code)
	}

	bandAt := strings.Index(body, `verdict-band--failed`)
	if bandAt < 0 {
		t.Fatalf("failed shell detail has no verdict band:\n%s", body)
	}
	band := body[bandAt:]
	i := strings.Index(band, `<p class="sentence">`)
	if i < 0 {
		t.Fatalf("failed shell detail has no verdict sentence:\n%s", body)
	}
	rest := band[i+len(`<p class="sentence">`):]
	j := strings.Index(rest, "</p>")
	if j < 0 {
		t.Fatalf("verdict sentence is not closed:\n%s", body)
	}
	sentence := rest[:j]
	if strings.Contains(sentence, "Ansible") {
		t.Errorf("failed shell job's verdict sentence names Ansible: %q", sentence)
	}
	if !strings.Contains(sentence, "The command failed") {
		t.Errorf("failed shell job's verdict sentence is not the honest replacement: %q", sentence)
	}
}

// TestChecksNoNetgroupReadsAbsence is the /checks location gate: a target with
// no netgroup at all must read an honest absence, while a real netgroup — and
// the store's "away" miss label — keep their location prose. Rendering the
// empty netgroup as "on away" claims a network location the target does not
// have.
func TestChecksNoNetgroupReadsAbsence(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "roaming", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "10.0.0.5", Port: 22},
		},
	})
	if err != nil {
		t.Fatalf("seed check: %v", err)
	}
	now := time.Now().UTC()
	for _, st := range []store.CheckState{
		{CheckID: c.ID, TargetName: "laptop", Status: store.CheckUp,
			Consecutive: 3, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
			LastDurationMS: 12},
		{CheckID: c.ID, TargetName: "nas", Status: store.CheckDown,
			Consecutive: 3, Emitted: store.CheckDown, FirstSeen: now, LastChange: now,
			Netgroup: "away", LastDurationMS: 30},
	} {
		if err := h.store.SetCheckState(ctx, st); err != nil {
			t.Fatalf("SetCheckState %s: %v", st.TargetName, err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks = %d, want 200", code)
	}

	card := page[strings.Index(page, "roaming"):]
	laptop := card[strings.Index(card, `<span class="mono">laptop</span>`):]
	nas := laptop[strings.Index(laptop, `<span class="mono">nas</span>`):]
	if !strings.Contains(laptop[:strings.Index(laptop, `<span class="mono">nas</span>`)], `<span class="meta">no netgroup</span>`) {
		t.Errorf("a target with no netgroup must read an honest absence:\n%s", laptop)
	}
	if strings.Contains(laptop[:strings.Index(laptop, `<span class="mono">nas</span>`)], "on away") {
		t.Error("a target with no netgroup rendered the away location")
	}
	if !strings.Contains(nas, "on away") {
		t.Errorf("the store's away miss label keeps its location prose:\n%s", nas)
	}
}
