package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// writeAPIStub is a stand-in for the daemon's write + follow surface: it records
// what the CLI POSTed and answers the SAME envelopes internal/web/api_write.go
// does, so a test asserts the request AND the rendering in one run.
type writeAPIStub struct {
	mu sync.Mutex

	runBody     map[string]any
	runIdem     string
	planBody    map[string]any
	planIdem    string
	applyBody   map[string]any
	destroyBody map[string]any
	destroyIdem string
	decision    map[string]any
	streams     int

	// applyGated makes an apply answer with the pending envelope.
	applyGated bool
	// jobState is what the job settles as, once the stream is done.
	jobState string
	// outcome is what a decision settles as.
	outcome string
	message string
}

const stubJobID = 9

func (s *writeAPIStub) body(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	out := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("body %q: %v", raw, err)
		}
	}
	return out
}

func (s *writeAPIStub) handler(t *testing.T) http.Handler {
	t.Helper()
	if s.jobState == "" {
		s.jobState = "succeeded"
	}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/playbooks/run", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.runBody, s.runIdem = s.body(t, r), r.Header.Get("Idempotency-Key")
		s.mu.Unlock()
		_ = wire.WriteSuccess(w, client.KindJobEnqueued,
			client.JobEnqueued{JobID: stubJobID, State: "queued"})
	})
	mux.HandleFunc("POST /api/v1/workspaces/{name}/plan", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.planBody, s.planIdem = s.body(t, r), r.Header.Get("Idempotency-Key")
		s.mu.Unlock()
		_ = wire.WriteSuccess(w, client.KindJobEnqueued,
			client.JobEnqueued{JobID: stubJobID, State: "queued"})
	})
	mux.HandleFunc("POST /api/v1/workspaces/{name}/apply", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.applyBody = s.body(t, r)
		gated := s.applyGated
		s.mu.Unlock()
		if gated {
			_ = wire.WriteSuccess(w, client.KindApprovalPending, client.ApprovalPending{
				ApprovalID: 4, JobID: 12, State: "awaiting_approval",
				ExpiresAt: "2026-08-23T10:30:00Z", TTLSeconds: 1800,
				Policy: "prod-guard", PlanArtifactID: 3,
			})
			return
		}
		_ = wire.WriteSuccess(w, client.KindJobEnqueued,
			client.JobEnqueued{JobID: stubJobID, State: "queued"})
	})
	mux.HandleFunc("POST /api/v1/workspaces/{name}/destroy", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.destroyBody, s.destroyIdem = s.body(t, r), r.Header.Get("Idempotency-Key")
		s.mu.Unlock()
		_ = wire.WriteSuccess(w, client.KindJobEnqueued,
			client.JobEnqueued{JobID: stubJobID, State: "queued"})
	})
	mux.HandleFunc("POST /api/v1/approvals/{id}/decision", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.decision = s.body(t, r)
		outcome, message := s.outcome, s.message
		s.mu.Unlock()
		_ = wire.WriteSuccess(w, client.KindDecision,
			client.Decision{ApprovalID: 4, Outcome: outcome, Message: message})
	})

	mux.HandleFunc("GET /api/v1/jobs/{id}/events", func(w http.ResponseWriter, _ *http.Request) {
		_ = wire.WriteSuccess(w, wire.KindJobEvents, []wire.JobEventView{})
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/stream", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.streams++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w,
			"event: line\ndata: {\"seq\":1,\"ts\":\"2026-08-23T10:00:00Z\",\"level\":\"stdout\",\"msg\":\"PLAY [all]\"}\n\n")
		_, _ = fmt.Fprint(w,
			"event: line\ndata: {\"seq\":2,\"ts\":\"2026-08-23T10:00:01Z\",\"level\":\"stdout\",\"msg\":\"ok: [node0]\"}\n\n")
		_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
		_ = http.NewResponseController(w).Flush()
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		started, finished := "2026-08-23T10:00:00Z", "2026-08-23T10:00:04Z"
		exit := 0
		_ = wire.WriteSuccess(w, wire.KindJob, wire.JobView{
			ID: stubJobID, State: s.jobState, Kind: "playbook", ExitCode: &exit,
			QueuedAt: "2026-08-23T09:59:59Z", StartedAt: &started, FinishedAt: &finished,
		})
	})
	return mux
}

// The north-star shape in miniature: a remote run enqueues over HTTP, the follow
// prints the job's events live, and the command reports the settled job.
func TestRemotePlaybookRunFollows(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "--idempotency-key", "run-1",
		"playbook", "run", "site.yml", "--source", "demo", "--inventory", "lab",
	})
	if err != nil {
		t.Fatalf("remote playbook run: %v", err)
	}
	for _, want := range []string{
		"job 9 queued for the remote daemon",
		"[stdout] PLAY [all]",
		"[stdout] ok: [node0]",
		"job 9  state: succeeded  duration: 4s",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.runBody["playbook"] != "site.yml" || stub.runBody["source"] != "demo" ||
		stub.runBody["inventory"] != "lab" {
		t.Fatalf("posted %+v", stub.runBody)
	}
	if stub.runIdem != "run-1" {
		t.Errorf("Idempotency-Key = %q", stub.runIdem)
	}
	if stub.streams != 1 {
		t.Errorf("stream attaches = %d, want 1", stub.streams)
	}
}

// --no-follow is the enqueue-and-walk-away shape: the id, and no stream at all.
func TestRemotePlaybookRunNoFollow(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "playbook", "run", "site.yml", "--source", "demo", "--no-follow",
	})
	if err != nil {
		t.Fatalf("remote playbook run: %v", err)
	}
	if strings.TrimSpace(out) != "job 9 queued for the remote daemon" {
		t.Fatalf("output = %q", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.streams != 0 {
		t.Errorf("--no-follow must not open the stream (attaches = %d)", stub.streams)
	}
}

// The JSON write envelope is the daemon's own, re-emitted byte for byte.
func TestRemotePlaybookRunJSONEnvelope(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "--output", "json",
		"playbook", "run", "site.yml", "--source", "demo", "--no-follow",
	})
	if err != nil {
		t.Fatalf("remote playbook run: %v", err)
	}
	want, err := wire.MarshalSuccess(client.KindJobEnqueued,
		client.JobEnqueued{JobID: stubJobID, State: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("json = %q, want %q", out, want)
	}
}

// A followed run in JSON mode emits ONE document — the settled job — not the
// event stream a caller did not ask to parse.
func TestRemotePlaybookRunJSONFollowEmitsFinalJob(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "--output", "json", "playbook", "run", "site.yml", "--source", "demo",
	})
	if err != nil {
		t.Fatalf("remote playbook run: %v", err)
	}
	if strings.Count(out, "schema_version") != 1 {
		t.Fatalf("want exactly one envelope, got:\n%s", out)
	}
	if !strings.Contains(out, `"kind":"job.get"`) || !strings.Contains(out, `"state":"succeeded"`) {
		t.Fatalf("json = %s", out)
	}
}

func TestRemoteWorkspacePlanEnqueues(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "--idempotency-key", "plan-1",
		"workspace", "plan", "edge", "--run-secrets", "aws", "--timeout", "90s",
		"--runner", "fedora-ee",
	})
	if err != nil {
		t.Fatalf("remote workspace plan: %v", err)
	}
	if strings.TrimSpace(out) != "job 9 queued for the remote daemon" {
		t.Fatalf("output = %q", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.planBody["run_secrets"] != "aws" || stub.planBody["timeout"] != "1m30s" {
		t.Fatalf("posted %+v", stub.planBody)
	}
	if stub.planBody["runner"] != "fedora-ee" {
		t.Fatalf("posted %+v, want runner = fedora-ee", stub.planBody)
	}
	if stub.planIdem != "plan-1" {
		t.Errorf("Idempotency-Key = %q", stub.planIdem)
	}
}

// TestRemotePlaybookRunCarriesRunner: --runner on a remote playbook run travels
// in the request body, the mirror of the workspace plan/apply seam above.
func TestRemotePlaybookRunCarriesRunner(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "playbook", "run", "site.yml", "--source", "demo",
		"--no-follow", "--runner", "fedora-ee",
	})
	if err != nil {
		t.Fatalf("remote playbook run: %v", err)
	}
	if strings.TrimSpace(out) != "job 9 queued for the remote daemon" {
		t.Fatalf("output = %q", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.runBody["runner"] != "fedora-ee" {
		t.Fatalf("posted %+v, want runner = fedora-ee", stub.runBody)
	}
}

// TestRemotePlaybookRunCarriesEgressPorts: --egress on a remote playbook run
// travels in the request body as egress_ports, so the daemon grants the same
// ports a local run would — a security-posture flag must never go silent on
// the wire.
func TestRemotePlaybookRunCarriesEgressPorts(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "playbook", "run", "site.yml", "--source", "demo",
		"--no-follow", "--egress", "2222",
	})
	if err != nil {
		t.Fatalf("remote playbook run: %v", err)
	}
	if strings.TrimSpace(out) != "job 9 queued for the remote daemon" {
		t.Fatalf("output = %q", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	egress, ok := stub.runBody["egress_ports"]
	if !ok {
		t.Fatalf("posted body %+v carries no egress_ports", stub.runBody)
	}
	list, ok := egress.([]any)
	if !ok || len(list) != 1 || list[0].(float64) != 2222 {
		t.Fatalf("egress_ports = %v, want [2222]", egress)
	}
}

// TestRemotePlaybookRunCarriesSSHIdentity: --ssh-identity on a remote run
// travels as ssh_identity_files, unchanged, for the daemon to check against
// the files its operator allowed.
func TestRemotePlaybookRunCarriesSSHIdentity(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	_, _, err := runRoot(t, []string{
		"--remote", "box", "playbook", "run", "site.yml", "--source", "demo",
		"--no-follow", "--ssh-identity", "/run/keys/fleet/id_ed25519",
	})
	if err != nil {
		t.Fatalf("remote playbook run: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	ids, ok := stub.runBody["ssh_identity_files"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "/run/keys/fleet/id_ed25519" {
		t.Fatalf("ssh_identity_files = %v, want [/run/keys/fleet/id_ed25519]", stub.runBody["ssh_identity_files"])
	}
}

// TestRemoteWorkspaceDestroyEnqueues: `workspace destroy` is the scriptable twin
// of the destroy write route — it POSTs the run-secrets/timeout/runner body to
// /api/v1/workspaces/{name}/destroy and prints the enqueued job id.
func TestRemoteWorkspaceDestroyEnqueues(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "--idempotency-key", "destroy-1",
		"workspace", "destroy", "edge", "--run-secrets", "aws", "--timeout", "90s",
		"--runner", "fedora-ee",
	})
	if err != nil {
		t.Fatalf("remote workspace destroy: %v", err)
	}
	if strings.TrimSpace(out) != "job 9 queued for the remote daemon" {
		t.Fatalf("output = %q", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.destroyBody["run_secrets"] != "aws" || stub.destroyBody["timeout"] != "1m30s" {
		t.Fatalf("posted %+v", stub.destroyBody)
	}
	if stub.destroyBody["runner"] != "fedora-ee" {
		t.Fatalf("posted %+v, want runner = fedora-ee", stub.destroyBody)
	}
	if stub.destroyIdem != "destroy-1" {
		t.Errorf("Idempotency-Key = %q", stub.destroyIdem)
	}
}

// TestRemoteWorkspaceApplyGated reports the approval, its deadline AND its TTL,
// so the operator reads the fail-closed auto-reject as intended.
func TestRemoteWorkspaceApplyGated(t *testing.T) {
	stub := &writeAPIStub{applyGated: true}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "workspace", "apply", "edge", "--plan", "3"})
	if err != nil {
		t.Fatalf("remote workspace apply: %v", err)
	}
	for _, want := range []string{
		`plan 3 requires approval by policy "prod-guard"`,
		"job 12 gated (awaiting_approval); approval 4 expires 2026-08-23 10:30:00Z (in 30m0s)",
		"decide it with `exocomp --remote box approval decide 4 --approve`",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.applyBody["plan"] != "3" {
		t.Fatalf("posted %+v", stub.applyBody)
	}
}

func TestRemoteApprovalDecide(t *testing.T) {
	stub := &writeAPIStub{outcome: "approved", message: "approval 4 approved; job 12 released"}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "approval", "decide", "4", "--approve"})
	if err != nil {
		t.Fatalf("remote approval decide: %v", err)
	}
	if strings.TrimSpace(out) != "approval 4 approved; job 12 released" {
		t.Fatalf("output = %q", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.decision["decision"] != "approve" {
		t.Fatalf("posted %+v", stub.decision)
	}
	// A decision carries no capability: the daemon derives the token itself.
	if _, ok := stub.decision["token"]; ok {
		t.Error("a remote decision must not carry a token")
	}
}

// A decision that settled nothing (already decided, expired) fails closed
// remotely exactly as it does locally, so a script cannot mistake it for fresh.
func TestRemoteApprovalDecideFailsClosed(t *testing.T) {
	stub := &writeAPIStub{outcome: "expired", message: "approval 4 expired; nothing was released"}
	pinnedRemote(t, stub.handler(t))

	_, _, err := runRoot(t, []string{"--remote", "box", "approval", "decide", "4", "--approve"})
	if err == nil {
		t.Fatal("an expired decision must not exit 0")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %v", err)
	}
}

// A remote refusal keeps its taxonomy code in the JSON envelope.
func TestRemoteWriteErrorEnvelope(t *testing.T) {
	pinnedRemote(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = wire.WriteErrorValue(w, wire.NewError(wire.CodeForbidden, "this token's scope may not enqueue writes"))
	}))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "--output", "json", "workspace", "plan", "edge",
	})
	if err == nil {
		t.Fatal("a forbidden write must fail")
	}
	if !strings.Contains(out, `"code":"forbidden"`) {
		t.Fatalf("error envelope = %s", out)
	}
}

func TestRemoteJobsFollow(t *testing.T) {
	stub := &writeAPIStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "jobs", "follow", "9"})
	if err != nil {
		t.Fatalf("remote jobs follow: %v", err)
	}
	if !strings.Contains(out, "[stdout] ok: [node0]") ||
		!strings.Contains(out, "job 9  state: succeeded  duration: 4s") {
		t.Fatalf("output = %q", out)
	}
}

// --- guards: inputs that name something on THIS machine ---------------------

func TestRemoteRunRefusesLocalInputs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "relative playbook without a source",
			args: []string{"playbook", "run", "site.yml"},
			want: "path on this machine",
		},
		{
			name: "inventory path",
			args: []string{"playbook", "run", "site.yml", "--source", "demo", "--inventory", "./hosts.ini"},
			want: "registered inventory",
		},
		{
			name: "extra-var file",
			args: []string{"playbook", "run", "site.yml", "--source", "demo", "-e", "@vars.yml"},
			want: "reads a file on this machine",
		},
		{
			name: "hooked-source wait",
			args: []string{"playbook", "run", "site.yml", "--source", "demo", "--wait"},
			want: "--wait",
		},
		{
			name: "plan by path",
			args: []string{"workspace", "apply", "edge", "--plan", "/var/lib/plan.tfplan"},
			want: "saved plan artifact id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &writeAPIStub{}
			pinnedRemote(t, stub.handler(t))
			_, _, err := runRoot(t, append([]string{"--remote", "box"}, tc.args...))
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			stub.mu.Lock()
			defer stub.mu.Unlock()
			if stub.runBody != nil || stub.applyBody != nil {
				t.Error("a refused command must not reach the daemon")
			}
		})
	}
}

// --no-follow is a remote-only shape: locally the run drains or follows the job
// it enqueued, so accepting the flag would promise something that cannot happen.
func TestNoFollowRefusedLocally(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	_, _, err := runRoot(t, []string{"playbook", "run", "site.yml", "--no-follow"})
	if err == nil || !strings.Contains(err.Error(), "--no-follow applies to a --remote run") {
		t.Fatalf("error = %v", err)
	}
}

// The LOCAL path is unchanged: a --local write command never contacts the
// pinned default remote, it fails against the local (empty) data dir.
func TestLocalWriteStaysLocal(t *testing.T) {
	pinnedRemote(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a --local command must NOT contact the remote (%s %s)", r.Method, r.URL.Path)
	}))
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	_, _, err := runRoot(t, []string{"--local", "workspace", "plan", "edge"})
	if err == nil {
		t.Fatal("a plan against an empty data dir must fail locally")
	}
	if !strings.Contains(err.Error(), "edge") {
		t.Fatalf("expected the local no-workspace error, got %v", err)
	}
}

// `jobs follow` works locally too: it tails the same append-only rows and exits
// with the settled job's code.
func TestLocalJobsFollowTerminalJob(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id := seedTerminalJob(t)

	out, _, err := runRoot(t, []string{"jobs", "follow", fmt.Sprint(id)})
	if err != nil {
		t.Fatalf("local jobs follow: %v", err)
	}
	if !strings.Contains(out, "[info] ok: [node0]") {
		t.Fatalf("output = %q", out)
	}
	if !strings.Contains(out, fmt.Sprintf("job %d  state: succeeded", id)) {
		t.Fatalf("output = %q", out)
	}
}

// seedTerminalJob writes one settled job with an event into the local store.
func seedTerminalJob(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	job, err := st.CreateJob(ctx, store.NewJob{Runbook: "site", Kind: "playbook", Trigger: "cli"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := st.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelInfo, Msg: "ok: [node0]",
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := st.ClaimJob(ctx, job.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("transition: %v", err)
	}
	return job.ID
}
