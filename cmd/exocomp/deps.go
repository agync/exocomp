package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/deps"
	"exocomp.dev/exocomp/internal/executor"
)

// pythonCacheDir is the venv cache root under the data dir. It sits beside
// cache/collections so both dependency planes live under one backed-up,
// offline-restorable tree (ADR 0011).
const pythonCacheDir = "cache/python"

// resolveProjectVenv resolves the per-source uv venv a playbook run executes
// inside (ADR 0011). It discovers the project's Python manifest from the
// playbook's directory; a source that declares none returns a zero Venv, so the
// run stays on the shared image toolchain — which is also what keeps DR-path
// runbooks (seeded standalone, carrying no manifest) on the image, never a
// project venv. A declared manifest is resolved into cache/python/<source>/<hash>,
// hash-gated so an unchanged manifest invokes no uv.
//
// It is fail-closed: a manifest that will not resolve returns an error, so a run
// never proceeds against half-met dependencies.
func resolveProjectVenv(ctx context.Context, dataDir, playbook string) (deps.Venv, error) {
	if playbook == "" {
		return deps.Venv{}, nil
	}
	start := filepath.Dir(playbook)
	manifest, err := deps.DiscoverPython(start)
	if err != nil {
		return deps.Venv{}, fmt.Errorf("deps: discover manifest for %s: %w", playbook, err)
	}
	if !manifest.HasDeps() {
		return deps.Venv{}, nil
	}
	return deps.EnsureVenv(ctx, deps.VenvSpec{
		CacheRoot: filepath.Join(dataDir, pythonCacheDir),
		Source:    deps.SourceKey(manifest.Root),
		Manifest:  manifest,
	})
}

// ansiblePlaybookPath returns the playbook file an ansible argv names: its last
// positional argument. ansible-playbook takes playbooks as its only positionals
// and reads options before them, so the tail is the playbook whether or not the
// argv also carries an --inventory.
func ansiblePlaybookPath(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	last := argv[len(argv)-1]
	if strings.HasPrefix(last, "-") {
		return ""
	}
	return last
}

// applyVenv points ansible options at a resolved venv: the run executes the
// venv's own ansible-playbook, its interpreter drives the modules, and its bin
// leads PATH. A zero Venv (no declared deps) leaves the options on the image
// toolchain.
func applyVenv(opts *executor.AnsibleOptions, venv deps.Venv) {
	if venv.Dir == "" {
		return
	}
	opts.Binary = venv.Ansible
	opts.Interpreter = venv.Python
	opts.VenvBin = venv.Bin
}
