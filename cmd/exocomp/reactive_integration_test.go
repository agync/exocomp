package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestReactivePipelineEndToEnd is the reactive gate as one run of the real
// binary: an Alertmanager-shaped POST reaches the daemon's hook, a CEL rule
// matches it, the guardrails gate the action behind an approval, the decision
// link releases it, the daemon runs the job, and the funnel is readable off
// `event list`.
func TestReactivePipelineEndToEnd(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	bin := buildExocomp(t)
	runCLI(t, bin, "init")

	// The action's side effect: a file only the released job creates.
	marker := filepath.Join(t.TempDir(), "restarted")
	runCLI(t, bin, "rule", "add", "restart-exporter",
		"--expr", `event.name == "DiskFull" && event.labels["severity"] == "critical"`,
		"--action", "shell", "--require-approval", "--cooldown", "1h", "--daily-cap", "5",
		"--", "/usr/bin/touch", marker)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0",
		"--approval-ttl", "10m", "--dedup-window", "5m")
	line := d.waitForLine(t, &d.stdout, "state backend listening on http://", 30*time.Second)
	addr := strings.TrimPrefix(strings.TrimSpace(line), "state backend listening on http://")
	hook := d.waitForLine(t, &d.stdout, "hook alertmanager", 10*time.Second)
	hookURL := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(hook), "hook alertmanager"))
	d.waitForLine(t, &d.stdout, "scheduler started", 10*time.Second)

	if !strings.HasPrefix(hookURL, "http://"+addr+"/hook/") {
		t.Fatalf("hook URL %q is not on the daemon's listener", hookURL)
	}

	// An unauthenticated POST reaches nothing.
	if code, _ := postJSON(t, "http://"+addr+"/hook/deadbeefdeadbeefdead",
		alertPayload("DiskFull", "web-01", "firing")); code != http.StatusNotFound {
		t.Fatalf("post to a guessed hook answered %d, want 404", code)
	}

	// The real one gates.
	code, body := postJSON(t, hookURL, alertPayload("DiskFull", "web-01", "firing"))
	if code != http.StatusAccepted {
		t.Fatalf("hook answered %d: %s", code, body)
	}
	if !strings.Contains(body, `"outcome":"gated"`) {
		t.Fatalf("hook response = %s, want a gated outcome", body)
	}

	// A repeat inside the dedup window is swallowed, and says so.
	code, body = postJSON(t, hookURL, alertPayload("DiskFull", "web-01", "firing"))
	if code != http.StatusAccepted || !strings.Contains(body, `"outcome":"suppressed"`) {
		t.Fatalf("repeat answered %d: %s", code, body)
	}
	if !strings.Contains(body, "duplicate of event") {
		t.Fatalf("suppressed reason does not name the duplicate: %s", body)
	}

	// The job exists and nothing can run it yet.
	approvals, _ := runCLIExit(t, bin, "approval", "list")
	if !strings.Contains(approvals, "pending") {
		t.Fatalf("approval list = %s", approvals)
	}
	gated := waitForApproval(t)
	if state := jobState(t, gated.JobID); state != store.StateAwaitingApproval {
		t.Fatalf("gated job is %s, want awaiting_approval", state)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the gated action ran before the decision (stat %s: %v)", marker, err)
	}

	links, _ := runCLIExit(t, bin, "approval", "link", fmt.Sprint(gated.ID))
	approveURL, rejectURL := decisionLinks(t, links)

	// A tampered token decides nothing.
	if code, _ := getURL(t, approveURL+"x"); code != http.StatusForbidden {
		t.Fatalf("tampered token answered %d, want 403", code)
	}

	code, body = getURL(t, approveURL)
	if code != http.StatusOK || !strings.Contains(body, `"outcome":"approved"`) {
		t.Fatalf("approve answered %d: %s", code, body)
	}

	// Released, claimed by the daemon, run.
	waitFor(t, "the approved job to succeed", 30*time.Second, func() bool {
		return jobState(t, gated.JobID) == store.StateSucceeded
	})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the approved action did not run: %v", err)
	}

	// The link is spent: the second tap is a terminal notice, not a second
	// decision.
	code, body = getURL(t, rejectURL)
	if code != http.StatusOK || !strings.Contains(body, "already been used") {
		t.Fatalf("reused link answered %d: %s", code, body)
	}

	// The funnel is readable, and so is the trace behind each row.
	events, _ := runCLIExit(t, bin, "event", "list")
	for _, want := range []string{"gated", "suppressed", "restart-exporter"} {
		if !strings.Contains(events, want) {
			t.Fatalf("event list does not show %q:\n%s", want, events)
		}
	}
	explained, _ := runCLIExit(t, bin, "event", "explain", "1")
	for _, want := range []string{"trace:", "silence", "dedup", "cooldown", "cap"} {
		if !strings.Contains(explained, want) {
			t.Fatalf("event explain does not show %q:\n%s", want, explained)
		}
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
}

// TestGlobalSilenceSwallowsAndSaysSo drives the kill switch through the CLI
// and the hook: it needs --yes-really and an expiry, and while it is up every
// event is swallowed with a count.
func TestGlobalSilenceSwallowsAndSaysSo(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	bin := buildExocomp(t)
	runCLI(t, bin, "init")
	runCLI(t, bin, "rule", "add", "restart", "--expr", `event.name == "DiskFull"`,
		"--action", "shell", "--", "/bin/true")

	// Both refusals: no expiry, and no confirmation.
	if out, code := runCLIExit(t, bin, "silence", "add", "--global", "--yes-really"); code == 0 {
		t.Fatalf("a global silence with no --for succeeded: %s", out)
	}
	out, code := runCLIExit(t, bin, "silence", "add", "--global", "--for", "1h")
	if code == 0 {
		t.Fatalf("a global silence without --yes-really succeeded: %s", out)
	}
	if !strings.Contains(out, "running jobs still finish") {
		t.Fatalf("the global-silence refusal does not say what it does not touch: %s", out)
	}

	runCLI(t, bin, "silence", "add", "--global", "--for", "1h", "--yes-really",
		"--reason", "rebooting the rack")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "state backend listening on http://", 30*time.Second)
	hook := d.waitForLine(t, &d.stdout, "hook alertmanager", 10*time.Second)
	hookURL := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(hook), "hook alertmanager"))

	code, body := postJSON(t, hookURL, alertPayload("DiskFull", "web-01", "firing"))
	if code != http.StatusAccepted || !strings.Contains(body, `"outcome":"suppressed"`) {
		t.Fatalf("post under a global silence answered %d: %s", code, body)
	}
	if !strings.Contains(body, "global pause") || !strings.Contains(body, "swallowed") {
		t.Fatalf("suppressed reason = %s, want the scope and the count", body)
	}

	listed, _ := runCLIExit(t, bin, "silence", "list")
	if !strings.Contains(listed, "global pause") || !strings.Contains(listed, "rebooting the rack") {
		t.Fatalf("silence list = %s", listed)
	}
}

func alertPayload(alertname, instance, status string) []byte {
	body, err := json.Marshal(map[string]any{
		"version": "4", "status": status, "receiver": "exocomp",
		"commonLabels": map[string]string{"alertname": alertname, "severity": "critical"},
		"alerts": []map[string]any{{
			"status": status,
			"labels": map[string]string{
				"alertname": alertname, "instance": instance + ":9100", "severity": "critical",
			},
			"annotations": map[string]string{"summary": "the disk is full"},
			"fingerprint": "fp-" + alertname + "-" + instance,
		}},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func postJSON(t *testing.T, url string, body []byte) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(t, req)
}

func getURL(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return doRequest(t, req)
}

func doRequest(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// decisionLinks pulls the two URLs out of `approval link`'s output.
func decisionLinks(t *testing.T, out string) (approve, reject string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "approve":
			approve = fields[1]
		case "reject":
			reject = fields[1]
		}
	}
	if approve == "" || reject == "" {
		t.Fatalf("approval link printed no decision links:\n%s", out)
	}
	return approve, reject
}

// waitForApproval blocks until exactly one approval is pending and returns it.
func waitForApproval(t *testing.T) store.Approval {
	t.Helper()

	var found store.Approval
	waitFor(t, "an approval to be created", 20*time.Second, func() bool {
		st := openTestStore(t)
		defer func() { _ = st.Close() }()

		rows, err := st.ListApprovals(context.Background(), true)
		if err != nil {
			t.Fatalf("list approvals: %v", err)
		}
		if len(rows) == 0 {
			return false
		}
		found = rows[0]
		return true
	})
	return found
}

// openTestStore opens the daemon's database from a test. The daemon holds it
// too; SQLite's WAL mode and the store's busy timeout are what make that a
// wait rather than a failure.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir, err := dataDir()
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}
