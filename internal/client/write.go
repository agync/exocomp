package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// This file is the typed WRITE surface: one method per /api/v1 POST route
// (internal/web/api_write.go). Every write ENQUEUES — it never runs anything in
// this process — so a method returns identifiers and states, never values.
//
// The response shapes below are field-for-field the API's own write views, so a
// decoded value re-emitted through wire.WriteSuccess reproduces the daemon's
// bytes exactly; that is what keeps a remote `--output json` write identical to
// the local one. wire itself carries no write projections yet, which is why
// the shapes are declared here rather than imported.

// Write-response kinds, mirroring internal/web/api_write.go.
const (
	KindJobEnqueued     = "job.enqueued"
	KindApprovalPending = "approval.pending"
	KindDecision        = "approval.decision"
)

// JobEnqueued is a clean enqueue: the job id to follow and its state at enqueue
// time.
type JobEnqueued struct {
	JobID int64  `json:"job_id"`
	State string `json:"state,omitempty"`
}

// ApprovalPending is a gated apply: the job sits in awaiting_approval and this
// approval releases it. ExpiresAt/TTLSeconds are carried so a caller reads the
// fail-closed rejection at TTL as intended, not as an error.
type ApprovalPending struct {
	ApprovalID     int64  `json:"approval_id"`
	JobID          int64  `json:"job_id"`
	State          string `json:"state"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	TTLSeconds     int64  `json:"ttl_seconds,omitempty"`
	Policy         string `json:"policy,omitempty"`
	PlanArtifactID int64  `json:"plan_artifact_id,omitempty"`
}

// Decision is a settled approval: which approval, the outcome, and the terminal
// notice. An outcome that is neither approved nor rejected (already settled,
// expired) still arrives here with a 200 — the decision was refused, not the
// request — so the caller decides how to report it.
type Decision struct {
	ApprovalID int64  `json:"approval_id"`
	Outcome    string `json:"outcome,omitempty"`
	Message    string `json:"message,omitempty"`
}

// ApplyResult is the two-shaped answer of an apply: exactly one of Enqueued and
// Pending is set. A gated apply enqueued nothing runnable — the job waits behind
// Pending — so the caller must not treat it as a started run.
type ApplyResult struct {
	Enqueued *JobEnqueued
	Pending  *ApprovalPending
}

// WorkspaceRequest is the optional body of a plan or an apply. Plan names a
// SAVED plan artifact by id (apply only); the API refuses anything else, since
// applying a path this machine can see says nothing about the daemon's disk.
type WorkspaceRequest struct {
	RunSecrets     string
	Timeout        time.Duration
	Plan           string
	IdempotencyKey string
	// Runner names a registered runner whose memory_max/cpu_quota the daemon
	// should enforce as a per-job cgroup cap, mirroring the local --runner
	// flag. Empty is the default, uncapped path.
	Runner string
}

// body renders the request as the JSON the write API accepts. Timeout is a Go
// duration string, the same text the CLI's --timeout parses.
func (r WorkspaceRequest) body() map[string]string {
	b := map[string]string{}
	if r.RunSecrets != "" {
		b["run_secrets"] = r.RunSecrets
	}
	if r.Timeout > 0 {
		b["timeout"] = r.Timeout.String()
	}
	if r.Plan != "" {
		b["plan"] = r.Plan
	}
	if r.Runner != "" {
		b["runner"] = r.Runner
	}
	return b
}

// PlanWorkspace enqueues a tofu plan for one workspace and returns the job id.
// The daemon runs it; FollowJob watches it.
func (c *Client) PlanWorkspace(ctx context.Context, name string, req WorkspaceRequest) (JobEnqueued, error) {
	kind, data, err := c.do(ctx, http.MethodPost,
		"/api/v1/workspaces/"+pathEscape(name)+"/plan", nil, req.body(), req.IdempotencyKey)
	if err != nil {
		return JobEnqueued{}, err
	}
	return decodeEnqueued(kind, data)
}

// ApplyWorkspace enqueues a tofu apply. With Plan set to a saved plan artifact
// id the daemon consults that plan's recorded policy verdict: a deny is refused
// (a conflict APIError), a require_approval comes back as Pending, and anything
// else enqueues.
func (c *Client) ApplyWorkspace(ctx context.Context, name string, req WorkspaceRequest) (ApplyResult, error) {
	kind, data, err := c.do(ctx, http.MethodPost,
		"/api/v1/workspaces/"+pathEscape(name)+"/apply", nil, req.body(), req.IdempotencyKey)
	if err != nil {
		return ApplyResult{}, err
	}
	switch kind {
	case KindApprovalPending:
		var pending ApprovalPending
		if err := json.Unmarshal(data, &pending); err != nil {
			return ApplyResult{}, fmt.Errorf("client: decode %s payload: %w", kind, err)
		}
		return ApplyResult{Pending: &pending}, nil
	case KindJobEnqueued:
		enq, err := decodeEnqueued(kind, data)
		if err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Enqueued: &enq}, nil
	default:
		return ApplyResult{}, fmt.Errorf("client: apply answered with unexpected kind %q", kind)
	}
}

// DestroyWorkspace enqueues a tofu destroy of one workspace's whole state and
// returns the job id. There is no gated form — a destroy has no plan for a
// policy to read, so the daemon answers the plain enqueue envelope.
func (c *Client) DestroyWorkspace(ctx context.Context, name string, req WorkspaceRequest) (JobEnqueued, error) {
	kind, data, err := c.do(ctx, http.MethodPost,
		"/api/v1/workspaces/"+pathEscape(name)+"/destroy", nil, req.body(), req.IdempotencyKey)
	if err != nil {
		return JobEnqueued{}, err
	}
	return decodeEnqueued(kind, data)
}

// PlaybookRequest is the body of a playbook run. Playbook and Inventory are
// resolved on the DAEMON — Source rebases the playbook onto that source's
// checkout, Inventory names a registered inventory — so no path here means a
// path on the calling machine.
type PlaybookRequest struct {
	Playbook            string   `json:"playbook"`
	Inventory           string   `json:"inventory,omitempty"`
	VaultProject        string   `json:"vault_project,omitempty"`
	RunSecrets          string   `json:"run_secrets,omitempty"`
	Source              string   `json:"source,omitempty"`
	Limit               string   `json:"limit,omitempty"`
	Tags                string   `json:"tags,omitempty"`
	SkipTags            string   `json:"skip_tags,omitempty"`
	Check               bool     `json:"check,omitempty"`
	Diff                bool     `json:"diff,omitempty"`
	ExtraVars           []string `json:"extra_vars,omitempty"`
	TolerateUnreachable bool     `json:"tolerate_unreachable,omitempty"`
	Timeout             string   `json:"timeout,omitempty"`
	// Runner names a registered runner whose memory_max/cpu_quota the daemon
	// should enforce as a per-job cgroup cap, mirroring the local --runner
	// flag. Empty is the default, uncapped path.
	Runner string `json:"runner,omitempty"`
	// EgressPorts are the outbound TCP ports the daemon should grant the run on
	// top of the ansible kind's SSH port 22, for an inventory host on a
	// nonstandard port. Empty is the default posture: no widening.
	EgressPorts []int `json:"egress_ports,omitempty"`
	// SSHIdentityFiles are ssh identity key files on the daemon the run may
	// read. The daemon refuses any its operator did not allow.
	SSHIdentityFiles []string `json:"ssh_identity_files,omitempty"`
	// IdempotencyKey travels as a header, not a body field.
	IdempotencyKey string `json:"-"`
}

// RunPlaybook enqueues one ansible playbook and returns the job id.
func (c *Client) RunPlaybook(ctx context.Context, req PlaybookRequest) (JobEnqueued, error) {
	kind, data, err := c.do(ctx, http.MethodPost, "/api/v1/playbooks/run", nil, req, req.IdempotencyKey)
	if err != nil {
		return JobEnqueued{}, err
	}
	return decodeEnqueued(kind, data)
}

// DecideApproval settles one approval. decision is "approve" or "reject"; the
// capability token is derived server-side, so nothing bearer-like travels here.
func (c *Client) DecideApproval(ctx context.Context, id int64, decision string) (Decision, error) {
	kind, data, err := c.do(ctx, http.MethodPost,
		"/api/v1/approvals/"+strconv.FormatInt(id, 10)+"/decision", nil,
		map[string]string{"decision": decision}, "")
	if err != nil {
		return Decision{}, err
	}
	if kind != KindDecision {
		return Decision{}, fmt.Errorf("client: decision answered with unexpected kind %q", kind)
	}
	var out Decision
	if err := json.Unmarshal(data, &out); err != nil {
		return Decision{}, fmt.Errorf("client: decode %s payload: %w", kind, err)
	}
	return out, nil
}

func decodeEnqueued(kind string, data json.RawMessage) (JobEnqueued, error) {
	if kind != KindJobEnqueued {
		return JobEnqueued{}, fmt.Errorf("client: write answered with unexpected kind %q", kind)
	}
	var out JobEnqueued
	if err := json.Unmarshal(data, &out); err != nil {
		return JobEnqueued{}, fmt.Errorf("client: decode %s payload: %w", kind, err)
	}
	return out, nil
}
