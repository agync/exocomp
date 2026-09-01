package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/wire"
)

// runRoot executes the full root command with args and captured streams, so a
// test drives client mode exactly as a shell would (persistent --remote/--token
// flags resolved, remoteClient consulted).
func runRoot(t *testing.T, args []string) (string, string, error) {
	t.Helper()
	root := newRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errb.String(), err
}

// pinnedRemote starts a TLS API stub and pins it under alias "box" in an
// isolated hosts.toml, returning the server so the test can assert what it served.
func pinnedRemote(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	withConfigHome(t)
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	fp := client.Fingerprint(srv.Certificate().Raw)
	cfg := hostsConfig{Default: "box", Hosts: map[string]hostEntry{
		"box": {Endpoint: srv.URL, Fingerprint: fp, Token: "test-token"},
	}}
	if err := saveHostsConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return srv
}

var sampleJobs = []wire.JobView{
	{ID: 1, State: "running", Runbook: "web", Kind: "playbook", QueuedAt: "2026-08-22T10:00:00Z"},
	{ID: 2, State: "queued", Runbook: "db", QueuedAt: "2026-08-22T10:05:00Z"},
}

func jobsAPIStub(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = wire.WriteErrorValue(w, wire.NewError(wire.CodeUnauthenticated, "no bearer"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindJobList, sampleJobs)
	})
	return mux
}

// The GATE: `--remote box jobs list --output json` renders the SAME bytes the
// local path would for the same rows.
func TestRemoteJobsListJSONMatchesLocal(t *testing.T) {
	pinnedRemote(t, jobsAPIStub(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "jobs", "list", "--output", "json"})
	if err != nil {
		t.Fatalf("remote jobs list: %v", err)
	}
	// The remote path decodes the daemon's envelope into wire.JobView and
	// re-emits through the SAME wire marshaller the local --output json uses, so
	// the bytes must equal what that marshaller produces for the same rows.
	want, err := wire.MarshalSuccess(wire.KindJobList, sampleJobs)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("remote JSON != local projection:\n remote: %q\n local:  %q", out, want)
	}
}

// Env selection (EXOCOMP_REMOTE) activates client mode just like the flag.
func TestRemoteViaEnv(t *testing.T) {
	pinnedRemote(t, jobsAPIStub(t))
	t.Setenv(remoteEnv, "box")

	out, _, err := runRoot(t, []string{"jobs", "list", "--output", "json"})
	if err != nil {
		t.Fatalf("env remote: %v", err)
	}
	if !strings.Contains(out, `"kind":"job.list"`) || !strings.Contains(out, `"runbook":"web"`) {
		t.Fatalf("did not hit the remote: %s", out)
	}
}

// A bare command with NO --remote and NO env stays LOCAL: it opens the local data
// dir (here an empty temp one) and reports no jobs, never touching the pinned
// remote.
func TestBareCommandStaysLocal(t *testing.T) {
	pinnedRemote(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("bare command must NOT contact the remote")
	}))
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	out, _, err := runRoot(t, []string{"jobs", "list"})
	if err != nil {
		t.Fatalf("local jobs list: %v", err)
	}
	if strings.TrimSpace(out) != "no jobs" {
		t.Fatalf("expected local empty listing, got %q", out)
	}
}

// A pinned-cert mismatch is refused: the stored fingerprint does not match the
// server's cert, so the connection is rejected before any data is read.
func TestRemotePinMismatchRefused(t *testing.T) {
	withConfigHome(t)
	srv := httptest.NewTLSServer(jobsAPIStub(t))
	defer srv.Close()
	cfg := hostsConfig{Hosts: map[string]hostEntry{
		"box": {
			Endpoint:    srv.URL,
			Fingerprint: "00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF",
			Token:       "test-token",
		},
	}}
	if err := saveHostsConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_, _, err := runRoot(t, []string{"--remote", "box", "jobs", "list"})
	if err == nil {
		t.Fatal("pin mismatch must refuse")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("expected a fingerprint refusal, got %v", err)
	}
}

// TestRemoteHTTPEndpointRefused: a pinned entry whose endpoint is http:// (a
// manually edited hosts.toml, or an alias re-pointed by hand) is refused
// before any request goes out — otherwise the bearer token and every
// run-secret a remote apply carries would go out in cleartext. Enforced at
// internal/client.baseURL, which remoteClient's call to client.New always
// goes through: the one construction path every --remote verb shares.
func TestRemoteHTTPEndpointRefused(t *testing.T) {
	withConfigHome(t)
	cfg := hostsConfig{Hosts: map[string]hostEntry{
		"box": {
			Endpoint:    "http://box.tailnet.ts.net:8443",
			Fingerprint: "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88",
			Token:       "test-token",
		},
	}}
	if err := saveHostsConfig(cfg); err != nil {
		t.Fatal(err)
	}

	_, _, err := runRoot(t, []string{"--remote", "box", "jobs", "list"})
	if err == nil {
		t.Fatal("http:// pinned endpoint must be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("expected an https refusal naming the reason, got %v", err)
	}
}

// An unpinned selector (no login) is refused rather than silently connecting.
func TestRemoteUnpinnedRefused(t *testing.T) {
	withConfigHome(t)
	_, _, err := runRoot(t, []string{"--remote", "never-logged-in", "jobs", "list"})
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("want not-pinned refusal, got %v", err)
	}
}

// The ansible --host pattern is a DIFFERENT axis from --remote and must not
// activate client mode: it selects which managed nodes a play targets, not which
// daemon the CLI talks to.
func TestHostFlagDistinctFromRemote(t *testing.T) {
	root := newRootCmd()
	if root.PersistentFlags().Lookup(remoteFlag) == nil {
		t.Fatal("root is missing the global --remote flag")
	}
	// --host is NOT a global flag; it is local to command/adhoc.
	if root.PersistentFlags().Lookup("host") != nil {
		t.Fatal("--host must not be a global flag (it is the ansible host pattern)")
	}
	var adhoc *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "command" {
			adhoc = c
		}
	}
	if adhoc == nil {
		t.Fatal("no `command` verb")
	}
	if adhoc.Flags().Lookup("host") == nil {
		t.Error("`command` should carry the ansible --host pattern flag")
	}
	// Setting --host on the command verb does not enter remote mode: selectedRemote
	// keys only on --remote/EXOCOMP_REMOTE.
	if _, active := selectedRemote(adhoc); active {
		t.Error("--host must not activate remote client mode")
	}
}

// TestRemoteJobsListShowsRawRunbook:the remote list prints the raw stored
// runbook in the RUNBOOK column — the exact value --runbook matches (rule:
// prefix included), matching the local list — while the get detail keeps both
// the stripped display name and a raw line.
func TestRemoteJobsListShowsRawRunbook(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	job := wire.JobView{ID: 7, State: "queued", Runbook: "rule:deploy-app", Kind: "tofu", QueuedAt: "2026-08-22T10:00:00Z"}
	if err := renderRemoteJobsList(cmd, outputText, []wire.JobView{job}); err != nil {
		t.Fatalf("render remote jobs list: %v", err)
	}
	if !strings.Contains(out.String(), "rule:deploy-app") {
		t.Fatalf("remote jobs list missing the raw runbook value: %q", out.String())
	}

	out.Reset()
	if err := renderRemoteJobGet(cmd, outputText, job); err != nil {
		t.Fatalf("render remote job get: %v", err)
	}
	if !strings.Contains(out.String(), "deploy-app") {
		t.Fatalf("remote job get missing the stripped runbook name: %q", out.String())
	}
	// The raw stored runbook is printed too, so an operator who needs the exact
	// key --runbook filters on can read it off the detail page.
	if !strings.Contains(out.String(), "runbook (raw)") || !strings.Contains(out.String(), "rule:deploy-app") {
		t.Fatalf("remote job get missing the raw runbook line: %q", out.String())
	}
}
