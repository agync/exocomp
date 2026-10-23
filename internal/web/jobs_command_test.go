package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// redactorOf is a Redactor option that scrubs the given known values.
func redactorOf(known [][]byte) func(context.Context) (*secrets.Redactor, error) {
	return func(context.Context) (*secrets.Redactor, error) { return secrets.NewRedactor(known), nil }
}

// seedAdhoc parks an ad-hoc ansible job in awaiting_approval, where no worker
// claims it, so the list renders exactly the argv the test wrote.
func seedAdhoc(t *testing.T, h *harness, runbook, source string, argv []string) store.Job {
	t.Helper()
	job, err := h.store.CreateGatedJob(context.Background(), store.NewJob{
		Runbook: runbook, Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{Argv: argv, Source: source},
	})
	if err != nil {
		t.Fatalf("seed ad-hoc job: %v", err)
	}
	return job
}

// TestJobsListNamesAdhocCommand: an ad-hoc row reads "module · host" over its
// command text, taken from the argv by flag name, so a trailing --become or a
// jinja msg= argument never becomes the title.
func TestJobsListNamesAdhocCommand(t *testing.T) {
	h := newHarness(t)
	h.server.redactor = redactorOf(nil)
	seedAdhoc(t, h, "command", "src-a",
		executor.AnsibleAdhocArgv("host-a", "/inv/hosts", "command", "systemctl status foo", "--become"))
	seedAdhoc(t, h, "ansible", "", executor.AnsibleAdhocArgv("host-b", "", "debug", "msg={{ inventory_hostname }}"))
	seedAdhoc(t, h, "ping", "", executor.AnsibleAdhocArgv("host-c", "", "ping", ""))

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, body := getBody(t, client, ts.URL+"/jobs")
	if code != 200 {
		t.Fatalf("jobs status = %d", code)
	}
	for _, want := range []string{
		`<b class="mono">command · host-a</b>`,
		`<small class="mono cmd" title="systemctl status foo">systemctl status foo</small>`,
		`<small class="mono src">src-a</small>`,
		`<b class="mono">debug · host-b</b>`,
		`msg={{ inventory_hostname }}`,
		`<b class="mono">ping · host-c</b>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jobs list missing %q", want)
		}
	}
	for _, bad := range []string{`<b class="mono">--become</b>`, `<b class="mono">command</b>`, `<b class="mono">msg=`} {
		if strings.Contains(body, bad) {
			t.Errorf("jobs list still carries a positional-guess title %q", bad)
		}
	}
}

// TestJobsListRedactsCommand: a sealed run-secret value in the args is
// scrubbed from the list row and its hover text, and from the job detail's
// Command tile. A surface with no redactor shows no command text at all.
func TestJobsListRedactsCommand(t *testing.T) {
	const secret = "tok-4f9a1c2e7b-run-secret"
	h := newHarness(t)
	h.server.redactor = redactorOf([][]byte{[]byte(secret)})
	job := seedAdhoc(t, h, "command", "",
		executor.AnsibleAdhocArgv("host-a", "", "shell", "curl -H 'Authorization: "+secret+"' https://svc.example/health"))

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, body := getBody(t, client, ts.URL+"/jobs")
	if code != 200 {
		t.Fatalf("jobs status = %d", code)
	}
	if strings.Contains(body, secret) {
		t.Error("the jobs list renders the unredacted secret")
	}
	if !strings.Contains(body, "Authorization: [REDACTED]") {
		t.Error("the jobs list does not show the redacted command")
	}

	// The detail's Command tile is scrubbed too. The Schedule dialog on the
	// same page replays the exact argv and is not part of this change.
	_, detail := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	_, tile, found := strings.Cut(detail, "<span>Command</span>")
	if !found {
		t.Fatal("job detail has no Command tile")
	}
	tile, _, _ = strings.Cut(tile, "</div>")
	if strings.Contains(tile, secret) || !strings.Contains(tile, "Authorization: [REDACTED]") {
		t.Errorf("Command tile is not redacted: %s", tile)
	}
}

// TestJobDetailShowsCommand: the detail's RUN CONFIGURATION names the command,
// with the full text as the tile's hover.
func TestJobDetailShowsCommand(t *testing.T) {
	h := newHarness(t)
	h.server.redactor = redactorOf(nil)
	job := seedAdhoc(t, h, "command", "", executor.AnsibleAdhocArgv("host-a", "", "command", "uptime -p"))
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	for _, want := range []string{`<span>Command</span>`, `<b title="uptime -p">uptime -p</b>`, `command · host-a`} {
		if !strings.Contains(body, want) {
			t.Errorf("job detail missing %q", want)
		}
	}
}

// TestCommandTextFailsClosed: command text is shown only to an operator, and
// only once a redactor resolved from a complete secret set. A missing
// redactor, a resolve error (a secret that would not open), a nil filter and a
// non-operator token each leave the title and drop the command text, in the
// list and in the detail.
func TestCommandTextFailsClosed(t *testing.T) {
	const marker = "marker-cmd-not-for-display"
	cases := []struct {
		name     string
		scope    string
		redactor func(context.Context) (*secrets.Redactor, error)
	}{
		{"no redactor", "operator", nil},
		{"incomplete secret set", "operator", func(context.Context) (*secrets.Redactor, error) {
			// A non-nil filter beside the error pins that the error is checked.
			return secrets.NewRedactor(nil), errors.New("1 secrets did not open for redaction")
		}},
		{"nil filter", "operator", func(context.Context) (*secrets.Redactor, error) { return nil, nil }},
		{"readonly token", "readonly", redactorOf(nil)},
		{"approve token", "approve", redactorOf(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.server.redactor = tc.redactor
			job := seedAdhoc(t, h, "command", "", executor.AnsibleAdhocArgv("host-a", "", "shell", "echo "+marker))
			ts := httptest.NewServer(h.server.Handler())
			defer ts.Close()
			client := login(t, ts.URL, h.mint(t, "viewer", tc.scope))

			_, list := getBody(t, client, ts.URL+"/jobs")
			if strings.Contains(list, marker) || !strings.Contains(list, `<b class="mono">shell · host-a</b>`) {
				t.Errorf("jobs list must keep the title and drop the command text")
			}
			_, detail := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
			// The operator's Schedule dialog echoes the argv by design, so the
			// detail check is on the Command tile alone.
			if strings.Contains(detail, "<span>Command</span>") {
				t.Error("job detail must not render a Command tile")
			}
		})
	}
}
