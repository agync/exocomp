package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunDirTmpfs reports whether dir is backed by a tmpfs mount — a RAM-only
// filesystem whose contents never reach durable disk. The file-staging path
// asserts it before writing a sealed bearer to a file, and the doctor reads it
// to show whether the per-job run directory holds staged secrets in RAM
// (docs/design/bootstrap-first.md, Phase 3). An error (statfs failed, or a
// non-Linux host with no probe) is reported as "not tmpfs" by the callers,
// which fail closed on it.
func RunDirTmpfs(dir string) (bool, error) {
	t, err := runDirFSType(dir)
	if err != nil {
		return false, err
	}
	return t == tmpfsMagic, nil
}

// StageRunSecretFiles materializes the run-secret values named in fileEnv into
// 0600 files under dir and rewrites each named env var from its secret value to
// the file's path, so a tool that authenticates from a KUBECONFIG file
// (kubectl/helm, an exec-auth provider) reads the credential from a path while
// the value itself only ever exists in dir.
//
// dir MUST be the run's own private tree — the one removed with the job, so the
// file is torn down with it — AND tmpfs: the staged value is a plaintext bearer,
// and writing it onto durable disk is the exposure this path exists to avoid.
// When dir is not tmpfs the function stages NOTHING and returns an error. That
// is ADR 0005's never-silent-downgrade discipline applied to disk: a caller that
// asked for a file gets a clear refusal, never a silent durable-disk artifact.
//
// It returns a rewritten copy; the input map is left untouched, so a caller can
// fall back on it. A named env not present in rs is skipped — an empty capture
// binds nothing, the same empty-tolerance the ab:<workflow> resolution takes.
// Only the path travels onward as env; the value reaches no argv, event, or log.
func StageRunSecretFiles(rs RunSecrets, fileEnv []string, dir string) (RunSecrets, error) {
	if len(fileEnv) == 0 {
		return rs, nil
	}
	tmpfs, err := RunDirTmpfs(dir)
	if err != nil {
		return nil, fmt.Errorf("executor: run-secret file staging: %w", err)
	}
	if !tmpfs {
		return nil, fmt.Errorf("executor: run-secret file staging refused: %s is not tmpfs; "+
			"a staged secret would land on durable disk (Phase 3 needs the run dir on a noswap tmpfs)", dir)
	}

	out := make(RunSecrets, len(rs))
	for k, v := range rs {
		out[k] = v
	}
	for _, name := range fileEnv {
		v, ok := rs[name]
		if !ok {
			continue
		}
		path := filepath.Join(dir, runSecretFileName(name))
		if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
			return nil, fmt.Errorf("executor: stage run-secret %s: %w", name, err)
		}
		out[name] = path
	}
	return out, nil
}

// runSecretFileName is the deterministic basename a staged run-secret value is
// written under: the env name lower-cased. An env name is a POSIX identifier
// (isEnvName), so lower-casing yields a safe, collision-free basename inside the
// run's own private directory.
func runSecretFileName(name string) string {
	return strings.ToLower(name)
}
