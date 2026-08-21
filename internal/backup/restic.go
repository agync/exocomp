package backup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
)

// defaultRetention is the policy the design names: a week of dailies and a
// month of weeklies, pruned in the same command.
var defaultRetention = []string{"--keep-daily", "7", "--keep-weekly", "4"}

// retentionKeys maps a target's retention config onto restic forget flags.
var retentionKeys = map[string]string{
	"last":    "--keep-last",
	"hourly":  "--keep-hourly",
	"daily":   "--keep-daily",
	"weekly":  "--keep-weekly",
	"monthly": "--keep-monthly",
	"yearly":  "--keep-yearly",
}

// resticSummary is the one line of `restic backup --json` that reports the
// outcome. It enriches the backup_run row; the exit code decides success.
type resticSummary struct {
	MessageType string `json:"message_type"`
	SnapshotID  string `json:"snapshot_id"`
	DataAdded   int64  `json:"data_added"`
}

func (e *Engine) resticPassword(name string) (string, error) {
	key := e.env.Derive("restic-repo", name)
	if len(key) != dekLen {
		return "", errors.New("backup: envelope is closed")
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// resticLeg initializes the repository if needed, backs up the staging dir,
// applies retention, and writes keyring.age plus RECOVERY.md at the target
// root — outside the repository, because the repository password is derived
// from the key that keyring wraps.
func (e *Engine) resticLeg(ctx context.Context, t Target, stage string) (_ string, _ int64, err error) {
	// An s3 repository uri can carry its own key and secret, and both this
	// package and restic quote the uri back in failure messages. red covers
	// what the uri cannot: the repository password and the resolved storage
	// credentials, which restic echoes back on its own.
	var red *secrets.Redactor
	defer func() { err = scrubbed(redacted(red, err)) }()

	password, err := e.resticPassword(t.Name)
	if err != nil {
		return "", 0, err
	}
	repo, err := resticRepo(t)
	if err != nil {
		return "", 0, err
	}
	keep, err := parseRetention(t.Retention)
	if err != nil {
		return "", 0, err
	}

	if t.Kind == KindResticLocal {
		if err := preflightLocal(repo); err != nil {
			return "", 0, err
		}
	}
	if t.Kind == KindResticS3 {
		resolved, warned, err := resolveS3Env(ctx, e.store, e.env, t.Name, t.Env)
		if err != nil {
			return "", 0, err
		}
		if warned {
			e.opts.Logger.Warn("s3 target has no sealed credentials; falling back to the process environment",
				"target", t.Name)
		}
		t.Env = resolved
	}

	passFile, cleanup, err := writePasswordFile(e.opts.StageRoot, password)
	if err != nil {
		return "", 0, err
	}
	defer cleanup()

	red = secrets.NewRedactor(legSecrets(password, t.Env))
	env := t.Env
	run := func(timeout time.Duration, args ...string) (toolResult, error) {
		return e.runTool(ctx, executor.Spec{
			Kind:    "shell",
			Argv:    append([]string{"restic", "-r", repo}, args...),
			Dir:     stage,
			Env:     resticEnv(env, passFile),
			Timeout: timeout,
		}, red)
	}

	probe, err := run(e.probeTimeout(), "cat", "config")
	if err != nil {
		return "", 0, err
	}
	if probe.exitCode != 0 {
		init, err := run(e.probeTimeout(), "init")
		if err != nil {
			return "", 0, err
		}
		if init.exitCode != 0 {
			return "", 0, fmt.Errorf("restic init exited %d: %s", init.exitCode, init.tail(5))
		}
	}

	// No timeout past this point: how long a backup takes is the data's
	// business, not a reachability question.
	backup, err := run(0, "backup", "--json", "--tag", "exocomp", "--tag", t.Name, ".")
	if err != nil {
		return "", 0, err
	}
	if backup.exitCode != 0 {
		return "", 0, fmt.Errorf("restic backup exited %d: %s", backup.exitCode, backup.tail(5))
	}
	summary := parseResticSummary(backup.stdout)

	if len(keep) > 0 {
		forget, err := run(0, append(append([]string{"forget"}, keep...), "--prune")...)
		if err != nil {
			return "", 0, err
		}
		if forget.exitCode != 0 {
			return summary.SnapshotID, summary.DataAdded,
				fmt.Errorf("restic forget exited %d: %s", forget.exitCode, forget.tail(5))
		}
	}

	if err := e.writeRootFiles(ctx, t, stage); err != nil {
		return summary.SnapshotID, summary.DataAdded, err
	}
	return summary.SnapshotID, summary.DataAdded, nil
}

// secretEnvKeys names the target environment entries whose values are secret.
// The region and the cache dir are not.
var secretEnvKeys = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
}

// legSecrets is everything a restic leg must never write down: the repository
// password and the storage credentials resolved for this target. They are
// already in hand when the leg starts, which is the only moment anything can
// scrub them out of what restic prints.
func legSecrets(password string, env map[string]string) [][]byte {
	known := [][]byte{[]byte(password)}
	for _, k := range secretEnvKeys {
		if v := env[k]; v != "" {
			known = append(known, []byte(v))
		}
	}
	return known
}

// redacted rewrites an error's text with every known secret replaced. Like
// scrubbed, the original is dropped rather than wrapped: unwrapping a wrapper
// would hand the unredacted text straight back.
func redacted(red *secrets.Redactor, err error) error {
	if err == nil || red == nil {
		return err
	}
	clean := string(red.Redact([]byte(err.Error())))
	if clean == err.Error() {
		return err
	}
	return errors.New(clean)
}

// writeRootFiles puts keyring.age and RECOVERY.md at the target root, next to
// the repository rather than inside it.
func (e *Engine) writeRootFiles(ctx context.Context, t Target, stage string) error {
	keyring, err := os.ReadFile(filepath.Join(stage, KeyringName))
	if err != nil {
		return fmt.Errorf("backup: read staged keyring: %w", err)
	}
	recovery := renderRecovery(t, e.env.Recipients(), e.opts.Now().UTC())

	switch t.Kind {
	case KindResticLocal:
		root := localRepoPath(t.URI)
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("backup: target root: %w", err)
		}
		if err := os.WriteFile(filepath.Join(root, KeyringName), keyring, 0o644); err != nil {
			return fmt.Errorf("backup: write target keyring: %w", err)
		}
		if err := os.WriteFile(filepath.Join(root, RecoveryName), recovery, 0o644); err != nil {
			return fmt.Errorf("backup: write target recovery notes: %w", err)
		}
		return nil
	case KindResticS3:
		client, err := newS3Client(t.URI, t.Env)
		if err != nil {
			return err
		}
		if err := client.put(ctx, KeyringName, keyring, "application/octet-stream"); err != nil {
			return err
		}
		return client.put(ctx, RecoveryName, recovery, "text/markdown")
	default:
		return fmt.Errorf("backup: target kind %q has no root", t.Kind)
	}
}

// resticEnv points restic at a password file. The password itself never enters
// the child's environment: /proc/<pid>/environ is readable by every process of
// this uid for the whole life of the leg, and a backup is unbounded in time.
// A 0600 file bounds the same exposure to the filesystem.
func resticEnv(extra map[string]string, passwordFile string) []string {
	env := map[string]string{"RESTIC_PASSWORD_FILE": passwordFile}
	for k, v := range extra {
		env[k] = v
	}
	return toolEnv(env)
}

// writePasswordFile puts password in a 0600 file inside a fresh 0700 directory
// under root, and returns the path with the function that removes both.
//
// The directory is deliberately not the backup staging dir: the leg runs
// `restic backup .` there and the git leg tars it, so a credential dropped in
// it would be archived into the snapshot it protects.
func writePasswordFile(root, password string) (string, func(), error) {
	dir, err := os.MkdirTemp(root, "exocomp-cred-")
	if err != nil {
		return "", nil, fmt.Errorf("backup: create credential dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	// MkdirTemp already creates 0700; say so explicitly, because everything
	// below depends on it.
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("backup: credential dir mode: %w", err)
	}
	path := filepath.Join(dir, "restic.pass")
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("backup: write repository password: %w", err)
	}
	return path, cleanup, nil
}

// preflightLocal asserts the repository directory exists and is a directory
// before restic touches it. An unmounted mount path or a file in the way must
// fail the leg now: restic's own backoff would burn the backup window first
// and fail anyway.
func preflightLocal(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("backup: target root %s: %w", root, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("backup: target root %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup: target root %s is not a directory", root)
	}
	return nil
}

func resticRepo(t Target) (string, error) {
	switch t.Kind {
	case KindResticLocal:
		path := localRepoPath(t.URI)
		if path == "" {
			return "", errors.New("backup: restic_local target has an empty uri")
		}
		return path, nil
	case KindResticS3:
		if !strings.HasPrefix(t.URI, "s3:") {
			return "", fmt.Errorf("backup: restic_s3 uri %q is missing the s3 scheme prefix", t.URI)
		}
		return t.URI, nil
	default:
		return "", fmt.Errorf("backup: kind %q is not a restic target", t.Kind)
	}
}

// localRepoPath accepts both a bare path and restic's explicit "local:" form.
func localRepoPath(uri string) string {
	return strings.TrimPrefix(uri, "local:")
}

// parseRetention turns a target's retention config ("daily=7,weekly=4") into
// restic forget flags. Empty means the default policy; "none" disables
// pruning explicitly.
func parseRetention(spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "":
		return defaultRetention, nil
	case "none":
		return nil, nil
	}

	var flags []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("backup: retention %q: want key=value", part)
		}
		flag, known := retentionKeys[strings.ToLower(strings.TrimSpace(key))]
		if !known {
			return nil, fmt.Errorf("backup: retention key %q is not one of last/hourly/daily/weekly/monthly/yearly", key)
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("backup: retention %q: want a non-negative count", part)
		}
		flags = append(flags, flag, strconv.Itoa(n))
	}
	if len(flags) == 0 {
		return nil, fmt.Errorf("backup: retention %q sets no policy", spec)
	}
	return flags, nil
}

func parseResticSummary(lines []string) resticSummary {
	var out resticSummary
	for _, line := range lines {
		var s resticSummary
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue
		}
		if s.MessageType == "summary" {
			out = s
		}
	}
	return out
}
