package main

import (
	"context"
	"encoding/base64"
	"path/filepath"

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
func newAnsibleDriver(dataDir string, env *secrets.Envelope, vaultProject string) (executor.Driver, [][]byte) {
	opts := executor.AnsibleOptions{
		VaultID:         executor.DefaultVaultID,
		CollectionsPath: filepath.Join(dataDir, collectionsCacheDir),
	}
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

	drv, derived := newAnsibleDriver(d.dataDir, d.env, job.Args.VaultProject)
	if len(derived) == 0 {
		return drv.Run(ctx, spec, sink)
	}
	return drv.Run(ctx, spec, runSink{redactor: secrets.NewRedactor(derived), next: sink})
}
