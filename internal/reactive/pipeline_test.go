package reactive_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// TestAlertmanagerWebhookReachesTheQueue is the pipeline end to end: a real
// Alertmanager-shaped POST, through the derived hook token, the normalizer,
// a CEL rule and the guardrails, into a job the daemon runs.
func TestAlertmanagerWebhookReachesTheQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, shellRule("restart-node-exporter"))
	srv := h.serve(t, "")

	body := alertmanagerBody("DiskFull", "web-01", "firing")
	url := srv.URL + reactive.HookPrefix + reactive.HookToken(h.deriver, reactive.SourceAlertmanager)
	code, response := post(t, srv.Client(), url, body, map[string]string{
		reactive.SignatureHeader: reactive.SignBody(h.deriver, reactive.SourceAlertmanager, body),
	})
	if code != 202 {
		t.Fatalf("hook answered %d: %s", code, response)
	}
	if !strings.Contains(response, `"outcome":"fired"`) {
		t.Fatalf("hook response = %s, want a fired outcome", response)
	}

	event := lastEvent(t, h.store)
	if event.Outcome != store.OutcomeFired || event.MatchedRule != "restart-node-exporter" {
		t.Fatalf("event = %+v, want fired by restart-node-exporter", event)
	}
	if event.JobID == nil {
		t.Fatal("a fired event names no job")
	}
	if event.DedupKey != "alertmanager:fp:fp-DiskFull-web-01" {
		t.Fatalf("dedup key = %q", event.DedupKey)
	}

	job, err := h.store.GetJob(ctx, *event.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != store.StateQueued || job.Trigger != reactive.Trigger {
		t.Fatalf("job = %+v, want a queued job triggered by an event", job)
	}

	// The daemon runs it like anything else that reached the queue.
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.queue.Run(runCtx) }()
	waitForJobState(t, h.store, job.ID, store.StateSucceeded)
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("queue run: %v", err)
	}
}

func TestUnknownHookTokenIsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	srv := h.serve(t, "")

	code, _ := post(t, srv.Client(), srv.URL+reactive.HookPrefix+"0123456789abcdef0123",
		alertmanagerBody("DiskFull", "web-01", "firing"), nil)
	if code != 404 {
		t.Fatalf("unknown hook token answered %d, want 404", code)
	}
	events, err := h.store.ListEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("an unauthenticated post recorded %d events", len(events))
	}
}

// TestWrongSignatureIsRefused: a sender that signs must sign correctly. The
// path token alone is not enough once a signature is on the wire, because a
// body rewritten in flight is exactly what the signature exists to catch.
func TestWrongSignatureIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	h.addRule(t, shellRule("restart"))
	srv := h.serve(t, "")

	body := alertmanagerBody("DiskFull", "web-01", "firing")
	url := srv.URL + reactive.HookPrefix + reactive.HookToken(h.deriver, reactive.SourceAlertmanager)

	// A signature over a different body.
	wrong := reactive.SignBody(h.deriver, reactive.SourceAlertmanager, []byte("something else"))
	code, _ := post(t, srv.Client(), url, body, map[string]string{reactive.SignatureHeader: wrong})
	if code != 401 {
		t.Fatalf("wrong signature answered %d, want 401", code)
	}

	// A signature made under another source's subkey.
	foreign := reactive.SignBody(h.deriver, reactive.SourceGeneric, body)
	code, _ = post(t, srv.Client(), url, body, map[string]string{reactive.SignatureHeader: foreign})
	if code != 401 {
		t.Fatalf("foreign-key signature answered %d, want 401", code)
	}

	events, err := h.store.ListEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a refused post recorded %d events", len(events))
	}
}

func TestCIDRAllowlistRefusesAStranger(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	h.addRule(t, shellRule("restart"))

	// httptest connects from loopback, which this list excludes.
	srv := h.serve(t, "10.0.0.0/8, 192.168.0.0/16")
	url := srv.URL + reactive.HookPrefix + reactive.HookToken(h.deriver, reactive.SourceAlertmanager)
	code, _ := post(t, srv.Client(), url, alertmanagerBody("DiskFull", "web-01", "firing"), nil)
	if code != 403 {
		t.Fatalf("post from outside the allowlist answered %d, want 403", code)
	}

	// The same request from an allowed range gets through.
	open := h.serve(t, "127.0.0.0/8, ::1/128")
	openURL := open.URL + reactive.HookPrefix + reactive.HookToken(h.deriver, reactive.SourceAlertmanager)
	code, _ = post(t, open.Client(), openURL, alertmanagerBody("DiskFull", "web-01", "firing"), nil)
	if code != 202 {
		t.Fatalf("post from inside the allowlist answered %d, want 202", code)
	}
}

func TestUnmatchedEventIsStillRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, harnessOptions{})
	h.addRule(t, shellRule("restart"))

	stored, err := h.engine.Ingest(ctx, h.firing(t, "CertExpiring", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stored.Outcome != store.OutcomeIgnored || stored.Reason != "no rule matched" {
		t.Fatalf("event = %+v, want ignored with a reason", stored)
	}
}

// TestARuleThatWillNotEvaluateDoesNotFire: an expression that errors is not a
// match. A rule that cannot decide must not be able to fire anything.
func TestARuleThatWillNotEvaluateDoesNotFire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, harnessOptions{})

	// Compiles (dyn typing defers it) and fails at evaluation: a label is a
	// string, and adding an int to one is a runtime error.
	if _, err := h.store.CreateRule(ctx, store.NewRule{
		Name:       "broken",
		CELExpr:    `event.labels["severity"] + 1 == 2`,
		ActionKind: store.ActionShell,
		Action:     store.RuleAction{Argv: []string{"/bin/true"}},
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	stored, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stored.Outcome != store.OutcomeIgnored {
		t.Fatalf("event outcome = %s, want ignored", stored.Outcome)
	}
	if !strings.Contains(stored.Trace, "evaluation failed") {
		t.Fatalf("trace does not record the evaluation failure: %s", stored.Trace)
	}
}

func TestCompileRefusesNonBooleanAndUnknownFields(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		expr string
	}{
		{"not a boolean", `event.name`},
		{"unknown variable", `alert.name == "DiskFull"`},
		{"syntax error", `event.name ==`},
		{"empty", ``},
	} {
		if _, err := reactive.Compile(tc.expr); err == nil {
			t.Fatalf("%s: %q compiled, want a refusal", tc.name, tc.expr)
		}
	}

	if _, err := reactive.Compile(`event.labels["severity"] == "critical" && event.host != ""`); err != nil {
		t.Fatalf("a valid expression was refused: %v", err)
	}
}

// TestEvaluationIsTimeBudgeted: a comprehension over a big list is stopped by
// the cost limit rather than running for as long as it likes. Arbitrary input
// must not be able to hold the ingress open.
func TestEvaluationIsTimeBudgeted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})

	prg, err := reactive.Compile(
		`size(event.labels) >= 0 && [1,2,3,4,5,6,7,8,9,10].all(a, ` +
			`[1,2,3,4,5,6,7,8,9,10].all(b, [1,2,3,4,5,6,7,8,9,10].all(c, ` +
			`[1,2,3,4,5,6,7,8,9,10].all(d, [1,2,3,4,5,6,7,8,9,10].all(e, ` +
			`a + b + c + d + e > 0)))))`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	start := time.Now()
	_, err = prg.Match(context.Background(), h.firing(t, "DiskFull", "web-01"))
	if err == nil {
		t.Fatal("a 100k-iteration comprehension evaluated, want the budget to stop it")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("evaluation took %s before the budget stopped it", elapsed)
	}
}
