package main

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"time"

	"exocomp.dev/exocomp/internal/deps"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// vaultPurpose is the HKDF purpose the ansible vault password is derived
// under; the project name is the derivation id.
const vaultPurpose = "ansible-vault"

// newAnsibleDriver builds the driver one ansible job runs through, and returns
// the plaintexts that run's redactor must scrub on top of the sealed secrets.
// Whoever runs the job — the CLI draining its own queue or the daemon claiming
// the row — derives the vault password here and nowhere else, so the two
// cannot drift apart. An empty project runs without a vault password.
func newAnsibleDriver(dataDir string, env *secrets.Envelope, vaultProject string,
	runSecrets executor.RunSecrets, inventoryArtifact string, venv deps.Venv,
	extraVars map[string]string, tolerateUnreachable bool,
	captures []executor.AnsibleCapture, sealer executor.OutputSealer) (executor.Driver, [][]byte) {
	opts := executor.AnsibleOptions{
		VaultID:             executor.DefaultVaultID,
		CollectionsPath:     filepath.Join(dataDir, collectionsCacheDir),
		RunSecrets:          runSecrets,
		InventoryArtifact:   inventoryArtifact,
		ExtraVars:           extraVars,
		TolerateUnreachable: tolerateUnreachable,
		// A bootstrap play's declared captures are sealed after a successful run.
		// The sealer is nil for every other play, which drops any capture — the
		// fail-safe direction (internal/executor/ansible.go: captureActive).
		Captures:     captures,
		OutputSealer: sealer,
	}
	// A source that declared Python dependencies runs inside its per-source uv
	// venv (ADR 0011): the venv's ansible-playbook, interpreter, and PATH. A
	// zero venv leaves the driver on the image toolchain.
	applyVenv(&opts, venv)
	if env == nil || vaultProject == "" {
		return ansibleTierDriver(dataDir, opts), nil
	}

	derived := env.Derive(vaultPurpose, vaultProject)
	password := []byte(base64.StdEncoding.EncodeToString(derived))
	opts.VaultPassword = password
	return ansibleTierDriver(dataDir, opts), [][]byte{derived, password}
}

// ansibleJobDriver runs a queued ansible job the daemon claimed. The password
// is deliberately absent from the job row, so the driver is rebuilt per job
// from the vault project the row names.
type ansibleJobDriver struct {
	store   *store.Store
	env     *secrets.Envelope
	dataDir string
}

// Run implements executor.Driver.
func (d ansibleJobDriver) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	job, err := d.store.GetJob(ctx, spec.JobID)
	if err != nil {
		return executor.Result{}, err
	}

	runSecrets, secretPlain, err := resolveRunSecrets(ctx, d.store, d.env,
		job.Args.RunSecretBinding, job.Args.Inventory)
	if err != nil {
		return executor.Result{}, err
	}

	// A play whose argv names no inventory but whose job names a registered
	// static inventory (story 5: the tofu outputs a prior apply captured) reads
	// it as a copy in the run's own private dir — no bearer, no passphrase, no
	// tfstate near it. A play that already names an inventory keeps it as-is.
	inventoryArtifact := ""
	if !hasInventoryFlag(spec.Argv) {
		inventoryArtifact, err = staticInventoryPath(ctx, d.store, job.Args.Inventory)
		if err != nil {
			return executor.Result{}, err
		}
	}

	// A provision chain's onboarding step pins each derived host's key before it
	// pushes the fleet key to it. It happens here because the driver is the
	// first place the hosts are known — they come from the inventory the apply
	// materialized, not from the command that defined the workflow. Any other
	// job is a no-op.
	if err := recordProvisionedHostKeys(ctx, d.store, job, inventoryArtifact, sink); err != nil {
		return executor.Result{}, err
	}

	// Resolve the per-source venv before the run (ADR 0011 vendoring step). A
	// failure fails the job: a run must not proceed against half-met deps.
	venv, err := resolveProjectVenv(ctx, d.dataDir, ansiblePlaybookPath(spec.Argv))
	if err != nil {
		return executor.Result{}, err
	}
	if venv.Created {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelSystem,
			Msg: "resolved project deps into " + venv.Dir})
	}

	// A bootstrap play carries its capture declarations and workflow id as reserved
	// extra-vars the composer smuggled onto the job; extract and strip them so the
	// play sees only its own vars, and build the sealer that binds each capture
	// under ab:<workflow>. A play with no captures, or a driver with no envelope,
	// gets a nil sealer and seals nothing.
	captures, workflow, extraVars := extractBootstrapCaptures(job.Args.ExtraVars)
	var sealer executor.OutputSealer
	if len(captures) > 0 && d.env != nil {
		sealer = executor.AnsibleOutputSealer{Env: d.env, Store: d.store, Workflow: workflow}
	}

	drv, derived := newAnsibleDriver(d.dataDir, d.env, job.Args.VaultProject, runSecrets, inventoryArtifact, venv, extraVars, job.Args.TolerateUnreachable, captures, sealer)
	material := append(derived, secretPlain...)
	if len(material) == 0 {
		return drv.Run(ctx, spec, sink)
	}
	return drv.Run(ctx, spec, runSink{redactor: secrets.NewRedactor(material), next: sink})
}
