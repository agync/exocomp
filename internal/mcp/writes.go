package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// Every write tool routes through the SAME internal/service verb the CLI and the
// HTTP write API call (invariant 5): the verb assembles the queue.Spec and makes
// the gate decision, and this face supplies a pure-enqueue Enqueuer. The local
// view kinds below carry identifiers and states only, never values — the wire
// package names no write projection, so, like internal/web, they live beside the
// face that emits them.
const (
	kindJobEnqueued     = "job.enqueued"
	kindApprovalPending = "approval.pending"
	// kindCheckDeleted and kindNetgroupDeleted are the values-free
	// acknowledgments a remove returns: the name that was removed, nothing else.
	kindCheckDeleted    = "check.deleted"
	kindNetgroupDeleted = "netgroup.deleted"
)

type jobEnqueuedView struct {
	JobID int64  `json:"job_id"`
	State string `json:"state,omitempty"`
}

// namedView is the body of a remove acknowledgment.
type namedView struct {
	Name string `json:"name"`
}

// approvalPendingView is a gated apply: the job sits in awaiting_approval and
// this approval releases it. ttl_seconds and expires_at let an agent read the
// fail-closed rejection at TTL as intended, not as an error (agent-interface §4).
type approvalPendingView struct {
	ApprovalID     int64  `json:"approval_id"`
	JobID          int64  `json:"job_id"`
	State          string `json:"state"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	TTLSeconds     int64  `json:"ttl_seconds,omitempty"`
	Policy         string `json:"policy,omitempty"`
	PlanArtifactID int64  `json:"plan_artifact_id,omitempty"`
}

// (note: plain saved-plan applies are now enqueued against the sealed plan
// path, matching the HTTP write API; see toolWorkspaceApply's buildArgv.)

// planWriteArgs is the shared argument shape for plan and apply. All optional; a
// bare {name} is the common case.
type planWriteArgs struct {
	Name           string `json:"name"`
	RunSecrets     string `json:"run_secrets"`
	Runner         string `json:"runner"`
	Timeout        string `json:"timeout"`
	IdempotencyKey string `json:"idempotency_key"`
	// Plan names a saved plan artifact by id for apply; empty is a fresh apply.
	Plan string `json:"plan"`
}

// Tool labels for the idempotency namespace, and the namespacing itself. A
// caller-chosen key is scoped to this face, the authenticated principal, and the
// tool that used it, so an agent's guessable key ("deploy-<date>", a ticket id)
// cannot collide with another caller's write or address the queue's reserved
// `wf:` workflow step keys.
const (
	toolPlanKey  = "workspace.plan"
	toolApplyKey = "workspace.apply"
)

func idempotencyKey(c Caller, tool, key string) string {
	if key == "" {
		return ""
	}
	return "mcp:" + c.KeyNamespace + ":" + tool + ":" + key
}

func parseTimeout(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	return time.ParseDuration(v)
}

// tofuArtifactsDir is where a plan's -out file lands, mirroring the CLI and the
// HTTP API. It sits inside the 0700 data dir: plan files hold resource values in
// the clear until they are sealed.
func (s *Server) tofuArtifactsDir() (string, error) {
	dir := filepath.Join(s.dataDir, "artifacts", "tofu")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// toolWorkspacePlan enqueues a tofu plan through PlanWorkspace and returns the
// job id. Sealing the plan into a consumable artifact is a CLI-resident post-run
// step (recordPlan/evalPlanPolicy), not done here — the same boundary the HTTP
// write API draws.
func (s *Server) toolWorkspacePlan(ctx context.Context, c Caller, args planWriteArgs) (string, any, error) {
	if args.Name == "" {
		return "", nil, invalidArg("workspace name is required")
	}
	timeout, err := parseTimeout(args.Timeout)
	if err != nil {
		return "", nil, invalidArg("timeout is not a valid Go duration")
	}
	ws, err := s.store.WorkspaceByName(ctx, args.Name)
	if err != nil {
		return "", nil, err
	}
	artifacts, err := s.tofuArtifactsDir()
	if err != nil {
		return "", nil, err
	}
	res, err := s.service().PlanWorkspace(ctx, s.newEnqueuer(), service.PlanInput{
		Workspace:      ws.Name,
		TofuDir:        ws.TofuDir,
		ArtifactsDir:   artifacts,
		RunSecrets:     args.RunSecrets,
		RunnerName:     args.Runner,
		IdempotencyKey: idempotencyKey(c, toolPlanKey, args.IdempotencyKey),
		Timeout:        timeout,
		// TriggerAPI tells the daemon to seal this plan's -out file into a
		// consumable artifact post-run, matching the HTTP write API.
		Trigger: service.TriggerAPI,
		Actor:   c.Actor,
	})
	if err != nil {
		return "", nil, err
	}
	return kindJobEnqueued, jobEnqueuedView{JobID: res.Job.ID, State: string(res.Job.State)}, nil
}

// toolWorkspaceApply enqueues a tofu apply through ApplyWorkspace. A fresh apply
// (no plan) auto-approves and enqueues plainly. A saved plan named by artifact
// id is routed by its recorded policy verdict: deny is refused, require_approval
// lands the job in awaiting_approval behind a fresh approval (the pending view),
// and a plain saved-plan apply is refused for now (decrypt is CLI-resident).
func (s *Server) toolWorkspaceApply(ctx context.Context, c Caller, args planWriteArgs) (string, any, error) {
	if args.Name == "" {
		return "", nil, invalidArg("workspace name is required")
	}
	timeout, err := parseTimeout(args.Timeout)
	if err != nil {
		return "", nil, invalidArg("timeout is not a valid Go duration")
	}
	ws, err := s.store.WorkspaceByName(ctx, args.Name)
	if err != nil {
		return "", nil, err
	}

	var saved *store.Artifact
	var gate service.PlanGate
	if args.Plan != "" {
		id, perr := strconv.ParseInt(args.Plan, 10, 64)
		if perr != nil || id <= 0 {
			return "", nil, invalidArg("plan must be a saved plan artifact id")
		}
		art, gerr := s.store.GetArtifact(ctx, id)
		if gerr != nil {
			return "", nil, gerr
		}
		if art.Kind != reactive.PlanArtifactKind {
			return "", nil, invalidArg("artifact " + args.Plan + " is not a plan")
		}
		saved = &art
		// Fail-closed adjudication: a plan on a policied workspace with no
		// recorded verdict is unconsumable, not unpoliced.
		gate, err = s.service().PlanGateForPlan(ctx, ws.Name, art)
		var mismatch *service.PlanWorkspaceMismatchError
		if errors.As(err, &mismatch) {
			// A plan named for another workspace is a bad argument.
			return "", nil, invalidArg(err.Error())
		}
		if err != nil {
			return "", nil, err
		}
	}

	deriver, closeEnv, err := s.approvalDeriver(saved, gate)
	if err != nil {
		return "", nil, err
	}
	if closeEnv != nil {
		defer closeEnv()
	}

	res, err := s.service().ApplyWorkspace(ctx, s.newEnqueuer(), deriver, service.ApplyInput{
		Workspace:       ws.Name,
		WorkspaceSerial: ws.Serial,
		TofuDir:         ws.TofuDir,
		RunSecrets:      args.RunSecrets,
		RunnerName:      args.Runner,
		IdempotencyKey:  idempotencyKey(c, toolApplyKey, args.IdempotencyKey),
		Timeout:         timeout,
		SavedPlan:       saved,
		Gate:            gate,
		Actor:           c.Actor,
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		if saved == nil {
			return executor.TofuArgv("apply", "-auto-approve"), nil, func() {}, nil
		}
		// Enqueue the apply against the SEALED plan path; the daemon that
		// claims the row decrypts it into a run-private plaintext for the
		// one run that consumes it, matching the HTTP write API's behaviour.
		return executor.TofuArgv("apply", saved.Path), saved, func() {}, nil
	})
	if err != nil {
		return "", nil, err
	}

	if res.Gated {
		a := res.Approval
		return kindApprovalPending, approvalPendingView{
			ApprovalID:     a.ApprovalID,
			JobID:          a.JobID,
			State:          string(a.State),
			ExpiresAt:      a.ExpiresAt.UTC().Format(time.RFC3339),
			TTLSeconds:     int64(a.ExpiresAt.Sub(s.now()).Seconds()),
			Policy:         a.Policy,
			PlanArtifactID: a.PlanArtifactID,
		}, nil
	}

	run := res.Run
	defer run.Release()
	return kindJobEnqueued, jobEnqueuedView{JobID: run.Job.ID, State: string(run.Job.State)}, nil
}

// approvalDeriver opens the secrets envelope only when the apply will actually
// route through the gate (a require-approval saved plan), because that is the
// only path that mints an approval capability. A fresh apply and a deny/plain
// plan need no deriver, so the envelope — and the keyring it needs — is never
// touched for them.
func (s *Server) approvalDeriver(saved *store.Artifact, gate service.PlanGate) (reactive.Deriver, func(), error) {
	if saved == nil || !gate.RequireApproval {
		return nil, nil, nil
	}
	return s.openEnvelope()
}

// There is deliberately no decide seam here. The decide seam is supplied by
// the transport and is non-nil only for an approve-scope token (TokenCaller,
// ADR 0014 §3) — there is no ambient path, so an agent holding workspace_apply
// does not also hold the release for the gate that apply raises. An approval
// is decided from the browser/phone surface, the CLI, or the scope-checked
// /api/v1 decision route.

// --- checks (write) ---------------------------------------------------------
//
// Each tool calls the SAME store verb the CLI and the HTTP write API call
// (CreateCheck/UpdateCheck/SetCheckEnabled/DeleteCheck/SetCheckSnooze). A check
// is configuration, not a secret and not a queued job, so there is no
// idempotency key to namespace: a repeat add is a duplicate-name refusal, not a
// second row.

// checkWriteArgs is the shared add/update argument shape. The optional kind
// fields are POINTERS so an omitted field is distinguishable from a zero value:
// the CLI refuses a kind-inapplicable flag through cobra's Changed, and this is
// the JSON argument's equivalent. A non-pointer int would make expect:0
// indistinguishable from an omitted expect.
type checkWriteArgs struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Interval   string   `json:"interval"`
	Targets    []string `json:"targets"`
	Expect     *int     `json:"expect"`
	MinDays    *int     `json:"min_days"`
	ExpectAddr *string  `json:"expect_addr"`
	Op         *string  `json:"op"`
	Value      *float64 `json:"value"`
}

// storeWriteErr maps a store refusal the wire taxonomy cannot name on its own —
// operator-input validation — onto invalid_argument, stripping the doubled
// internal namespace the store wraps it in (mirroring the CLI's
// validationError). A duplicate or not_found keeps its sentinel so
// wire.ErrorFor names it.
func storeWriteErr(err error) error {
	if errors.Is(err, store.ErrValidation) {
		msg := strings.TrimPrefix(err.Error(), "store: validation: store: ")
		msg = strings.TrimPrefix(msg, "store: validation: ")
		return invalidArg(msg)
	}
	return err
}

// checkTargetsFromArgs parses the name=spec targets and applies the kind's
// optional fields, refusing a field that does not belong to the kind and a
// metric missing its operator or value — the same contract the CLI's
// applyKindFlags enforces.
func checkTargetsFromArgs(kind string, specs []string, a *checkWriteArgs) ([]store.CheckTarget, error) {
	parsed, err := store.ParseCheckTargets(kind, specs, false)
	if err != nil {
		return nil, invalidArg(err.Error())
	}
	if len(parsed) == 0 {
		return nil, invalidArg("check needs at least one target")
	}
	switch {
	case a.Expect != nil && kind != store.CheckHTTP:
		return nil, invalidArg("expect applies to kind http")
	case a.MinDays != nil && kind != store.CheckTLS:
		return nil, invalidArg("min_days applies to kind tls")
	case a.ExpectAddr != nil && kind != store.CheckDNS:
		return nil, invalidArg("expect_addr applies to kind dns")
	case a.Op != nil && kind != store.CheckMetric:
		return nil, invalidArg("op applies to kind metric")
	case a.Value != nil && kind != store.CheckMetric:
		return nil, invalidArg("value applies to kind metric")
	case kind == store.CheckMetric && a.Op == nil:
		return nil, invalidArg("op is required for kind metric")
	case kind == store.CheckMetric && a.Value == nil:
		return nil, invalidArg("value is required for kind metric")
	}
	var (
		expect     int
		minDays    int
		expectAddr string
		op         string
		value      float64
	)
	if a.Expect != nil {
		expect = *a.Expect
	}
	if a.MinDays != nil {
		minDays = *a.MinDays
	}
	if a.ExpectAddr != nil {
		expectAddr = *a.ExpectAddr
	}
	if a.Op != nil {
		op = *a.Op
	}
	if a.Value != nil {
		value = *a.Value
	}
	for i := range parsed {
		parsed[i].Expect = expect
		parsed[i].MinDays = minDays
		parsed[i].ExpectAddr = expectAddr
		parsed[i].Op = op
		parsed[i].Value = value
	}
	return parsed, nil
}

func checkIntervalSeconds(v string) (int64, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, invalidArg("interval must be a positive Go duration (e.g. 5m)")
	}
	return int64(d.Seconds()), nil
}

func (s *Server) runCheckAdd(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a checkWriteArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.Name == "" {
		return "", nil, invalidArg("name is required")
	}
	if a.Kind == "" {
		return "", nil, invalidArg("kind is required")
	}
	intervalS, err := checkIntervalSeconds(a.Interval)
	if err != nil {
		return "", nil, err
	}
	targets, err := checkTargetsFromArgs(a.Kind, a.Targets, &a)
	if err != nil {
		return "", nil, err
	}
	created, err := s.store.CreateCheck(ctx, store.NewCheck{
		Name: a.Name, Kind: a.Kind, IntervalS: intervalS, Targets: targets,
	})
	if err != nil {
		return "", nil, storeWriteErr(err)
	}
	return s.readCheck(ctx, created.Name)
}

func (s *Server) runCheckUpdate(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a checkWriteArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.Name == "" {
		return "", nil, invalidArg("name is required")
	}
	cur, err := s.store.GetCheck(ctx, a.Name)
	if err != nil {
		return "", nil, err
	}
	if a.Kind != "" && a.Kind != cur.Kind {
		return "", nil, invalidArg("kind is not updatable; the stored kind is kept")
	}
	intervalS := cur.IntervalS
	if a.Interval != "" {
		intervalS, err = checkIntervalSeconds(a.Interval)
		if err != nil {
			return "", nil, err
		}
	}
	targets, err := checkTargetsFromArgs(cur.Kind, a.Targets, &a)
	if err != nil {
		return "", nil, err
	}
	if _, err := s.store.UpdateCheck(ctx, a.Name, targets, intervalS); err != nil {
		return "", nil, storeWriteErr(err)
	}
	return s.readCheck(ctx, a.Name)
}

// checkNameArg decodes the bare {name} shape the enable/disable/rm/unsnooze
// tools share.
func decodeCheckName(args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", err
	}
	if a.Name == "" {
		return "", invalidArg("name is required")
	}
	return a.Name, nil
}

func (s *Server) runCheckEnable(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	name, err := decodeCheckName(args)
	if err != nil {
		return "", nil, err
	}
	if err := s.store.SetCheckEnabled(ctx, name, true); err != nil {
		return "", nil, storeWriteErr(err)
	}
	return s.readCheck(ctx, name)
}

func (s *Server) runCheckDisable(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	name, err := decodeCheckName(args)
	if err != nil {
		return "", nil, err
	}
	if err := s.store.SetCheckEnabled(ctx, name, false); err != nil {
		return "", nil, storeWriteErr(err)
	}
	return s.readCheck(ctx, name)
}

func (s *Server) runCheckRm(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	name, err := decodeCheckName(args)
	if err != nil {
		return "", nil, err
	}
	if err := s.store.DeleteCheck(ctx, name); err != nil {
		return "", nil, storeWriteErr(err)
	}
	return kindCheckDeleted, namedView{Name: name}, nil
}

// runCheckSnooze suppresses a check's transitions until a deadline. Like the
// CLI, for is mandatory and positive: a snooze the operator forgets is how a
// check stops noticing it is broken. Probing and state advance continue.
func (s *Server) runCheckSnooze(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		Name string `json:"name"`
		For  string `json:"for"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.Name == "" {
		return "", nil, invalidArg("name is required")
	}
	d, err := time.ParseDuration(a.For)
	if err != nil || d <= 0 {
		return "", nil, invalidArg("for must be a positive Go duration (e.g. 4h)")
	}
	if err := s.store.SetCheckSnooze(ctx, a.Name, s.now().Add(d)); err != nil {
		return "", nil, storeWriteErr(err)
	}
	return s.readCheck(ctx, a.Name)
}

func (s *Server) runCheckUnsnooze(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	name, err := decodeCheckName(args)
	if err != nil {
		return "", nil, err
	}
	if err := s.store.SetCheckSnooze(ctx, name, time.Time{}); err != nil {
		return "", nil, storeWriteErr(err)
	}
	return s.readCheck(ctx, name)
}

// --- netgroups (write) ------------------------------------------------------

// netgroupWriteArgs is the shared add/update shape. prefixes is the CLI's
// comma-separated CIDR list (a `!`-prefixed member is an exclusion), parsed by
// the one store.ParsePrefixList the CLI and the web form also use.
type netgroupWriteArgs struct {
	Name     string `json:"name"`
	Prefixes string `json:"prefixes"`
}

func netgroupPrefixes(spec string) (store.PrefixList, error) {
	list, err := store.ParsePrefixList(spec)
	if err != nil {
		return store.PrefixList{}, invalidArg(err.Error())
	}
	if len(list.Includes) == 0 {
		return store.PrefixList{}, invalidArg("need at least one include CIDR")
	}
	return list, nil
}

func (s *Server) runNetgroupAdd(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a netgroupWriteArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.Name == "" {
		return "", nil, invalidArg("name is required")
	}
	list, err := netgroupPrefixes(a.Prefixes)
	if err != nil {
		return "", nil, err
	}
	g, err := s.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: a.Name, Prefixes: list.Includes, Excludes: list.Excludes,
	})
	if err != nil {
		return "", nil, storeWriteErr(err)
	}
	return wire.KindNetgroup, wire.NewNetgroupList([]store.Netgroup{g})[0], nil
}

func (s *Server) runNetgroupUpdate(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a netgroupWriteArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.Name == "" {
		return "", nil, invalidArg("name is required")
	}
	list, err := netgroupPrefixes(a.Prefixes)
	if err != nil {
		return "", nil, err
	}
	g, err := s.store.UpdateNetgroup(ctx, a.Name, list.Includes, list.Excludes)
	if err != nil {
		return "", nil, storeWriteErr(err)
	}
	return wire.KindNetgroup, wire.NewNetgroupList([]store.Netgroup{g})[0], nil
}

func (s *Server) runNetgroupRm(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	name, err := decodeCheckName(args)
	if err != nil {
		return "", nil, err
	}
	if err := s.store.DeleteNetgroup(ctx, name); err != nil {
		return "", nil, storeWriteErr(err)
	}
	return kindNetgroupDeleted, namedView{Name: name}, nil
}
