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
	runSecrets executor.RunSecrets, inventoryArtifact string, venv deps.Venv) (executor.Driver, [][]byte) {
	opts := executor.AnsibleOptions{
		VaultID:           executor.DefaultVaultID,
		CollectionsPath:   filepath.Join(dataDir, collectionsCacheDir),
		RunSecrets:        runSecrets,
		InventoryArtifact: inventoryArtifact,
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

	drv, derived := newAnsibleDriver(d.dataDir, d.env, job.Args.VaultProject, runSecrets, inventoryArtifact, venv)
	material := append(derived, secretPlain...)
	if len(material) == 0 {
		return drv.Run(ctx, spec, sink)
	}
	return drv.Run(ctx, spec, runSink{redactor: secrets.NewRedactor(material), next: sink})
}
